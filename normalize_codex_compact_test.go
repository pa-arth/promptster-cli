package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A real compaction pair, copied from a local rollout (codex-cli 0.146.0). The
// `compacted` record's replacement_history is trimmed to one entry — what
// matters is that both lines go in and one event comes out.
const (
	codexCompactRecordLine = `{"timestamp":"2026-08-03T22:15:03.710Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","id":"msg_x","role":"user","content":[{"type":"input_text","text":"the entire prior conversation, verbatim"}]}]}}`
	codexCompactMarkerLine = `{"timestamp":"2026-08-03T22:15:03.833Z","type":"event_msg","payload":{"type":"context_compacted"}}`
)

func codexCompactEvents(t *testing.T, lines ...string) []Event {
	t.Helper()
	p := newCodexRolloutProcessor("sess-1", false)
	var out []Event
	for _, l := range lines {
		out = append(out, p.process([]byte(l))...)
	}
	return out
}

// One compaction, one event. Before this a codex session reported zero resets no
// matter how many it performed, which is what let a candidate be graded
// `context_hygiene: developing` for "zero clears or compactions".
func TestCodexCompactionEmitsOneResetEvent(t *testing.T) {
	events := codexCompactEvents(t, codexRolloutLines[0], codexCompactRecordLine, codexCompactMarkerLine)

	var compacts []Event
	for _, e := range events {
		if e.Kind == "context_compact" {
			compacts = append(compacts, e)
		}
	}
	if len(compacts) != 1 {
		t.Fatalf("got %d context_compact events, want exactly 1 — the `compacted` record and the "+
			"`context_compacted` marker arrive 1:1, so reading both double-counts every reset", len(compacts))
	}
	e := compacts[0]
	if e.Source != "codex" {
		t.Errorf("source = %q, want codex", e.Source)
	}
	if e.SessionID != "sess-1" {
		t.Errorf("sessionId = %q, want sess-1", e.SessionID)
	}
	if e.Actor == nil || e.Actor.Type != "system" {
		t.Errorf("actor = %+v, want the system actor — a compaction is not something the candidate typed, "+
			"and the Claude PreCompact path stamps it the same way", e.Actor)
	}
}

// Nothing is manufactured on the way out. Claude carries auto-vs-manual and a
// context percentage; codex carries neither and logs no slash commands at all,
// so inventing a value here would repeat the mistake this change undoes.
func TestCodexCompactionInventsNothing(t *testing.T) {
	for _, e := range codexCompactEvents(t, codexRolloutLines[0], codexCompactRecordLine, codexCompactMarkerLine) {
		if e.Kind != "context_compact" {
			continue
		}
		data, _ := e.Data.(map[string]interface{})
		for _, k := range []string{"trigger", "contextPct", "preTokens", "postTokens"} {
			if v, ok := data[k]; ok {
				t.Errorf("context_compact carries %s=%v; the codex rollout does not report it", k, v)
			}
		}
	}
}

// The `compacted` record we deliberately skip carries the entire prior
// conversation. Keying off the marker keeps it out by construction.
func TestCodexCompactionNeverCarriesReplacementHistory(t *testing.T) {
	for _, e := range codexCompactEvents(t, codexRolloutLines[0], codexCompactRecordLine, codexCompactMarkerLine) {
		if e.Kind != "context_compact" {
			continue
		}
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), "the entire prior conversation") {
			t.Fatalf("context_compact leaked replacement_history: %s", blob)
		}
	}
}

// The marker is an EventMsg variant, so it still arrives on codex >= 0.149 where
// user and agent messages moved under item_completed. If a future build moves it
// too, this is the test that says so.
func TestCodexCompactionSurvivesItemCompletedStream(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-08-26T08:24:40.000Z","type":"session_meta","payload":{"id":"01a03e3e","cwd":"/tmp/ws","originator":"codex-tui","cli_version":"0.149.1","model_provider":"openai"}}`,
		`{"timestamp":"2026-08-26T08:25:00.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"input_text","text":"go on"}]}}}`,
		codexCompactMarkerLine,
	}
	n := 0
	for _, e := range codexCompactEvents(t, lines...) {
		if e.Kind == "context_compact" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d context_compact on a 0.149 rollout, want 1", n)
	}
}
