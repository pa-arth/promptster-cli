package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claude Code BYO-subscription capture works by tailing the per-session
// transcript JSONL that Claude Code writes to
// ~/.claude/projects/<munged-cwd>/<session-uuid>.jsonl. The transcript is the
// authoritative record — full assistant messages, tool calls + results, and
// (critically for BYO mode, where no traffic crosses the Promptster proxy)
// per-API-request token usage with the exact model, which the worker prices
// into an ESTIMATED cost_efficiency_v1.
//
// Verified line shapes (Claude Code 2.1.x):
//
//	{"type":"user","message":{"role":"user","content":"..."},
//	 "uuid","parentUuid","timestamp","cwd","sessionId","isMeta?","isSidechain?",
//	 "promptId","promptSource","permissionMode",...}
//	{"type":"assistant","message":{"id","model","content":[{type:text|thinking|tool_use}],
//	 "usage":{input_tokens,cache_creation_input_tokens,cache_read_input_tokens,
//	          output_tokens,cache_creation:{ephemeral_5m_input_tokens,ephemeral_1h_input_tokens}}},
//	 "requestId","timestamp",...}
//	{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id",...}]},
//	 "toolUseResult": <the SAME structured object the PostToolUse hook receives>}
//
// plus non-message line types (mode, permission-mode, worktree-state,
// file-history-snapshot, attachment, last-prompt, ai-title, system, ...) that
// carry no candidate-visible signal but DO serve as assistant-message flush
// boundaries.
//
// One API response is split across MULTIPLE assistant lines (one per content
// block), all sharing message.id and carrying identical usage — so usage is
// deduped by message.id and text blocks are accumulated until a boundary.

// claudePendingTool holds a tool_use block awaiting its tool_result line so the
// pair can be fed through normalizePostToolUseByTool — the SAME function the
// hook path uses, which keeps event shapes identical across capture channels.
type claudePendingTool struct {
	name  string
	input map[string]interface{}
}

// claudeMsgAccum accumulates one assistant API message (one message.id) across
// its per-content-block transcript lines.
type claudeMsgAccum struct {
	msgID     string
	ts        string
	model     string
	requestID string
	text      strings.Builder
	usage     map[string]interface{}
	updatedAt time.Time // wall clock of last appended line, for stale flush
}

// claudeTranscriptProcessor converts Claude Code transcript JSONL lines into
// canonical Events. Stateful per transcript file: tool_use blocks are
// correlated with their tool_result lines by id, and assistant text/usage is
// accumulated per message.id.
type claudeTranscriptProcessor struct {
	sessionID          string
	consentToIntegrity bool
	// usageOnly puts the processor in sidechain mode (subagents/agent-*.jsonl
	// files): every line is agent-authored, so only per-request token usage is
	// extracted — prompts, responses, and tool events must never enter the
	// candidate's timeline from a sidechain.
	usageOnly     bool
	pendingTools  map[string]claudePendingTool
	emittedMsgIDs map[string]bool
	accum         *claudeMsgAccum
	// lastPromptTs is the TRANSCRIPT timestamp of the previous human prompt —
	// used for cadence enrichment. The hook path uses wall clock at submit
	// time; here the watcher observes lines up to a poll later, so deltas must
	// come from line timestamps, not time.Now().
	lastPromptTs time.Time
	// Lane identity of this transcript: one file IS one Claude Code process
	// (the filename is the session uuid), so the first record carrying
	// sessionId/cwd pins both for every event the file produces. Distinct
	// lanes = parallel sessions; the worker's parallelism signals key on
	// meta.ideSessionId / meta.cwd.
	ideSessionID string
	laneCwd      string
	// Interrupt tracking. Claude Code writes a synthetic user line
	// ([Request interrupted by user] / ...for tool use) when the candidate hits
	// ESC/Ctrl+C mid-response. We classify what was cut POSITIONALLY from the
	// most-recent assistant record — interruptedMessageId is null ~1/3 of the
	// time, so it can't be relied on. lastAssistantHadTool/lastCutTool* describe
	// that record; lastAssistantMsgID detects message-boundary transitions.
	lastAssistantMsgID   string
	lastAssistantHadTool bool
	lastCutToolName      string
	lastCutToolInput     map[string]interface{}
	// pendingInterruptID is the ID of an emitted interrupt awaiting the redirect
	// prompt that follows it. The next prompt (with no assistant record between)
	// back-links to it; an intervening assistant record or a second interrupt
	// clears it.
	pendingInterruptID string
}

