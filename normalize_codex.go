package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Codex instrumentation works by tailing the per-session rollout JSONL that the
// `codex` CLI writes to ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl. Unlike
// Claude Code / Cursor, the codex hooks engine does NOT fire for `codex exec`
// (and is interactive-TUI-gated besides), so the rollout file is the reliable
// capture channel — it is written in every mode and carries prompts, tool
// calls, command output, file patches, assistant messages and token usage.
//
// Each rollout line is one RolloutItem:
//
//	{"timestamp":"...","type":"session_meta|event_msg|response_item|turn_context","payload":{...}}
//
// The payload's own "type" discriminates further (user_message, agent_message,
// function_call, custom_tool_call, patch_apply_end, token_count, ...).

// codexPendingCall holds a tool call awaiting its output line so the two can be
// merged into a single canonical event (mirrors how Claude's PostToolUse
// carries both input and response).
type codexPendingCall struct {
	name string
	args map[string]interface{}
}

// codexRolloutProcessor converts codex rollout JSONL lines into canonical
// Events. It is stateful: function-call lines are correlated with their
// *_output lines by call_id, and the latest token usage is attached to the next
// final assistant message.
type codexRolloutProcessor struct {
	sessionID          string
	consentToIntegrity bool
	pending            map[string]codexPendingCall
	// running holds shell calls the host DETACHED: exec_command answers with
	// "Script running with cell ID N" instead of output, and the real result
	// arrives later via one or more write_stdin calls. Keyed by cell ID. Without
	// this, the handoff line parses as exitCode=0 and every long-running command
	// is reported green regardless of how it actually ended.
	running map[string]codexPendingCall
	// sawFileChange records whether a FileChange item arrived since the current
	// apply_patch call. Ordering in the rollout is strictly
	// CALL -> FILECHANGE -> OUTPUT, and a REJECTED patch produces no FileChange
	// at all, so this is an exact signal rather than a heuristic: it decides
	// whether the authoritative record already covered this patch, or whether the
	// envelope has to stand in for a host that does not emit FileChange.
	sawFileChange  bool
	lastTokenUsage map[string]interface{}
	// pendingInterruptID is the ID of an emitted interrupt still waiting for the
	// prompt that follows it, so that prompt can be flagged `followsInterrupt`
	// and back-linked. Mirrors claudeTranscriptProcessor. Without the flag the
	// backend cannot separate a redirect (cut the run and steered it) from an
	// abort (cut it and walked away), and every codex interrupt lands as
	// neither.
	pendingInterruptID string
}

func newCodexRolloutProcessor(sessionID string, consentToIntegrity bool) *codexRolloutProcessor {
	return &codexRolloutProcessor{
		sessionID:          sessionID,
		consentToIntegrity: consentToIntegrity,
		pending:            map[string]codexPendingCall{},
		running:            map[string]codexPendingCall{},
	}
}

// newCodexEvent builds a canonical Event stamped with the rollout line's own
// timestamp (so the replay timeline reflects when things actually happened, not
// when the watcher observed them) and source="codex". Actor is derived from
// kind: prompts are the candidate, session lifecycle is the system, and every
// tool/output event is the agent acting.
func (p *codexRolloutProcessor) newCodexEvent(kind, ts string) Event {
	e := newEvent(kind, p.sessionID)
	e.Source = "codex"
	switch kind {
	case "prompt", "interrupt":
		e.Actor = humanActor()
	case "session_start", "session_end":
		e.Actor = systemActor()
	default:
		e.Actor = aiActor()
	}
	if t := parseCodexTs(ts); t != "" {
		e.Ts = t
	}
	return e
}

// process parses one rollout line and returns zero or more canonical events.
func (p *codexRolloutProcessor) process(line []byte) []Event {
	var rec map[string]interface{}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	typ, _ := rec["type"].(string)
	payload, _ := rec["payload"].(map[string]interface{})
	if payload == nil {
		return nil
	}
	ts, _ := rec["timestamp"].(string)
	raw := strPreview(string(line), 500)

	switch typ {
	case "session_meta":
		return p.sessionMeta(payload, ts, raw)
	case "event_msg":
		return p.eventMsg(payload, ts, raw)
	case "response_item":
		return p.responseItem(payload, ts, raw)
	default:
		// turn_context and unknown wrappers carry no candidate-visible signal.
		return nil
	}
}

func (p *codexRolloutProcessor) sessionMeta(payload map[string]interface{}, ts, raw string) []Event {
	e := p.newCodexEvent("session_start", ts)
	data := map[string]interface{}{
		"ideSessionId": stringField(payload, "id"),
		"cwd":          stringField(payload, "cwd"),
		"source":       stringField(payload, "originator"),
		"cliVersion":   stringField(payload, "cli_version"),
		"model":        stringField(payload, "model_provider"),
	}
	if p.consentToIntegrity {
		data["consentToIntegrity"] = true
	}
	e.Data = data
	e.RawPayload = raw
	return []Event{e}
}

func (p *codexRolloutProcessor) eventMsg(payload map[string]interface{}, ts, raw string) []Event {
	switch stringField(payload, "type") {
	case "user_message":
		return p.promptEvent(stringField(payload, "message"), ts, raw)

	case "agent_message":
		// Codex emits multiple agent_message lines per turn: "commentary" (interim
		// narration) and "final_answer". Only the final answer is the turn-end
		// assistant message analogous to Claude's Stop.
		if stringField(payload, "phase") != "final_answer" {
			return nil
		}
		return p.aiResponseEvent(stringField(payload, "message"), ts, raw)

	case "item_completed":
		// Codex 0.149 renamed the message stream. The old per-message
		// `user_message` / `agent_message` lines are gone; every item now arrives
		// as `item_completed` with the text under item.content[].text and the
		// agent phase under item.phase.
		//
		// Nothing here recognised that shape, so a candidate's prompts and the
		// model's answers were BOTH dropped on every current codex build — the
		// rollout still produced session_start and tool_use, so the session
		// looked captured while containing no conversation at all.
		//
		// Handled here rather than from `response_item` (which carries the same
		// messages) because response_item also carries the developer-role system
		// preamble and codex's own `<recommended_plugins>` boilerplate as
		// role="user" — capturing those as candidate prompts would be worse than
		// capturing nothing. item_completed carries only real items.
		return p.itemCompleted(payload, ts, raw)

	case "context_compacted":
		return p.contextCompacted(ts, raw)

	case "turn_aborted":
		return p.turnAborted(payload, ts, raw)

	case "web_search_end":
		return p.webSearchEnd(payload, ts)

	case "patch_apply_end":
		return p.patchApplyEnd(payload, ts, raw)

	case "token_count":
		// Stash the latest usage; attached to the next final assistant message.
		if info, ok := payload["info"].(map[string]interface{}); ok {
			if usage, ok := info["total_token_usage"].(map[string]interface{}); ok {
				p.lastTokenUsage = usage
			}
		}
		return nil

	default:
		return nil
	}
}

