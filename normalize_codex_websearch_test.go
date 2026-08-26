package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A real web_search_end, copied from a local rollout with `results` trimmed to
// one entry — the entry is kept verbatim because the point of these tests is
// that its contents never leave the normalizer.
const codexWebSearchLine = `{"timestamp":"2026-08-12T11:02:41.000Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"call_KXbk","query":"redis rate limiter sliding window","action":{"type":"search","queries":["redis rate limiter sliding window"]},"results":[{"type":"text_result","domain":"www.techradar.com","ref_id":"turn0news12","snippet":"THIRD PARTY PAGE TEXT that is nobody's work product","thumbnail_url":"https://images.openai.com/static-rsc-1/DdQ8","title":"Stop measuring AI usage","url":"https://www.techradar.com/pro/stop-measuring-ai-usage"}]}}`

// Codex runs search host-side, so it never appears in the function_call stream
// this file reads for tools. Before this it produced no event at all, and "did
// they look it up or guess" had no answer on codex.
func TestCodexWebSearchEmitsWebLookup(t *testing.T) {
	events := codexEvents(t, codexRolloutLines[0], codexWebSearchLine)

	lookups := onlyKind(events, "web_lookup")
	if len(lookups) != 1 {
		t.Fatalf("got %d web_lookup events, want 1", len(lookups))
	}
	e := lookups[0]
	if e.Source != "codex" {
		t.Errorf("source = %q, want codex", e.Source)
	}
	if e.Actor == nil || e.Actor.Type != "ai" {
		t.Errorf("actor = %+v, want the ai actor — the agent ran the search", e.Actor)
	}
	if got := eventData(t, e)["query"]; got != "redis rate limiter sliding window" {
		t.Errorf("query = %v, want the candidate-visible search string", got)
	}
}

// The query is the signal; `results` is fetched third-party page text — titles,
// urls, snippets, thumbnail urls — and none of it is the candidate's work or the
// agent's. It must not ride into the store on the event or its raw payload.
func TestCodexWebSearchNeverCarriesResults(t *testing.T) {
	for _, e := range onlyKind(codexEvents(t, codexRolloutLines[0], codexWebSearchLine), "web_lookup") {
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"THIRD PARTY PAGE TEXT", "techradar", "thumbnail_url", "turn0news12"} {
			if strings.Contains(string(blob), leak) {
				t.Fatalf("web_lookup leaked %q from results[]: %s", leak, blob)
			}
		}
	}
}

// Nothing is manufactured. Codex reports no url for a search (only for a fetch),
// and the allowlist admitting `url` is not a reason to invent one.
func TestCodexWebSearchInventsNoUrl(t *testing.T) {
	for _, e := range onlyKind(codexEvents(t, codexRolloutLines[0], codexWebSearchLine), "web_lookup") {
		if v, ok := eventData(t, e)["url"]; ok {
			t.Errorf("web_lookup carries url=%v; a codex search reports no single url", v)
		}
	}
}

// A search with no query string is not evidence of a lookup, and the backend's
// command-shaped schemas reject empty required fields besides.
func TestCodexWebSearchWithoutQueryEmitsNothing(t *testing.T) {
	empty := `{"timestamp":"2026-08-12T11:02:41.000Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"call_x","query":"   ","results":[]}}`
	if n := len(onlyKind(codexEvents(t, codexRolloutLines[0], empty), "web_lookup")); n != 0 {
		t.Fatalf("got %d web_lookup events for an empty query, want 0", n)
	}
}