func newClaudeTranscriptProcessor(sessionID string, consentToIntegrity bool) *claudeTranscriptProcessor {
	return &claudeTranscriptProcessor{
		sessionID:          sessionID,
		consentToIntegrity: consentToIntegrity,
		pendingTools:       map[string]claudePendingTool{},
		emittedMsgIDs:      map[string]bool{},
	}
}

// transcriptHumanProvenance / transcriptAiProvenance mirror the hook
// equivalents but record the capture method, so the worker can tell which
// channel observed an event.
func transcriptHumanProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_human",
		Confidence:    0.8,
		Observability: "medium",
		Methods:       []string{"transcript-jsonl"},
	}
}

func transcriptAiProvenance() *Provenance {
	return &Provenance{
		Attribution:   "likely_ai",
		Confidence:    0.9,
		Observability: "high",
		Methods:       []string{"transcript-jsonl"},
	}
}

func (p *claudeTranscriptProcessor) newTranscriptEvent(kind, ts string) Event {
	e := newEvent(kind, p.sessionID)
	e.Source = "claude-code"
	switch kind {
	case "prompt":
		e.Actor = humanActor()
		e.Provenance = transcriptHumanProvenance()
	default:
		e.Actor = aiActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// process parses one transcript line and returns zero or more canonical events.
// attachLane stamps the transcript's lane identity (meta.ideSessionId /
// meta.cwd) onto every outgoing event whose data is a map, never overwriting
// keys a normalizer already set.
func (p *claudeTranscriptProcessor) attachLane(events []Event) []Event {
	if p.ideSessionID == "" && p.laneCwd == "" {
		return events
	}
	for i := range events {
		data, ok := events[i].Data.(map[string]interface{})
		if !ok || data == nil {
			continue
		}
		meta, _ := data["meta"].(map[string]interface{})
		if meta == nil {
			meta = map[string]interface{}{}
		}
		if p.ideSessionID != "" {
			if _, exists := meta["ideSessionId"]; !exists {
				meta["ideSessionId"] = p.ideSessionID
			}
		}
		if p.laneCwd != "" {
			if _, exists := meta["cwd"]; !exists {
				meta["cwd"] = p.laneCwd
			}
		}
		data["meta"] = meta
	}
	return events
}

func (p *claudeTranscriptProcessor) process(line []byte) []Event {
	return p.attachLane(p.processLine(line))
}

func (p *claudeTranscriptProcessor) processLine(line []byte) []Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	if p.ideSessionID == "" {
		p.ideSessionID = stringField(rec, "sessionId")
	}
	if p.laneCwd == "" {
		p.laneCwd = stringField(rec, "cwd")
	}
	// Sidechain lines are subagent traffic: their "user" prompts are authored
	// by the AGENT, not the candidate, so they must never become prompt events
	// or pollute the main-context token series. Their token usage is real
	// spend though — extract it as subagent_usage so estimated cost doesn't
	// undercount subagent-heavy sessions. (Subagent transcripts live in
	// <session>/subagents/agent-*.jsonl, which the watcher tails in usageOnly
	// mode — the inline check is belt-and-braces for older layouts.)
	sidechain, _ := rec["isSidechain"].(bool)
	if p.usageOnly || sidechain {
		return p.sidechainUsage(rec)
	}

	typ, _ := rec["type"].(string)
	switch typ {
	case "assistant":
		return p.handleAssistant(rec, line)
	case "user":
		// A user line ends the in-flight assistant message (the API response
		// is complete before tool results / next prompts get written).
		events := p.flushAccum()
		return append(events, p.handleUser(rec, line)...)
	default:
		// mode / worktree-state / file-history-snapshot / system / ... carry
		// no signal of their own but are reliable flush boundaries.
		return p.flushAccum()
	}
}

// sidechainUsage extracts ONLY token usage from a sidechain (subagent) line:
// one subagent_usage event per assistant message.id, carrying the request's
// usage + model and nothing else. No accumulation is needed — every line of a
// message repeats the same usage.
func (p *claudeTranscriptProcessor) sidechainUsage(rec map[string]interface{}) []Event {
	typ, _ := rec["type"].(string)
	if typ != "assistant" {
		return nil
	}
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}
	msgID := stringField(msg, "id")
	if msgID == "" || p.emittedMsgIDs[msgID] {
		return nil
	}
	usage, _ := msg["usage"].(map[string]interface{})
	if usage == nil {
		return nil
	}
	p.emittedMsgIDs[msgID] = true

	ts, _ := rec["timestamp"].(string)
	e := p.newTranscriptEvent("subagent_usage", ts)
	e.Provenance = transcriptAiProvenance()
	data := map[string]interface{}{
		"usageScope":       "request",
		"sidechain":        true,
		"inputTokens":      intField(usage, "input_tokens"),
		"outputTokens":     intField(usage, "output_tokens"),
		"cacheReadTokens":  intField(usage, "cache_read_input_tokens"),
		"cacheWriteTokens": intField(usage, "cache_creation_input_tokens"),
	}
	if model := stringField(msg, "model"); model != "" {
		data["model"] = model
	}
	if reqID := stringField(rec, "requestId"); reqID != "" {
		data["requestId"] = reqID
	}
	if cc, ok := usage["cache_creation"].(map[string]interface{}); ok {
		data["cacheWrite5mTokens"] = intField(cc, "ephemeral_5m_input_tokens")
		data["cacheWrite1hTokens"] = intField(cc, "ephemeral_1h_input_tokens")
	}
	e.Data = data
	e.RawPayload = strPreview(fmt.Sprintf("subagent usage msg=%s", msgID), 100)
	return []Event{e}
}