// contextCompacted converts the rollout's compaction marker into a
// context_compact event — the same kind the Claude PreCompact hook emits, from
// the rail we were already reading and simply never looked at.
//
// Codex has always had /compact; our capture just never reported one, and a
// candidate was graded `context_hygiene: developing` for "zero clears or
// compactions" on a session whose tool could not have shown us one.
//
// Keyed off `event_msg`/`context_compacted`, NOT the sibling `type:"compacted"`
// record. The two arrive 1:1, so either counts the same resets — but
// `compacted`'s payload is the whole `replacement_history` (every prior turn
// verbatim, base64 image data included), and `context_compacted` is an
// `EventMsg` variant, so it survives the 0.149 rename that moved user and agent
// messages under `item_completed` and is handled here rather than there.
//
// No `trigger` and no token counts. Claude's PreCompact carries
// `context_window_used_pct` and the transcript's compact_boundary carries
// auto-vs-manual; the codex rollout carries neither, and it logs no slash
// commands at all, so any value here would be invented — the same manufactured
// fact that produced the false `developing` in the first place.
func (p *codexRolloutProcessor) contextCompacted(ts, raw string) []Event {
	e := p.newCodexEvent("context_compact", ts)
	e.Actor = systemActor()
	e.Data = map[string]interface{}{}
	e.RawPayload = raw
	return []Event{e}
}

// turnAborted converts the rollout's `turn_aborted` marker into the same
// `interrupt` event Claude Code emits when the candidate hits ESC. It was being
// dropped, so on codex a candidate who watched the agent go wrong and stopped it
// was indistinguishable from one who sat there and let it finish — the exact
// judgement the interrupt counters exist to make.
//
// SUBTYPE IS DERIVED FROM THE WIRE, NOT ASSUMED. Claude classifies action (a
// tool call was cut) vs generation (a text reply was cut) from the last
// assistant record; codex says only that the turn ended. But a cut tool call
// never receives its `function_call_output` line, so it is still sitting in
// `pending` when the abort arrives — and a turn cut while the model was writing
// prose has an empty `pending`, because every call it made was already answered.
// Verified against a real rollout: the interrupted turn's last exec completed
// (exit 0, output line present) and the abort followed an assistant `message`,
// which is `generation` and is what this returns for it.
//
// `running` counts too. Codex's own post-abort developer note says "any running
// unified exec processes may still be running in the background", so a detached
// cell in flight at abort time is an action that was cut.
//
// Only `pending` is cleared. Those calls can never be answered — the turn is
// over — and leaving them would let a later call reusing the id inherit the
// wrong tool name. `running` is deliberately left alone for the same reason the
// note gives: a backgrounded process can still complete and still be reported.
//
// Consecutive aborts collapse. Codex can log more than one for a single burst,
// and a second interrupt while the first has no prompt after it yet is the same
// intervention counted twice.
func (p *codexRolloutProcessor) turnAborted(payload map[string]interface{}, ts, raw string) []Event {
	if p.pendingInterruptID != "" {
		return nil
	}

	data := map[string]interface{}{}
	// `reason` is codex's own word for why the turn ended ("interrupted"). Kept
	// under `variant` — the field Claude uses for which sentinel matched — so the
	// two rails answer "what kind of stop was this" in one place.
	if reason := stringField(payload, "reason"); reason != "" {
		data["variant"] = reason
	}

	subtype := "generation"
	if cut := p.cutToolName(); cut != "" {
		subtype = "action"
		data["cutTool"] = cut
	}
	data["subtype"] = subtype
	p.pending = map[string]codexPendingCall{}

	e := p.newCodexEvent("interrupt", ts)
	e.Provenance = humanProvenance()
	e.Data = data
	e.RawPayload = raw
	p.pendingInterruptID = e.ID
	return []Event{e}
}

// cutToolName returns the name of a tool call that was in flight when the turn
// ended, or "" if none was. Map iteration is randomised and a turn can have more
// than one call outstanding, so the name is taken in sorted call-id order rather
// than whichever the runtime hands back first — an event that reports a
// different tool on every re-derivation of the same rollout is worse than one
// that reports a stable arbitrary choice.
func (p *codexRolloutProcessor) cutToolName() string {
	pick := func(m map[string]codexPendingCall) string {
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if name := m[id].name; name != "" {
				return name
			}
		}
		return ""
	}
	if name := pick(p.pending); name != "" {
		return name
	}
	return pick(p.running)
}

// webSearchEnd converts a completed codex web search into the `web_lookup`
// event Claude emits for WebSearch/WebFetch. Codex runs the search host-side,
// so it never appears in the `function_call` stream this file reads for tools —
// it arrives only as this event_msg, which nothing here handled. Every web
// lookup a codex candidate made was therefore invisible, and "did they check
// the docs or guess" had no answer on one of the two rails.
//
// THE QUERY IS KEPT AND THE RESULTS ARE NOT, and that is not a size decision.
// `results[]` is fetched page content — titles, urls, snippets of third-party
// text, thumbnail urls — none of which is the candidate's work or the agent's,
// and all of which would ride into our store as raw payload. The query is the
// signal ("what did they think to look up"); the snippets are someone else's
// copy. `raw` is not passed through for the same reason: it is the whole line,
// results included.
func (p *codexRolloutProcessor) webSearchEnd(payload map[string]interface{}, ts string) []Event {
	query := stringField(payload, "query")
	if strings.TrimSpace(query) == "" {
		return nil
	}
	e := p.newCodexEvent("web_lookup", ts)
	e.Provenance = aiProvenance()
	e.Data = map[string]interface{}{"query": query}
	e.RawPayload = strPreview(query, 500)
	return []Event{e}
}

