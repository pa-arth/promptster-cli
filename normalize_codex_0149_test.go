package main

import (
	"strings"
	"testing"
)

// Codex 0.149 replaced the per-message rollout lines this normalizer was built
// against. `event_msg/user_message` and `event_msg/agent_message` no longer
// appear at all; every item arrives as `event_msg/item_completed` with the text
// under item.content[].text.
//
// Nothing recognised that, so on every current codex build a captured session
// contained session_start and tool_use and NOT ONE prompt or answer. That is the
// worst failure shape available: the session looks captured, the reviewer opens
// it, and the conversation is simply absent.
//
// These lines are verbatim from a real codex 0.149.1 rollout.

const (
	codex0149UserMessage = `{"timestamp":"2026-08-26T04:41:07.915Z","type":"event_msg","payload":{"type":"item_completed","thread_id":"01a03c5f-3377-7b43-9b54-de41173d1ac8","turn_id":"01a03c5f-342f-7e50-a31c-6ee5dd90ef0c","item":{"type":"UserMessage","id":"01a03c5f-364b","content":[{"type":"text","text":"In one short sentence, what does the Style Match feature do?","text_elements":[]}]}}}`

	codex0149AgentCommentary = `{"timestamp":"2026-08-26T04:41:11.895Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"msg_a","content":[{"type":"Text","text":"I'll quickly check the project's wording."}],"phase":"commentary"}}}`

	codex0149AgentFinal = `{"timestamp":"2026-08-26T04:41:17.809Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"msg_b","content":[{"type":"Text","text":"Style Match recommends a coherent outfit suited to a customer's appearance."}],"phase":"final_answer"}}}`

	codex0149CommandExecution = `{"timestamp":"2026-08-26T04:41:15.0Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-1","command":["/bin/zsh","-lc","rg -n style ."],"status":"completed","stdout":"README.md:3:..."}}}`

	codex0149Reasoning = `{"timestamp":"2026-08-26T04:41:11.6Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"rs_1","summary_text":[],"raw_content":[]}}}`
)

// eventData narrows Event.Data (interface{}) to the map every normalizer builds.
func eventData(t *testing.T, e Event) map[string]interface{} {
	t.Helper()
	m, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("event %s carries %T, not a data map", e.Kind, e.Data)
	}
	return m
}

func kindsOf(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func TestCodex0149CapturesThePrompt(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := p.process([]byte(codex0149UserMessage))

	if len(events) != 1 || events[0].Kind != "prompt" {
		t.Fatalf("expected one prompt event, got %v", kindsOf(events))
	}
	text, _ := eventData(t, events[0])["text"].(string)
	if !strings.Contains(text, "what does the Style Match feature do") {
		t.Errorf("prompt text not extracted from item.content[].text: %q", text)
	}
	if events[0].Provenance == nil || events[0].Provenance.Attribution != humanProvenance().Attribution {
		t.Errorf("a candidate's prompt must be attributed to the human, got %+v", events[0].Provenance)
	}
}

func TestCodex0149CapturesTheFinalAnswerOnly(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)

	// Commentary is interim narration, exactly as pre-0.149 agent_message was.
	if got := p.process([]byte(codex0149AgentCommentary)); len(got) != 0 {
		t.Errorf("commentary should not be emitted as the turn's answer, got %v", kindsOf(got))
	}

	events := p.process([]byte(codex0149AgentFinal))
	if len(events) != 1 || events[0].Kind != "ai_response" {
		t.Fatalf("expected one ai_response event, got %v", kindsOf(events))
	}
	msg, _ := eventData(t, events[0])["lastAssistantMessage"].(string)
	if !strings.Contains(msg, "coherent outfit") {
		t.Errorf("answer text not extracted: %q", msg)
	}
}

// Commands still arrive as response_item/custom_tool_call + output pairs, which
// this normalizer already handled. Emitting CommandExecution here too would
// double-count every command the agent ran.
func TestCodex0149DoesNotDoubleCountCommandsOrEmitReasoning(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	if got := p.process([]byte(codex0149CommandExecution)); len(got) != 0 {
		t.Errorf("CommandExecution must not be emitted here (tool calls come from response_item): %v", kindsOf(got))
	}
	if got := p.process([]byte(codex0149Reasoning)); len(got) != 0 {
		t.Errorf("Reasoning items carry no candidate-visible signal: %v", kindsOf(got))
	}
}

// The pre-0.149 shape must keep working — a candidate on an older codex is not
// a candidate we stop capturing.
func TestPre0149MessageShapeStillWorks(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)

	events := p.process([]byte(`{"timestamp":"2026-08-26T04:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"old shape prompt"}}`))
	if len(events) != 1 || events[0].Kind != "prompt" || eventData(t, events[0])["text"] != "old shape prompt" {
		t.Fatalf("pre-0.149 user_message regressed: %v", kindsOf(events))
	}

	if got := p.process([]byte(`{"timestamp":"2026-08-26T04:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"interim","phase":"commentary"}}`)); len(got) != 0 {
		t.Errorf("pre-0.149 commentary should still be skipped: %v", kindsOf(got))
	}

	events = p.process([]byte(`{"timestamp":"2026-08-26T04:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"old shape answer","phase":"final_answer"}}`))
	if len(events) != 1 || events[0].Kind != "ai_response" || eventData(t, events[0])["lastAssistantMessage"] != "old shape answer" {
		t.Fatalf("pre-0.149 agent_message regressed: %v", kindsOf(events))
	}
}

// response_item/message carries the same conversation AND codex's developer-role
// system preamble and its `<recommended_plugins>` boilerplate as role="user".
// Capturing those as candidate prompts would be worse than capturing nothing,
// which is why item_completed is the source.
func TestResponseItemMessagesAreNotCapturedAsPrompts(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	for _, line := range []string{
		`{"timestamp":"2026-08-26T04:41:07.6Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>...</skills_instructions>"}]}}`,
		`{"timestamp":"2026-08-26T04:41:07.7Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>Airtable, Alpaca, Spotify...</recommended_plugins>"}]}}`,
		`{"timestamp":"2026-08-26T04:41:07.9Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"In one short sentence, what does the Style Match feature do?"}]}}`,
	} {
		if got := p.process([]byte(line)); len(got) != 0 {
			t.Errorf("response_item/message must not emit events (item_completed owns the conversation): %v", kindsOf(got))
		}
	}
}

// End to end over the turn as codex 0.149.1 actually writes it: one prompt, one
// answer, nothing doubled.
func TestCodex0149FullTurn(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	var all []Event
	for _, line := range []string{
		codex0149UserMessage,
		codex0149Reasoning,
		codex0149AgentCommentary,
		codex0149CommandExecution,
		codex0149AgentFinal,
	} {
		all = append(all, p.process([]byte(line))...)
	}
	got := kindsOf(all)
	if len(got) != 2 || got[0] != "prompt" || got[1] != "ai_response" {
		t.Fatalf("a one-prompt turn should normalize to exactly [prompt ai_response], got %v", got)
	}
}