func (p *claudeTranscriptProcessor) handleAssistant(rec map[string]interface{}, line []byte) []Event {
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return p.flushAccum()
	}
	msgID := stringField(msg, "id")
	ts, _ := rec["timestamp"].(string)

	// A new assistant message (re)sets the positional interrupt context: it is
	// now the most-recent assistant record, and it means any pending interrupt
	// was an abort with no redirect prompt — clear the pending back-link.
	if msgID != "" && msgID != p.lastAssistantMsgID {
		p.pendingInterruptID = ""
		p.lastAssistantMsgID = msgID
		p.lastAssistantHadTool = false
		p.lastCutToolName = ""
		p.lastCutToolInput = nil
	}

	var events []Event
	if p.accum != nil && p.accum.msgID != msgID {
		events = p.flushAccum()
	}
	// Start an accumulator on the first line of a not-yet-emitted message.
	if p.accum == nil && msgID != "" && !p.emittedMsgIDs[msgID] {
		p.accum = &claudeMsgAccum{
			msgID:     msgID,
			ts:        ts,
			model:     stringField(msg, "model"),
			requestID: stringField(rec, "requestId"),
		}
		if u, ok := msg["usage"].(map[string]interface{}); ok {
			p.accum.usage = u
		}
	}
	if p.accum != nil && p.accum.msgID == msgID {
		p.accum.updatedAt = time.Now()
	}

	content, _ := msg["content"].([]interface{})
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]interface{})
		if block == nil {
			continue
		}
		switch stringField(block, "type") {
		case "text":
			if p.accum != nil && p.accum.msgID == msgID {
				if p.accum.text.Len() > 0 {
					p.accum.text.WriteString("\n")
				}
				p.accum.text.WriteString(stringField(block, "text"))
			}
		case "tool_use":
			// Register even when the message was already flushed — the
			// tool_result may arrive on a later poll.
			id := stringField(block, "id")
			input, _ := block["input"].(map[string]interface{})
			if input == nil {
				input = map[string]interface{}{}
			}
			if id != "" {
				p.pendingTools[id] = claudePendingTool{name: stringField(block, "name"), input: input}
			}
			// Positional interrupt context: this message contains a tool call, so
			// an interrupt line arriving next cut an ACTION. Record the tool so
			// the interrupt event can name what was killed.
			p.lastAssistantHadTool = true
			p.lastCutToolName = stringField(block, "name")
			p.lastCutToolInput = input
		}
		// thinking / other block types carry no event of their own.
	}
	return events
}