// itemCompleted handles the codex ≥0.149 message stream. Only the two item types
// that carry conversation are emitted: tool calls still arrive as
// response_item/custom_tool_call pairs, so emitting CommandExecution here would
// double-count every command the agent ran.
func (p *codexRolloutProcessor) itemCompleted(payload map[string]interface{}, ts, raw string) []Event {
	item, _ := payload["item"].(map[string]interface{})
	if item == nil {
		return nil
	}
	switch stringField(item, "type") {
	case "UserMessage":
		return p.promptEvent(codexItemText(item), ts, raw)
	case "AgentMessage":
		// Same rule as the pre-0.149 agent_message: commentary is interim
		// narration, only final_answer is the turn-end assistant message.
		if stringField(item, "phase") != "final_answer" {
			return nil
		}
		return p.aiResponseEvent(codexItemText(item), ts, raw)
	case "FileChange":
		// The 0.149 spelling of patch_apply_end, and the AUTHORITATIVE record of
		// what landed on disk: `update` carries a real unified_diff, `add` and
		// `delete` carry the file's full content. It is emitted only for patches
		// that actually applied, strictly between the apply_patch call and its
		// output line.
		p.sawFileChange = true
		return p.fileChangeItem(item, ts, raw)
	default:
		return nil
	}
}

// fileChangeItem emits one file_diff per changed path from a FileChange item.
//
// This replaces reading the apply_patch ENVELOPE, which is the model's request
// rather than the outcome. The envelope also cannot describe a deletion: it
// spells one `*** Delete File: path` with no body, so a deleted file reported
// diff="" and linesRemoved=0 — the replay then renders "No diff content
// available" for a file the candidate deliberately removed, and the reviewer
// sees a 53-line deletion as zero lines of work.
func (p *codexRolloutProcessor) fileChangeItem(item map[string]interface{}, ts, raw string) []Event {
	changes, ok := item["changes"].(map[string]interface{})
	if !ok || len(changes) == 0 {
		return nil
	}
	// Map iteration is randomised; replay ordering must not be.
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	events := make([]Event, 0, len(paths))
	for _, path := range paths {
		change, _ := changes[path].(map[string]interface{})
		if change == nil {
			continue
		}
		changeType := stringField(change, "type")
		diff := stringField(change, "unified_diff")
		if diff == "" {
			// add/delete carry the whole file instead of a diff. Render it as one,
			// so the line counts are real and the replay has something to show.
			if content := stringField(change, "content"); content != "" {
				diff = prefixEveryLine(content, codexDiffSign(changeType))
			}
		}
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts)
		e.Provenance = aiProvenance()
		data := map[string]interface{}{
			"path":         path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   changeType,
		}
		if mv := stringField(change, "move_path"); mv != "" {
			data["movePath"] = mv
		}
		e.Data = data
		e.RawPayload = strPreview(diff, 500)
		events = append(events, e)
	}
	return events
}

// codexDiffSign is the unified-diff marker for a whole-file change. An unknown
// type is treated as an addition rather than dropped — a diff shown with the
// wrong sign is recoverable by eye; a silently missing file edit is not.
func codexDiffSign(changeType string) string {
	if changeType == "delete" {
		return "-"
	}
	return "+"
}

func prefixEveryLine(content, sign string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i, l := range lines {
		lines[i] = sign + l
	}
	return strings.Join(lines, "\n")
}

