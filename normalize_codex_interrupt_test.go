package main

import "testing"

// Real lines, copied from a local rollout (codex-cli 0.146.0) — the abort marker
// and the post-abort developer note codex writes beside it. `results` and other
// bulk are untouched; these carry none.
const (
	codexTurnAbortedLine = `{"timestamp":"2026-07-29T05:17:10.432Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"019fac4d-e8f5-71d1-82cc-e6189dca9f09","reason":"interrupted","started_at":1785302214,"completed_at":1785302230,"duration_ms":15472}}`

	// A call with no output line after it: the shape a tool cut mid-run leaves.
	codexUnansweredCallLine = `{"timestamp":"2026-07-29T05:17:05.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pnpm test\",\"workdir\":\"/tmp/ws\"}","call_id":"call_CUT"}}`

	codexPostAbortPromptLine = `{"timestamp":"2026-07-29T05:17:30.000Z","type":"event_msg","payload":{"type":"user_message","message":"stop, do the other file first","images":[]}}`

	codexSecondPromptLine = `{"timestamp":"2026-07-29T05:18:30.000Z","type":"event_msg","payload":{"type":"user_message","message":"now run the tests","images":[]}}`
)

func codexEvents(t *testing.T, lines ...string) []Event {
	t.Helper()
	p := newCodexRolloutProcessor("sess-1", false)
	var out []Event
	for _, l := range lines {
		out = append(out, p.process([]byte(l))...)
	}
	return out
}

func onlyKind(events []Event, kind string) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// The whole point: a codex candidate who stopped a run used to be
// indistinguishable from one who let it finish, because turn_aborted was not in
// the dispatch at all.
func TestCodexTurnAbortedEmitsInterrupt(t *testing.T) {
	events := codexEvents(t, codexRolloutLines[0], codexTurnAbortedLine)

	interrupts := onlyKind(events, "interrupt")
	if len(interrupts) != 1 {
		t.Fatalf("got %d interrupt events, want 1", len(interrupts))
	}
	e := interrupts[0]
	if e.Source != "codex" {
		t.Errorf("source = %q, want codex", e.Source)
	}
	// Stopping a run is the candidate acting, not the agent. If this reads as the
	// agent, every human-intervention count on codex measures the wrong actor.
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Errorf("actor = %+v, want the human actor", e.Actor)
	}
	if got := eventData(t, e)["variant"]; got != "interrupted" {
		t.Errorf("variant = %v, want codex's own reason string", got)
	}
}

// A turn cut while the model was writing prose. Every call it made was already
// answered, so nothing is in flight — verified against the real rollout this
// fixture came from, where the last exec exited 0 before the abort landed.
func TestCodexAbortAfterAnsweredCallIsGeneration(t *testing.T) {
	events := codexEvents(t,
		codexRolloutLines[0],
		codexRolloutLines[4], // exec_command call_B
		codexRolloutLines[5], // its output
		codexTurnAbortedLine,
	)

	interrupts := onlyKind(events, "interrupt")
	if len(interrupts) != 1 {
		t.Fatalf("got %d interrupts, want 1", len(interrupts))
	}
	data := eventData(t, interrupts[0])
	if data["subtype"] != "generation" {
		t.Errorf("subtype = %v, want generation — no call was outstanding when the turn ended", data["subtype"])
	}
	if _, ok := data["cutTool"]; ok {
		t.Errorf("cutTool = %v on a generation interrupt; no tool was cut", data["cutTool"])
	}
}

// A turn cut with a call still outstanding. The cut call never receives its
// function_call_output, which is exactly what makes this detectable.
func TestCodexAbortWithCallInFlightIsAction(t *testing.T) {
	events := codexEvents(t, codexRolloutLines[0], codexUnansweredCallLine, codexTurnAbortedLine)

	interrupts := onlyKind(events, "interrupt")
	if len(interrupts) != 1 {
		t.Fatalf("got %d interrupts, want 1", len(interrupts))
	}
	data := eventData(t, interrupts[0])
	if data["subtype"] != "action" {
		t.Errorf("subtype = %v, want action — exec_command was still in flight", data["subtype"])
	}
	if data["cutTool"] != "exec_command" {
		t.Errorf("cutTool = %v, want exec_command", data["cutTool"])
	}
}

// The flag the backend reads to tell "cut it and steered" from "cut it and
// walked away". It belongs on the FIRST prompt after the interrupt and no other.
func TestCodexPromptAfterAbortIsFlaggedOnceAndBackLinked(t *testing.T) {
	events := codexEvents(t,
		codexRolloutLines[0],
		codexTurnAbortedLine,
		codexPostAbortPromptLine,
		codexSecondPromptLine,
	)

	interrupts := onlyKind(events, "interrupt")
	prompts := onlyKind(events, "prompt")
	if len(interrupts) != 1 || len(prompts) != 2 {
		t.Fatalf("got %d interrupts and %d prompts, want 1 and 2", len(interrupts), len(prompts))
	}

	first := eventData(t, prompts[0])
	if first["followsInterrupt"] != true {
		t.Errorf("first prompt after the abort is not flagged followsInterrupt; the backend scores this "+
			"interrupt as neither redirect nor abort. data=%v", first)
	}
	if len(prompts[0].RelatedEventIDs) != 1 || prompts[0].RelatedEventIDs[0] != interrupts[0].ID {
		t.Errorf("redirect prompt back-links %v, want the interrupt id %q", prompts[0].RelatedEventIDs, interrupts[0].ID)
	}

	second := eventData(t, prompts[1])
	if _, ok := second["followsInterrupt"]; ok {
		t.Errorf("a later unrelated prompt is flagged followsInterrupt — every prompt in the session " +
			"would read as a redirect")
	}
}

// ESC ESC. Codex can log more than one abort for a single intervention; counting
// both reports two interventions the candidate did not make.
func TestCodexConsecutiveAbortsCollapse(t *testing.T) {
	events := codexEvents(t, codexRolloutLines[0], codexTurnAbortedLine, codexTurnAbortedLine)
	if n := len(onlyKind(events, "interrupt")); n != 1 {
		t.Fatalf("got %d interrupts for a burst, want 1", n)
	}
}

// A second abort AFTER the redirect prompt is a second real intervention.
func TestCodexAbortAfterRedirectCountsAgain(t *testing.T) {
	events := codexEvents(t,
		codexRolloutLines[0],
		codexTurnAbortedLine,
		codexPostAbortPromptLine,
		codexTurnAbortedLine,
	)
	if n := len(onlyKind(events, "interrupt")); n != 2 {
		t.Fatalf("got %d interrupts, want 2 — the burst-collapse must not swallow a later intervention", n)
	}
}

// The cut call is dropped, not resurrected. Its id can be reused by a later
// call, and inheriting the cut tool's name would report a command that never ran.
func TestCodexAbortClearsTheCutCall(t *testing.T) {
	lateOutput := `{"timestamp":"2026-07-29T05:17:40.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_CUT","output":"Chunk ID: z9\nProcess exited with code 0\nOutput:\nlate\n"}}`
	events := codexEvents(t, codexRolloutLines[0], codexUnansweredCallLine, codexTurnAbortedLine, lateOutput)

	if n := len(onlyKind(events, "command")); n != 0 {
		t.Fatalf("got %d command events after an abort cleared the call, want 0", n)
	}
}