// flushAccum emits the accumulated assistant message as ONE ai_response event
// carrying the request's token usage and model — the per-request usage series
// the worker needs for context-trend signals and estimated pricing.
func (p *claudeTranscriptProcessor) flushAccum() []Event {
	if p.accum == nil {
		return nil
	}
	a := p.accum
	p.accum = nil
	if a.msgID == "" || p.emittedMsgIDs[a.msgID] {
		return nil
	}
	p.emittedMsgIDs[a.msgID] = true

	e := p.newTranscriptEvent("ai_response", a.ts)
	e.Provenance = transcriptAiProvenance()
	data := map[string]interface{}{
		"lastAssistantMessage": a.text.String(),
		"usageScope":           "request",
	}
	if a.model != "" {
		data["model"] = a.model
	}
	if a.requestID != "" {
		data["requestId"] = a.requestID
	}
	if u := a.usage; u != nil {
		data["inputTokens"] = intField(u, "input_tokens")
		data["outputTokens"] = intField(u, "output_tokens")
		data["cacheReadTokens"] = intField(u, "cache_read_input_tokens")
		data["cacheWriteTokens"] = intField(u, "cache_creation_input_tokens")
		// 5m vs 1h cache writes price differently (1.25x vs 2x input) — pass
		// the split so the worker's estimate doesn't systematically undercount
		// (Claude Code defaults to the 1h tier).
		if cc, ok := u["cache_creation"].(map[string]interface{}); ok {
			data["cacheWrite5mTokens"] = intField(cc, "ephemeral_5m_input_tokens")
			data["cacheWrite1hTokens"] = intField(cc, "ephemeral_1h_input_tokens")
		}
	}
	e.Data = data
	e.RawPayload = strPreview(a.text.String(), 500)
	return []Event{e}
}

// flushStale force-flushes an accumulated assistant message that has not seen
// a new line in maxAge — covers the final message of a turn when Claude Code
// writes no further boundary line for a while.
func (p *claudeTranscriptProcessor) flushStale(maxAge time.Duration) []Event {
	if p.accum == nil || time.Since(p.accum.updatedAt) < maxAge {
		return nil
	}
	return p.attachLane(p.flushAccum())
}