// codexItemText joins the text runs of an item_completed content array. Codex
// spells the run type "Text" here and "text"/"input_text"/"output_text"
// elsewhere, so the type is not filtered on — any run carrying "text" counts.
func codexItemText(item map[string]interface{}) string {
	content, _ := item["content"].([]interface{})
	var parts []string
	for _, rawPart := range content {
		part, _ := rawPart.(map[string]interface{})
		if part == nil {
			continue
		}
		if t := stringField(part, "text"); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// promptEvent builds the candidate `prompt` event, shared by the pre- and
// post-0.149 rollout shapes.
func (p *codexRolloutProcessor) promptEvent(text, ts, raw string) []Event {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	e := p.newCodexEvent("prompt", ts)
	e.Provenance = humanProvenance()
	data := map[string]interface{}{"text": text}
	// This prompt is the first thing the candidate said after cutting a run, so
	// it is the evidence that they redirected rather than abandoned it. The
	// backend classifies redirect-vs-abort off this flag alone and clears the
	// interrupt either way, so the flag must be set exactly once, on the FIRST
	// prompt after the interrupt.
	if p.pendingInterruptID != "" {
		e.RelatedEventIDs = append(e.RelatedEventIDs, p.pendingInterruptID)
		data["followsInterrupt"] = true
		p.pendingInterruptID = ""
	}
	// Cadence enrichment, opt-in only — mirrors normalizeClaudeCode.
	if p.consentToIntegrity {
		data["promptLengthChars"] = len(text)
		if last := loadLastPromptTs(); !last.IsZero() {
			deltaMs := time.Since(last).Milliseconds()
			data["timeSinceLastPromptMs"] = deltaMs
			if len(text) > 300 && deltaMs < 1000 {
				data["likelyPaste"] = true
			}
		}
	}
	e.Data = data
	e.RawPayload = raw
	saveLastPromptTs()
	return []Event{e}
}

// aiResponseEvent builds the turn-end `ai_response` event, shared by the pre-
// and post-0.149 rollout shapes.
func (p *codexRolloutProcessor) aiResponseEvent(text, ts, raw string) []Event {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	e := p.newCodexEvent("ai_response", ts)
	data := map[string]interface{}{
		"lastAssistantMessage": text,
	}
	p.attachTokenUsage(data)
	if last := loadLastPromptTs(); !last.IsZero() {
		data["turnDurationMs"] = time.Since(last).Milliseconds()
	}
	e.Data = data
	e.RawPayload = raw
	return []Event{e}
}

// patchApplyEnd emits one file_diff per changed file. The payload carries a
// ready-made unified_diff per path, plus the change type (add/update/delete),
// so no apply-patch envelope parsing is needed.
func (p *codexRolloutProcessor) patchApplyEnd(payload map[string]interface{}, ts, raw string) []Event {
	changes, ok := payload["changes"].(map[string]interface{})
	if !ok || len(changes) == 0 {
		return nil
	}
	var events []Event
	for path, rawChange := range changes {
		change, _ := rawChange.(map[string]interface{})
		if change == nil {
			continue
		}
		diff := stringField(change, "unified_diff")
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts)
		e.Provenance = aiProvenance()
		data := map[string]interface{}{
			"path":         path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   stringField(change, "type"),
		}
		if mv := stringField(change, "move_path"); mv != "" {
			data["movePath"] = mv
		}
		e.Data = data
		e.RawPayload = strPreview(diff, 500)
		events = append(events, e)
	}
	return events
}

func (p *codexRolloutProcessor) responseItem(payload map[string]interface{}, ts, raw string) []Event {
	switch stringField(payload, "type") {
	case "function_call", "custom_tool_call":
		name := stringField(payload, "name")
		args := parseCodexArgs(payload)
		// Codex >= 0.149 exposes ONE generic custom tool named `exec`, whose input
		// is a small JavaScript program calling the real tool (`tools.exec_command`,
		// `tools.apply_patch`, `tools.update_plan`, ...). Unwrap the identity here
		// or every shell run, test run and file edit collapses into an opaque
		// tool_use — which is exactly what shipped: 23 of 23 tool calls landed as
		// toolName="exec" and the fluency judge saw zero commands and zero diffs.
		// Direct codex-cli calls keep their own names and are untouched.
		if name == "exec" {
			name, args = unwrapCodexExec(args)
		}
		// apply_patch is reported via the richer event_msg/patch_apply_end; skip
		// the call line so we don't double-count file edits. Checked AFTER the
		// unwrap so a direct call is still recognised; the wrapped form has no
		// patch_apply_end companion and is emitted from the envelope instead.
		if name == "apply_patch" {
			return nil
		}
		if name == "wrapped_apply_patch" {
			// Arm the FileChange detector for THIS patch. Reset per call so a
			// FileChange from an earlier patch can never vouch for a later one.
			p.sawFileChange = false
		}
		callID := stringField(payload, "call_id")
		if callID != "" {
			p.pending[callID] = codexPendingCall{name: name, args: args}
		}
		return nil

	case "function_call_output", "custom_tool_call_output":
		callID := stringField(payload, "call_id")
		call, ok := p.pending[callID]
		if !ok {
			return nil
		}
		delete(p.pending, callID)
		output := codexOutputText(payload["output"])
		// A detached shell call answers with a cell ID, not a result. Hold it and
		// emit when the completing write_stdin arrives.
		if isCodexShellTool(call.name) {
			if cellID := codexRunningCellID(output); cellID != "" {
				p.running[cellID] = call
				return nil
			}
		}
		if call.name == "write_stdin" {
			cellID := stringField(call.args, "session_id")
			if held, ok := p.running[cellID]; ok {
				// Still running: the host may hand back a new cell ID. Re-key and wait.
				if nextID := codexRunningCellID(output); nextID != "" {
					if nextID != cellID {
						delete(p.running, cellID)
						p.running[nextID] = held
					}
					return nil
				}
				delete(p.running, cellID)
				return p.emitToolEvent(held, output, ts, raw)
			}
		}
		return p.emitToolEvent(call, output, ts, raw)

	default:
		// message / reasoning context items duplicate event_msg signal — skip.
		return nil
	}
}

// emitToolEvent converts a completed tool call (call + output) into the right
// canonical event, branching on the codex tool name.
func (p *codexRolloutProcessor) emitToolEvent(call codexPendingCall, output, ts, raw string) []Event {
	switch {
	case call.name == "wrapped_apply_patch":
		return p.emitWrappedPatch(call, output, ts, raw)

	case isCodexShellTool(call.name):
		cmd := codexCommandString(call.args)
		// The backend's command schema requires a non-empty invocation. The
		// wrapper occasionally builds argv indirectly, where the deliberately
		// non-evaluating extractor cannot recover cmd. Record the occurrence
		// rather than fabricating an empty command or retaining wrapper source.
		if strings.TrimSpace(cmd) == "" {
			e := p.newCodexEvent("tool_use", ts)
			e.Data = map[string]interface{}{
				"toolName": call.name,
				"ok":       codexToolStatus(output) == "completed",
			}
			e.RawPayload = raw
			return []Event{e}
		}
		exitCode, stdout := parseCodexExecOutput(output)
		e := p.newCodexEvent("command", ts)
		e.Provenance = aiProvenance()
		e.Data = map[string]interface{}{
			"command":  cmd,
			"exitCode": exitCode,
			"stdout":   stdout,
		}
		e.RawPayload = raw
		return []Event{e}

	case call.name == "update_plan":
		e := p.newCodexEvent("planning", ts)
		data := map[string]interface{}{}
		// Codex carries the plan steps under "plan" (array of {step,status}).
		if plan, ok := call.args["plan"]; ok {
			data["todos"] = plan
		} else if steps, ok := call.args["steps"]; ok {
			data["todos"] = steps
		}
		e.Data = data
		e.RawPayload = raw
		return []Event{e}

	case isCodexMCPTool(call.name):
		e := p.newCodexEvent("mcp_call", ts)
		e.Data = map[string]interface{}{
			"tool":        call.name,
			"argsPreview": jsonPreview(call.args, 100),
		}
		e.RawPayload = raw
		return []Event{e}

	default:
		e := p.newCodexEvent("tool_use", ts)
		e.Data = map[string]interface{}{
			"toolName":     call.name,
			"inputPreview": jsonPreview(call.args, 100),
			"ok":           true,
		}
		e.RawPayload = raw
		return []Event{e}
	}
}

func (p *codexRolloutProcessor) attachTokenUsage(data map[string]interface{}) {
	u := p.lastTokenUsage
	if u == nil {
		return
	}
	input := intField(u, "input_tokens")
	output := intField(u, "output_tokens")
	cacheRead := intField(u, "cached_input_tokens")
	if input > 0 || output > 0 {
		data["inputTokens"] = input
		data["outputTokens"] = output
		data["cacheReadTokens"] = cacheRead
		// Note: this is an OpenAI-priced estimate placeholder; authoritative cost
		// for BYOK comes from the proxy's metering, not this hook-side guess.
		data["reasoningTokens"] = intField(u, "reasoning_output_tokens")
	}
}

// --- helpers ---------------------------------------------------------------

func isCodexShellTool(name string) bool {
	switch name {
	case "exec_command", "shell", "local_shell", "local_shell_call", "container.exec", "unified_exec":
		return true
	}
	return false
}

// unwrapCodexExec recovers the real tool identity from the JavaScript wrapper
// used by Codex >= 0.149. Only narrowly-recognized calls are lifted; an
// unrecognized program stays a generic `exec` tool_use. The wrapper SOURCE is
// never emitted — only the recovered argument.
func unwrapCodexExec(args map[string]interface{}) (string, map[string]interface{}) {
	input := stringField(args, "input")
	// Dispatch on the FIRST tools.* call in the program, not on whichever name
	// happens to appear anywhere in it. A `strings.Contains` scan tested in a
	// fixed order reads the wrapper's own text: a write_stdin whose payload
	// mentions `tools.exec_command(` — a candidate piping a command string, or
	// grepping for the wrapper — is lifted as a shell command that never ran.
	// Positional matching cannot invert like that.
	switch codexFirstToolsCall(input) {
	case "exec_command":
		out := map[string]interface{}{}
		if cmd := extractJSCmdField(input); cmd != "" {
			out["cmd"] = cmd
		}
		return "exec_command", out
	case "update_plan":
		return "update_plan", map[string]interface{}{}
	case "apply_patch":
		out := map[string]interface{}{}
		if patch := extractJSCallStringArg(input, "tools.apply_patch"); patch != "" {
			out["patch"] = patch
		}
		// Distinct from a direct apply_patch call: that one is skipped above
		// because codex-cli also emits patch_apply_end, while the wrapped form has
		// no such companion record and must derive its file_diffs from the
		// envelope itself.
		return "wrapped_apply_patch", out
	case "write_stdin":
		out := map[string]interface{}{}
		if id := extractJSNumericField(input, "session_id"); id != "" {
			out["session_id"] = id
		}
		return "write_stdin", out
	default:
		return "exec", map[string]interface{}{}
	}
}

// codexFirstToolsCallAt returns the name of the first tools.* invocation that
// appears in CODE position, and the offset just past its opening paren.
//
// The scan skips string literals, template literals and comments, because a
// `tools.<name>(` occurring inside any of them is text the program prints or
// greps for, not a call it makes — and both directions of that mistake are real:
//
//	tools.write_stdin({"chars":"run tools.exec_command(x)\n"})   ← payload names it
//	const note = "calls tools.write_stdin(…)"; tools.exec_command({cmd:"ls"})
//
// A name-ordered `strings.Contains` sweep gets the first wrong and the second
// right; a plain first-match gets the first right and the second wrong. Only
// skipping non-code regions gets both.
//
// Template literals are treated as opaque strings. A `tools.*` call inside a
// `${…}` substitution is therefore missed rather than misread — conservative in
// the direction that costs a classification, never one that invents a call.
func codexFirstToolsCallAt(input string) (string, int) {
	const marker = "tools."
	for i := 0; i < len(input); i++ {
		switch c := input[i]; c {
		case '"', '\'', '`':
			// Skip to the matching close, honouring backslash escapes.
			quote := c
			i++
			for i < len(input) && input[i] != quote {
				if input[i] == '\\' {
					i++
				}
				i++
			}
		case '/':
			if i+1 >= len(input) {
				continue
			}
			if input[i+1] == '/' {
				for i < len(input) && input[i] != '\n' {
					i++
				}
			} else if input[i+1] == '*' {
				if end := strings.Index(input[i+2:], "*/"); end >= 0 {
					i += 2 + end + 1
				} else {
					return "", -1
				}
			}
		case 't':
			// Must not be part of a longer identifier (e.g. `mytools.foo(`).
			if i > 0 && isCodexIdentByte(input[i-1], false) {
				continue
			}
			if !strings.HasPrefix(input[i:], marker) {
				continue
			}
			j := i + len(marker)
			start := j
			for j < len(input) && isCodexIdentByte(input[j], j == start) {
				j++
			}
			if j == start {
				continue
			}
			name := input[start:j]
			for j < len(input) && (input[j] == ' ' || input[j] == '\t' || input[j] == '\n' || input[j] == '\r') {
				j++
			}
			if j < len(input) && input[j] == '(' {
				return name, j + 1
			}
			i = j - 1
		}
	}
	return "", -1
}

// codexFirstToolsCall returns the name of the first tools.* invocation in the
// wrapper program, or "" when there is none.
func codexFirstToolsCall(input string) string {
	name, _ := codexFirstToolsCallAt(input)
	return name
}

// extractJSCmdField pulls the `cmd:` string out of a tools.exec_command({...})
// object literal.
//
// It reads the literal that belongs to THE CALL rather than scanning the whole
// program for a `cmd:`, because both of the shapes a scan gets wrong occur in
// real rollouts:
//
//   - Quoted keys. Codex writes {cmd:"…"} and {"cmd":"…"} interchangeably, and a
//     regex anchored on `\bcmd\s*:` matches only the bare form — the quoted one
//     has a `"` where it needs the colon. That silently dropped the command text
//     on 92 of 635 exec_command calls (14.5%) in a 1,160-call corpus, which the
//     empty-cmd guard then filed as an unclassified tool_use.
//   - Batch loops. `const jobs = [{cmd:"a"},{cmd:"b"}]; for (…) tools.exec_command(j)`
//     puts several `cmd:` fields in the program before the call site, and a
//     first-match scan reports job A as though it were the whole batch.
//
// Failure stays quiet on purpose: an unrecoverable command returns "" so the
// caller records the occurrence as a tool_use, never a command with empty text.
func extractJSCmdField(input string) string {
	_, argStart := codexFirstToolsCallAt(input)
	if argStart < 0 {
		return ""
	}
	rest := input[argStart:]
	if open := strings.IndexByte(rest, '{'); open >= 0 {
		if raw, ok := codexScanObjectLiteral(rest[open:]); ok {
			var obj map[string]interface{}
			if json.Unmarshal([]byte(raw), &obj) == nil {
				if cmd, isStr := obj["cmd"].(string); isStr && cmd != "" {
					return cmd
				}
			}
		}
	}
	// The argument was not a decodable literal — an identifier from a batch loop,
	// a template string, a concatenation. There is deliberately NO fallback scan
	// here: the only thing a whole-program search for `cmd:` can find at this
	// point is a literal belonging to some OTHER call site, and reporting job A's
	// command as though it were this invocation is a worse record than admitting
	// we could not read it. Returning "" files the call as a tool_use.
	return ""
}

func isCodexIdentByte(c byte, first bool) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_', c == '$':
		return true
	case !first && c >= '0' && c <= '9':
		return true
	}
	return false
}