func (p *claudeTranscriptProcessor) handleUser(rec map[string]interface{}, line []byte) []Event {
	msg, _ := rec["message"].(map[string]interface{})
	if msg == nil {
		return nil
	}
	// Meta lines (local-command caveats, command wrappers) and compact
	// summaries are Claude-Code-internal, not candidate prompts.
	if isMeta, _ := rec["isMeta"].(bool); isMeta {
		return nil
	}
	if isCompact, _ := rec["isCompactSummary"].(bool); isCompact {
		return nil
	}
	ts, _ := rec["timestamp"].(string)

	switch content := msg["content"].(type) {
	case string:
		// An ESC/Ctrl+C interrupt arrives here as a plain-string user message —
		// intercept it before it becomes a spurious prompt.
		if variant, ok := interruptVariant(content); ok {
			return p.interruptEvent(variant, ts, line)
		}
		return p.promptEvent(content, rec, ts, line)
	case []interface{}:
		var events []Event
		var textParts []string
		hasToolResult := false
		interruptVar := ""
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]interface{})
			if block == nil {
				continue
			}
			switch stringField(block, "type") {
			case "tool_result":
				// The "...for tool use" sentinel can land inside a tool_result
				// block when a tool call was cut — catch it before pairing.
				if variant, ok := interruptVariant(toolResultText(block)); ok {
					interruptVar = variant
					continue
				}
				hasToolResult = true
				if ev, ok := p.resolveToolResult(rec, block, ts, line); ok {
					events = append(events, ev)
				}
			case "text":
				if variant, ok := interruptVariant(stringField(block, "text")); ok {
					interruptVar = variant
					continue
				}
				textParts = append(textParts, stringField(block, "text"))
			}
		}
		if interruptVar != "" {
			events = append(events, p.interruptEvent(interruptVar, ts, line)...)
		}
		// A content array with text blocks and no tool_result is a human
		// prompt (e.g. prompt with attachments).
		if !hasToolResult && len(textParts) > 0 {
			events = append(events, p.promptEvent(strings.Join(textParts, "\n"), rec, ts, line)...)
		}
		return events
	default:
		return nil
	}
}

// interruptSentinels maps the exact synthetic user text Claude Code writes on an
// ESC/Ctrl+C interrupt to the variant recorded on the interrupt event. Match is
// on the trimmed, whole string only.
var interruptSentinels = map[string]string{
	"[Request interrupted by user]":              "generation",
	"[Request interrupted by user for tool use]": "tool_use",
}

// interruptVariant reports whether text is an interrupt sentinel and, if so, the
// variant ("generation" | "tool_use") to stamp on the event.
func interruptVariant(text string) (string, bool) {
	v, ok := interruptSentinels[strings.TrimSpace(text)]
	return v, ok
}

// toolResultText returns the string content of a tool_result block, or "" when
// the content is not a plain string.
func toolResultText(block map[string]interface{}) string {
	if s, ok := block["content"].(string); ok {
		return s
	}
	return ""
}

// interruptEvent emits an `interrupt` event describing what the candidate cut
// mid-response. subtype is classified positionally from the most-recent
// assistant record: "action" if it contained a tool_use block (with the cut
// tool + a redacted, truncated input preview), else "generation". variant
// records which sentinel matched. Consecutive interrupts (ESC ESC) collapse: a
// second interrupt while one is still pending is skipped so a burst counts once.
func (p *claudeTranscriptProcessor) interruptEvent(variant, ts string, line []byte) []Event {
	if p.pendingInterruptID != "" {
		return nil
	}
	subtype := "generation"
	data := map[string]interface{}{
		"variant": variant,
	}
	if p.lastAssistantHadTool {
		subtype = "action"
		if p.lastCutToolName != "" {
			data["cutTool"] = p.lastCutToolName
		}
		// The input came off an already-redacted transcript line (redactBytes in
		// cmd_claude_watch.go runs before process()), so this only truncates.
		if preview := cutToolInputPreview(p.lastCutToolInput); preview != "" {
			data["cutToolInput"] = preview
		}
	}
	data["subtype"] = subtype

	e := p.newTranscriptEvent("interrupt", ts)
	e.Actor = humanActor()
	e.Provenance = transcriptHumanProvenance()
	e.Data = data
	e.RawPayload = strPreview(string(line), 500)
	p.pendingInterruptID = e.ID
	return []Event{e}
}

// cutToolInputPreview pulls the most salient string from a cut tool's input —
// the shell command or the target file path — truncated to 200 chars. Returns
// "" when the input carries none.
func cutToolInputPreview(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path"} {
		if s, ok := input[key].(string); ok && s != "" {
			return strPreview(s, 200)
		}
	}
	return ""
}

func (p *claudeTranscriptProcessor) promptEvent(text string, rec map[string]interface{}, ts string, line []byte) []Event {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// Slash-command envelopes are candidate-typed skill/command invocations:
	// surface them as tool_intent (the ecosystem_leverage signal) instead of
	// dropping them, so pure-transcript capture matches hook capture. Local
	// command output stays dropped — it's output, not an invocation.
	if strings.HasPrefix(trimmed, "<command-") {
		return p.slashCommandEvent(trimmed, ts, line)
	}
	if strings.HasPrefix(trimmed, "<local-command") {
		return nil
	}

	e := p.newTranscriptEvent("prompt", ts)
	data := map[string]interface{}{"text": text}

	meta := map[string]interface{}{}
	if v := stringField(rec, "sessionId"); v != "" {
		meta["ideSessionId"] = v
	}
	if v := stringField(rec, "permissionMode"); v != "" {
		meta["permissionMode"] = v
	}
	if v := stringField(rec, "promptSource"); v != "" {
		meta["promptSource"] = v
	}
	if v := stringField(rec, "promptId"); v != "" {
		meta["promptId"] = v
	}
	if v := stringField(rec, "cwd"); v != "" {
		meta["cwd"] = v
	}
	if len(meta) > 0 {
		data["meta"] = meta
	}

	// Cadence enrichment (opt-in): deltas from TRANSCRIPT timestamps, since
	// the watcher observes lines after the fact.
	lineTime, err := time.Parse(time.RFC3339, ts)
	if p.consentToIntegrity {
		data["promptLengthChars"] = len(text)
		if err == nil && !p.lastPromptTs.IsZero() {
			deltaMs := lineTime.Sub(p.lastPromptTs).Milliseconds()
			data["timeSinceLastPromptMs"] = deltaMs
			if len(text) > 300 && deltaMs < 1000 {
				data["likelyPaste"] = true
			}
		}
	}
	if err == nil {
		p.lastPromptTs = lineTime
	}

	// This is the redirect prompt following an interrupt (no assistant record
	// intervened, or it would have cleared pendingInterruptID) — back-link it.
	if p.pendingInterruptID != "" {
		e.RelatedEventIDs = append(e.RelatedEventIDs, p.pendingInterruptID)
		data["followsInterrupt"] = true
		p.pendingInterruptID = ""
	}

	e.Data = data
	e.RawPayload = strPreview(string(line), 500)
	return []Event{e}
}

// slashCommandBuiltins are Claude Code built-ins with dedicated semantics
// (context resets, app chrome, account plumbing). They aren't ecosystem
// leverage and would otherwise inflate skill-invocation counts.
var slashCommandBuiltins = map[string]bool{
	"clear": true, "compact": true, "exit": true, "quit": true,
	"login": true, "logout": true, "help": true, "doctor": true,
	"status": true, "cost": true, "config": true, "model": true,
	"memory": true, "init": true, "resume": true, "context": true,
	"todos": true, "hooks": true, "permissions": true, "bug": true,
	"release-notes": true, "terminal-setup": true, "vim": true,
	"upgrade": true, "mcp": true, "agents": true, "ide": true,
	"add-dir": true, "export": true, "rewind": true, "usage": true,
}