// codexScanObjectLiteral reads one balanced {...} off the front of s, quoting
// bare identifier keys as it goes so the result can be handed to a real JSON
// decoder, and reports whether the object terminated. The scan is string- and
// escape-aware so that braces, commas and colons inside a shell command
// (`rg -n "a,b" --glob '!{x}'`) never move the parser.
func codexScanObjectLiteral(s string) (string, bool) {
	var b strings.Builder
	var stack []byte // '{' or '[' — the container we are currently inside
	inStr, esc, atKey := false, false, false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr, atKey = true, false
			b.WriteByte(c)
		case '{':
			stack = append(stack, '{')
			atKey = true
			b.WriteByte(c)
		case '[':
			stack = append(stack, '[')
			atKey = false
			b.WriteByte(c)
		case '}', ']':
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
			b.WriteByte(c)
			if len(stack) == 0 {
				return b.String(), true
			}
		case ',':
			atKey = stack[len(stack)-1] == '{'
			b.WriteByte(c)
		case ' ', '\t', '\n', '\r':
			b.WriteByte(c)
		default:
			if atKey && isCodexIdentByte(c, true) {
				j := i
				for j < len(s) && isCodexIdentByte(s[j], j == i) {
					j++
				}
				b.WriteByte('"')
				b.WriteString(s[i:j])
				b.WriteByte('"')
				i, atKey = j-1, false
				continue
			}
			atKey = false
			b.WriteByte(c)
		}
	}
	return "", false
}

func extractJSNumericField(input, field string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\s*:\s*(\d+)`)
	m := re.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	return m[1]
}

// extractJSCallStringArg extracts a JSON-style double-quoted first argument.
// Refuses other JavaScript syntax rather than evaluating it or accidentally
// retaining wrapper source.
func extractJSCallStringArg(input, callee string) string {
	start := strings.Index(input, callee+"(")
	if start < 0 {
		return ""
	}
	rest := strings.TrimSpace(input[start+len(callee)+1:])
	if rest == "" {
		return ""
	}
	if rest[0] != '"' {
		// The wrapper commonly binds a large patch to a local first:
		//   const patch = "..."; await tools.apply_patch(patch)
		// Resolve only a plain identifier bound to a double-quoted literal. Never
		// evaluate expressions or template strings.
		end := 0
		for end < len(rest) && ((rest[end] >= 'a' && rest[end] <= 'z') ||
			(rest[end] >= 'A' && rest[end] <= 'Z') ||
			(rest[end] >= '0' && rest[end] <= '9') || rest[end] == '_') {
			end++
		}
		if end == 0 {
			return ""
		}
		name := rest[:end]
		for _, decl := range []string{"const ", "let ", "var "} {
			assign := decl + name
			idx := strings.Index(input, assign)
			if idx < 0 {
				continue
			}
			value := strings.TrimSpace(input[idx+len(assign):])
			if !strings.HasPrefix(value, "=") {
				continue
			}
			rest = strings.TrimSpace(strings.TrimPrefix(value, "="))
			break
		}
	}
	if rest == "" || rest[0] != '"' {
		return ""
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && rest[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			continue
		}
		s, ok := decodeJSDoubleQuoted(rest[:i+1])
		if ok {
			return s
		}
		return ""
	}
	return ""
}

// decodeJSDoubleQuoted decodes one JavaScript double-quoted string literal
// without evaluating JavaScript. The wrapper is JavaScript source, not a Go
// string: beyond JSON escapes it may legally carry \v, \xNN, identity escapes
// or escaped line continuations, all of which strconv.Unquote rejects.
func decodeJSDoubleQuoted(lit string) (string, bool) {
	if len(lit) < 2 || lit[0] != '"' || lit[len(lit)-1] != '"' {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(lit)-1; {
		if lit[i] != '\\' {
			r, size := utf8.DecodeRuneInString(lit[i : len(lit)-1])
			if r == utf8.RuneError && size == 1 {
				return "", false
			}
			out.WriteRune(r)
			i += size
			continue
		}
		i++
		if i >= len(lit)-1 {
			return "", false
		}
		switch lit[i] {
		case '"', '\\', '/':
			out.WriteByte(lit[i])
			i++
		case 'b':
			out.WriteByte('\b')
			i++
		case 'f':
			out.WriteByte('\f')
			i++
		case 'n':
			out.WriteByte('\n')
			i++
		case 'r':
			out.WriteByte('\r')
			i++
		case 't':
			out.WriteByte('\t')
			i++
		case 'v':
			out.WriteByte('\v')
			i++
		case '0':
			// Numeric escapes are unsupported except JavaScript's unambiguous NUL
			// escape (a following decimal digit makes it legacy octal syntax).
			if i+1 < len(lit)-1 && lit[i+1] >= '0' && lit[i+1] <= '9' {
				return "", false
			}
			out.WriteByte(0)
			i++
		case '\n':
			i++ // line continuation contributes no character
		case '\r':
			i++
			if i < len(lit)-1 && lit[i] == '\n' {
				i++
			}
		case 'x':
			v, next, ok := decodeJSHexEscape(lit, i+1, 2)
			if !ok {
				return "", false
			}
			out.WriteRune(rune(v))
			i = next
		case 'u':
			v, next, ok := decodeJSUnicodeEscape(lit, i)
			if !ok {
				return "", false
			}
			i = next
			if v >= 0xD800 && v <= 0xDBFF && i+2 < len(lit)-1 && lit[i] == '\\' && lit[i+1] == 'u' {
				if low, after, lowOK := decodeJSUnicodeEscape(lit, i+1); lowOK && low >= 0xDC00 && low <= 0xDFFF {
					out.WriteRune(utf16.DecodeRune(rune(v), rune(low)))
					i = after
					continue
				}
			}
			if v >= 0xD800 && v <= 0xDFFF {
				out.WriteRune(utf8.RuneError)
			} else {
				out.WriteRune(rune(v))
			}
		default:
			// Identity escape: \q is the character q. Decode a full rune so
			// non-ASCII identity escapes survive too.
			r, size := utf8.DecodeRuneInString(lit[i : len(lit)-1])
			if r == utf8.RuneError && size == 1 {
				return "", false
			}
			out.WriteRune(r)
			i += size
		}
	}
	return out.String(), true
}

func decodeJSUnicodeEscape(lit string, u int) (int, int, bool) {
	if u >= len(lit)-1 || lit[u] != 'u' {
		return 0, u, false
	}
	if u+1 < len(lit)-1 && lit[u+1] == '{' {
		end := strings.IndexByte(lit[u+2:len(lit)-1], '}')
		if end < 0 || end == 0 || end > 6 {
			return 0, u, false
		}
		end += u + 2
		v, _, ok := decodeJSHexEscape(lit, u+2, end-(u+2))
		if !ok || v > utf8.MaxRune {
			return 0, u, false
		}
		return v, end + 1, true
	}
	return decodeJSHexEscape(lit, u+1, 4)
}

func decodeJSHexEscape(lit string, start, count int) (int, int, bool) {
	if count <= 0 || start+count > len(lit)-1 {
		return 0, start, false
	}
	v := 0
	for _, c := range []byte(lit[start : start+count]) {
		v *= 16
		switch {
		case c >= '0' && c <= '9':
			v += int(c - '0')
		case c >= 'a' && c <= 'f':
			v += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v += int(c-'A') + 10
		default:
			return 0, start, false
		}
	}
	return v, start + count, true
}

// codexOutputText accepts both the legacy scalar output and the current
// Responses-style content array: [{type:"input_text","text":"..."}, ...].
// Without this the 0.149 array shape reads as "" and every exit code is lost.
func codexOutputText(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case []interface{}:
		parts := make([]string, 0, len(out))
		for _, item := range out {
			m, _ := item.(map[string]interface{})
			if text := stringField(m, "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func codexToolStatus(output string) string {
	if codexScriptFailed(output) {
		return "failed"
	}
	return "completed"
}

var codexRunningCellRe = regexp.MustCompile(`^Script running with cell ID ([0-9]+)$`)

// codexRunningCellID reports the cell ID when the host DETACHED a shell call.
// The handoff is a control response containing only this line; searching
// multiline stdout would misclassify a completed command that happened to print
// the same text and suppress its event forever.
func codexRunningCellID(output string) string {
	m := codexRunningCellRe.FindStringSubmatch(strings.TrimSpace(output))
	if m == nil {
		return ""
	}
	return m[1]
}

var codexPatchFileHeaders = []struct {
	prefix     string
	changeType string
}{
	{"*** Add File: ", "add"},
	{"*** Update File: ", "update"},
	{"*** Delete File: ", "delete"},
}

// codexPatchMovePrefix marks a rename inside an apply_patch envelope. It
// follows the `*** Update File:` header it belongs to and names the
// DESTINATION; the header keeps naming the source.
const codexPatchMovePrefix = "*** Move to: "

// emitWrappedPatch derives one file_diff per changed file from a wrapped
// apply_patch envelope, which arrives with no patch_apply_end companion.
//
// Unlike the teams normalizer — which is source-excluded and emits paths plus
// line counts only — HIRING keeps the diff body: the replay renders it, and
// `diffContent` is true for this surface. The per-file hunk lines are already
// unified-diff shaped inside the envelope, so they are carried through as-is.
func (p *codexRolloutProcessor) emitWrappedPatch(call codexPendingCall, output, ts, raw string) []Event {
	patch := stringField(call.args, "patch")
	if patch == "" {
		return nil
	}
	// A patch envelope is the model's REQUEST, not a record of what landed. Both
	// failures in the sampled rollout were rejected apply_patch calls ("Failed to
	// find expected lines", "multiple operations target ..."), each followed by a
	// retry. Emitting from the envelope alone invents file_diffs for edits that
	// never touched the tree, and double-counts the retry — inflating exactly the
	// number a reviewer reads as work done.
	if codexWrappedPatchRejected(output) {
		e := p.newCodexEvent("tool_use", ts)
		e.Data = map[string]interface{}{
			"toolName": "apply_patch",
			"ok":       false,
		}
		e.RawPayload = raw
		return []Event{e}
	}
	// The host already reported what landed, with real content for adds and
	// deletes and a real unified_diff for updates. Re-deriving it from the
	// envelope here would double every file edit AND report the worse version.
	if p.sawFileChange {
		return nil
	}
	type change struct {
		path       string
		movePath   string
		changeType string
		lines      []string
	}
	var changes []change
	current := -1
	for _, line := range strings.Split(patch, "\n") {
		matched := false
		for _, h := range codexPatchFileHeaders {
			if strings.HasPrefix(line, h.prefix) {
				changes = append(changes, change{
					path:       strings.TrimSpace(strings.TrimPrefix(line, h.prefix)),
					changeType: h.changeType,
				})
				current = len(changes) - 1
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// A rename names its destination on its own line. Dropping it left the
		// event keyed to a path the patch had just removed, so replay pointed at
		// the wrong file and cross-channel dedup could never match the Git event
		// for the destination. Carry it as movePath — the same shape the
		// FileChange and patch_apply_end producers already emit for a rename.
		if current >= 0 && strings.HasPrefix(line, codexPatchMovePrefix) {
			changes[current].movePath = strings.TrimSpace(strings.TrimPrefix(line, codexPatchMovePrefix))
			continue
		}
		// The rest of the envelope framing (*** Begin Patch / *** End Patch) is
		// not diff content. Everything else in a file section is.
		if current < 0 || strings.HasPrefix(line, "*** ") {
			continue
		}
		changes[current].lines = append(changes[current].lines, line)
	}
	events := make([]Event, 0, len(changes))
	for _, c := range changes {
		diff := strings.Join(c.lines, "\n")
		added, removed := countDiffLines(diff)
		e := p.newCodexEvent("file_diff", ts)
		e.Provenance = aiProvenance()
		data := map[string]interface{}{
			"path":         c.path,
			"diff":         diff,
			"linesAdded":   added,
			"linesRemoved": removed,
			"attribution":  "likely_ai",
			"changeType":   c.changeType,
		}
		if c.movePath != "" {
			data["movePath"] = c.movePath
		}
		e.Data = data
		e.RawPayload = strPreview(diff, 500)
		events = append(events, e)
	}
	return events
}

func isCodexMCPTool(name string) bool {
	// Codex namespaces MCP tools (e.g. "server__tool" or "mcp__server__tool").
	return strings.Contains(name, "__")
}

// codexCommandString extracts a human-readable command from codex tool args,
// which may be {"cmd":"..."}, {"command":"..."} or {"command":["bash","-lc","..."]}.
func codexCommandString(args map[string]interface{}) string {
	if s := stringField(args, "cmd"); s != "" {
		return s
	}
	switch v := args["command"].(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// parseCodexArgs decodes a function_call's "arguments" field, which codex sends
// as a JSON-encoded string. custom_tool_call carries a plain "input" string.
func parseCodexArgs(payload map[string]interface{}) map[string]interface{} {
	if s, ok := payload["arguments"].(string); ok && s != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	if m, ok := payload["arguments"].(map[string]interface{}); ok {
		return m
	}
	if s, ok := payload["input"].(string); ok && s != "" {
		return map[string]interface{}{"input": s}
	}
	return map[string]interface{}{}
}

var codexExitCodeRe = regexp.MustCompile(`(?i)(?:exited with code|exit code:?)\s*(\d+)`)

// parseCodexExecOutput pulls an exit code and the trailing stdout out of codex's
// exec output blob. Two vocabularies exist and BOTH must be read:
//
//	pre-0.149:  Chunk ID: ...\nWall time: ...\nProcess exited with code 0\nOutput:\n<stdout>
//	0.149+:     Script completed\nWall time 0.6 seconds\nOutput:\n<stdout>
//	            Script failed\nWall time 1.5 seconds\nOutput:\n\nScript error:\n<err>
//
// The 0.149 form carries no number, so matching only the numeric shape left
// exitCode at its 0 default for EVERY command. Measured on a real 430-line
// rollout: 43 "Script completed" and 2 "Script failed", all reported as exit 0
// — a fake green, which is worse than a blank because `cleanFirstPass` and the
// red/green arc count are built on it and the judge grades a flawless run that
// never happened.
func parseCodexExecOutput(output string) (int, string) {
	exitCode := 0
	switch {
	case codexExitCodeRe.MatchString(output):
		m := codexExitCodeRe.FindStringSubmatch(output)
		fmt.Sscanf(m[1], "%d", &exitCode)
	case codexScriptFailed(output):
		// The status line reports failure without a code. 1 is the honest
		// stand-in: non-zero is the fact the pipeline reads, and inventing a
		// specific errno would be worse than a generic failure.
		exitCode = 1
	}
	stdout := output
	if idx := strings.Index(output, "Output:\n"); idx >= 0 {
		stdout = output[idx+len("Output:\n"):]
	}
	return exitCode, stdout
}

// codexScriptFailed reports the 0.149 status line's failure form. Anchored to
// the head of the blob: "Script failed" appearing inside captured stdout is the
// command's own output, not this command's verdict.
func codexScriptFailed(output string) bool {
	return strings.HasPrefix(strings.TrimSpace(output), "Script failed")
}

// codexWrappedPatchRejected reports a wrapped apply_patch whose edits never
// reached the tree. The script envelope alone is not enough to decide that:
// codexScriptFailed is deliberately anchored to the head of the blob, and a
// wrapper that CATCHES the rejection reports "Script completed" with the
// refusal in its body. Every wrapped rejection in the sampled rollouts (4/4)
// did fail its script, so the anchored check carries them — this second clause
// is what makes the swallowed case impossible rather than merely unobserved.
//
// Under-reporting is the deliberate direction here. A script that lands one
// patch and is refused another emits nothing from the envelope, but a host that
// applied anything emits FileChange for it, and sawFileChange already returns
// before this point. So the only thing this can cost is an invented diff.
func codexWrappedPatchRejected(output string) bool {
	return codexScriptFailed(output) ||
		strings.Contains(output, "apply_patch verification failed")
}

// countDiffLines counts added/removed lines in a unified diff, excluding the
// ---/+++ file headers.
func countDiffLines(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return
}

// parseCodexTs normalizes a rollout timestamp ("2026-06-06T20:38:45.965Z") to
// RFC3339Nano. Returns "" if it can't be parsed (caller keeps the default).
func parseCodexTs(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func intField(m map[string]interface{}, key string) int64 {
	if f, ok := m[key].(float64); ok {
		return int64(f)
	}
	return 0
}