// slashCommandEvent converts a typed slash-command envelope
// (<command-name>/foo</command-name> ... <command-args>bar</command-args>)
// into a tool_intent event shaped like the PreToolUse hook's SlashCommand
// intent, so the worker's ecosystem harvest reads both capture paths the
// same way.
func (p *claudeTranscriptProcessor) slashCommandEvent(text, ts string, line []byte) []Event {
	name := extractTagContent(text, "command-name")
	if name == "" {
		return nil
	}
	if slashCommandBuiltins[strings.TrimPrefix(name, "/")] {
		return nil
	}
	preview := name
	if args := extractTagContent(text, "command-args"); args != "" {
		preview += " " + args
	}

	e := p.newTranscriptEvent("tool_intent", ts)
	e.Actor = humanActor()
	e.Provenance = transcriptHumanProvenance()
	e.Data = map[string]interface{}{
		"toolName":     "SlashCommand",
		"inputPreview": strPreview(preview, 100),
	}
	e.RawPayload = strPreview(string(line), 500)
	return []Event{e}
}

// extractTagContent returns the trimmed text between <tag> and </tag>, or ""
// when the tag is absent or unterminated.
func extractTagContent(text, tag string) string {
	open := "<" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], "</"+tag+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

// resolveToolResult pairs a tool_result block with its registered tool_use and
// feeds both through normalizePostToolUseByTool. The outer toolUseResult field
// carries the same structured response the PostToolUse hook receives
// (structuredPatch for edits, stdout/stderr for Bash, file for Read), so the
// resulting events are shape-identical to hook-captured ones.
func (p *claudeTranscriptProcessor) resolveToolResult(rec map[string]interface{}, block map[string]interface{}, ts string, line []byte) (Event, bool) {
	id := stringField(block, "tool_use_id")
	call, ok := p.pendingTools[id]
	if !ok {
		return Event{}, false
	}
	delete(p.pendingTools, id)

	isErr, _ := block["is_error"].(bool)
	toolResponse := map[string]interface{}{}
	switch tur := rec["toolUseResult"].(type) {
	case map[string]interface{}:
		toolResponse = tur
	case string:
		toolResponse["content"] = tur
		if isErr {
			toolResponse["error"] = tur
		}
	}
	// Transcript Bash results carry no structured exit_code. Failed runs DO
	// embed it in the result text ("Error: Exit code 2"), so parse that —
	// verification signals key off the real code — falling back to 1. Only
	// error results are scanned: a successful command's stdout may legitimately
	// TALK about exit codes, and success means 0 by definition (Claude Code
	// flags any nonzero exit as is_error).
	if _, has := toolResponse["exit_code"]; !has && isErr {
		blockText, _ := block["content"].(string)
		if code, ok := parseClaudeExitCode(toolResponse, blockText); ok {
			toolResponse["exit_code"] = float64(code)
		} else {
			toolResponse["exit_code"] = float64(1)
		}
	}

	ev, ok := normalizePostToolUseByTool(call.name, call.input, toolResponse, p.sessionID, strPreview(string(line), 500))
	if !ok {
		return Event{}, false
	}
	ev.Source = "claude-code"
	if t := parseCodexTs(ts); t != "" {
		ev.Ts = t
	}
	if ev.Actor == nil {
		ev.Actor = aiActor()
	}
	// Stamp the capture method while preserving the attribution the
	// normalizer chose (plan_decision is likely_human, edits likely_ai).
	if ev.Provenance != nil {
		ev.Provenance.Methods = []string{"transcript-jsonl"}
	}
	return ev, true
}

// parseClaudeExitCode scans the textual fields of a transcript tool result for
// an embedded exit code ("Error: Exit code 2", "exited with code 130", ...).
// Reuses the codex exit-code pattern — both tools phrase it the same way.
func parseClaudeExitCode(toolResponse map[string]interface{}, extra string) (int, bool) {
	candidates := []string{extra}
	for _, key := range []string{"content", "error", "stderr", "stdout"} {
		if s, ok := toolResponse[key].(string); ok && s != "" {
			candidates = append(candidates, s)
		}
	}
	for _, s := range candidates {
		if m := codexExitCodeRe.FindStringSubmatch(s); m != nil {
			code := 0
			if _, err := fmt.Sscanf(m[1], "%d", &code); err == nil {
				return code, true
			}
		}
	}
	return 0, false
}
