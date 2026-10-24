package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// askQuestionsJSON is the question set from the customer's session (932d08ef,
// ~minute 21), plus a multi-select whose label contains the ", " separator.
const askQuestionsJSON = `[
 {"question":"Include the dedicated liveness metric in this PR?","header":"Liveness","multiSelect":false,
  "options":[{"label":"Include it (Recommended)","description":"Emit github.actions.poller.liveness"},{"label":"Defer it","description":"Follow-up PR"}]},
 {"question":"Fix the duplicate jobs fetch for runs in both queued and in_progress?","header":"Dup fetch","multiSelect":false,
  "options":[{"label":"Fix it now"},{"label":"Defer"}]},
 {"question":"Which alerts should page?","header":"Paging","multiSelect":true,
  "options":[{"label":"Liveness, staleness"},{"label":"Rate limit"},{"label":"Errors"}]}
]`

const askAnswersJSON = `{
 "Include the dedicated liveness metric in this PR?":"Include it (Recommended)",
 "Fix the duplicate jobs fetch for runs in both queued and in_progress?":"yes defer + mention in the PR since it should be addressed in the future",
 "Which alerts should page?":"Liveness, staleness, Errors, only during business hours"
}`

func askQuestions(t *testing.T, e Event) []map[string]interface{} {
	t.Helper()
	qs, ok := dm(e)["questions"].([]map[string]interface{})
	if !ok {
		t.Fatalf("questions = %#v", dm(e)["questions"])
	}
	return qs
}

func assertCustomerAnswers(t *testing.T, e Event) {
	t.Helper()
	if e.Kind != "question_answer" {
		t.Fatalf("kind = %s", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Errorf("actor = %+v", e.Actor)
	}
	if dm(e)["status"] != "answered" || dm(e)["toolName"] != "AskUserQuestion" {
		t.Errorf("data = %v", dm(e))
	}
	qs := askQuestions(t, e)
	if len(qs) != 3 {
		t.Fatalf("questions = %d", len(qs))
	}
	if qs[0]["header"] != "Liveness" || !reflect.DeepEqual(qs[0]["selected"], []string{"Include it (Recommended)"}) {
		t.Errorf("q0 = %v", qs[0])
	}
	opts, _ := qs[0]["options"].([]map[string]interface{})
	if len(opts) != 2 || opts[0]["description"] != "Emit github.actions.poller.liveness" {
		t.Errorf("q0 options = %v", opts)
	}
	// Free text typed into "Other" on a single-select.
	if !reflect.DeepEqual(qs[1]["selected"], []string{}) ||
		qs[1]["otherText"] != "yes defer + mention in the PR since it should be addressed in the future" {
		t.Errorf("q1 = %v", qs[1])
	}
	// Multi-select: a label containing ", " stays whole; the rest is Other.
	if !reflect.DeepEqual(qs[2]["selected"], []string{"Liveness, staleness", "Errors"}) ||
		qs[2]["otherText"] != "only during business hours" || qs[2]["multiSelect"] != true {
		t.Errorf("q2 = %v", qs[2])
	}
}

func TestAskUserQuestionHookPostToolUse(t *testing.T) {
	payload := map[string]interface{}{}
	raw := `{"hook_event_name":"PostToolUse","session_id":"ide-1","transcript_path":"/x.jsonl","cwd":"/home/user/task","tool_name":"AskUserQuestion",
	 "tool_input":{"questions":` + askQuestionsJSON + `,"answers":` + askAnswersJSON + `,"annotations":{}},
	 "tool_response":{"questions":` + askQuestionsJSON + `,"answers":` + askAnswersJSON + `,"annotations":{"Which alerts should page?":{"notes":"weekends are quiet"}}}}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	e, ok := normalize(payload, "sess-1")
	if !ok {
		t.Fatal("not normalized")
	}
	assertCustomerAnswers(t, e)
	if askQuestions(t, e)[2]["notes"] != "weekends are quiet" {
		t.Errorf("notes = %v", askQuestions(t, e)[2])
	}
}

func TestAskUserQuestionHookDismissed(t *testing.T) {
	payload := map[string]interface{}{}
	raw := `{"hook_event_name":"PostToolUseFailure","session_id":"ide-1","tool_name":"AskUserQuestion","error":"User rejected tool use",
	 "tool_input":{"questions":` + askQuestionsJSON + `}}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	e, ok := normalize(payload, "sess-1")
	if !ok || e.Kind != "question_answer" || dm(e)["status"] != "declined" {
		t.Fatalf("event = %+v", e)
	}
	if qs := askQuestions(t, e); len(qs) != 3 || qs[0]["answer"] != nil {
		t.Errorf("questions = %v", qs)
	}
}

func TestAskUserQuestionTranscript(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1", false)
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus-5","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{"questions":`+compactJSON(askQuestionsJSON)+`}}]},"timestamp":"2026-10-21T16:52:11Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"The user answered: ..."}]},"toolUseResult":{"questions":`+compactJSON(askQuestionsJSON)+`,"answers":`+compactJSON(askAnswersJSON)+`,"annotations":{}},"timestamp":"2026-10-21T16:53:05Z"}`,
	)
	var qa *Event
	for i := range events {
		if events[i].Kind == "question_answer" {
			qa = &events[i]
		}
	}
	if qa == nil {
		t.Fatalf("no question_answer in %v", events)
	}
	assertCustomerAnswers(t, *qa)
	if qa.Ts != "2026-10-21T16:53:05Z" {
		t.Errorf("ts = %s", qa.Ts)
	}
}

func TestAskUserQuestionTranscriptRejected(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1", false)
	events := processAll(t, p,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{"questions":`+compactJSON(askQuestionsJSON)+`}}]},"timestamp":"2026-10-21T16:52:11Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","is_error":true,"content":"The user doesn't want to proceed with this tool use."}]},"toolUseResult":"User rejected tool use","timestamp":"2026-10-21T16:53:05Z"}`,
	)
	for _, e := range events {
		if e.Kind == "question_answer" {
			if dm(e)["status"] != "declined" || len(askQuestions(t, e)) != 3 {
				t.Errorf("data = %v", dm(e))
			}
			return
		}
	}
	t.Fatalf("no question_answer in %v", events)
}

func compactJSON(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Shapes verified against Claude Code 2.1.296 transcripts.
const (
	queuedHumanLine = `{"parentUuid":"0f44","isSidechain":false,"attachment":{"type":"queued_command","prompt":"also the rate is about 4k/hr at peak","source_uuid":"4bb2","delivery_id":"28c0","commandMode":"prompt","origin":{"kind":"human"},"timestamp":"2026-10-24T06:10:41.230Z","humanTurn":true},"type":"attachment","uuid":"53e9","timestamp":"2026-10-24T06:11:02.648Z","cwd":"/home/user/task","sessionId":"ide-1","version":"2.1.296"}`
	queuedTaskLine  = `{"isSidechain":false,"attachment":{"type":"queued_command","prompt":"<task-notification>\n<task-id>b1</task-id>\n</task-notification>","commandMode":"task-notification","origin":{"kind":"task-notification"}},"type":"attachment","timestamp":"2026-10-24T06:12:00Z"}`
	queuedCoordLine = `{"isSidechain":false,"attachment":{"type":"queued_command","prompt":"coordinator note","origin":{"kind":"coordinator"},"isMeta":true},"type":"attachment","timestamp":"2026-10-24T06:12:01Z"}`
	queueOpLine     = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-24T06:10:41.345Z","sessionId":"ide-1","content":"also the rate is about 4k/hr at peak"}`
)

func TestQueuedPromptTranscript(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1", false)
	events := processAll(t, p, queueOpLine, queuedHumanLine, queuedTaskLine, queuedCoordLine)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(events), events)
	}
	e := events[0]
	if e.Kind != "prompt" || e.Actor == nil || e.Actor.Type != "human" {
		t.Fatalf("event = %+v", e)
	}
	if dm(e)["text"] != "also the rate is about 4k/hr at peak" || dm(e)["queued"] != true {
		t.Errorf("data = %v", dm(e))
	}
	// When the candidate pressed enter, not when Claude picked it up.
	if e.Ts != "2026-10-24T06:10:41.23Z" {
		t.Errorf("ts = %s", e.Ts)
	}
	if meta, _ := dm(e)["meta"].(map[string]interface{}); meta["ideSessionId"] != "ide-1" {
		t.Errorf("meta = %v", meta)
	}
}

func TestDrainQueuedPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	transcript := filepath.Join(dir, "ide-1.jsonl")
	write := func(lines ...string) {
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, l := range lines {
			if _, err := f.WriteString(l); err != nil {
				t.Fatal(err)
			}
		}
	}
	start, _ := time.Parse(time.RFC3339, "2026-10-24T06:00:00Z")

	old := strings.Replace(queuedHumanLine, "2026-10-24T06:10:41.230Z", "2026-10-24T05:00:00Z", 1)
	write(old+"\n", queueOpLine+"\n", queuedHumanLine+"\n", queuedTaskLine+"\n")
	// A half-written line is left for the next scan.
	second := strings.Replace(queuedHumanLine, "4k/hr at peak", "9k/hr on Mondays", 1)
	write(second[:40])

	drain := func() []Event {
		var got []Event
		drainQueuedPrompts(transcript, "sess-1", start, func(e Event) { got = append(got, e) })
		return got
	}
	got := drain()
	if len(got) != 1 || dm(got[0])["text"] != "also the rate is about 4k/hr at peak" || got[0].SessionID != "sess-1" {
		t.Fatalf("first drain = %v", got)
	}
	if again := drain(); len(again) != 0 {
		t.Fatalf("re-drain emitted %v", again)
	}
	write(second[40:] + "\n")
	got = drain()
	if len(got) != 1 || dm(got[0])["text"] != "also the rate is about 9k/hr on Mondays" {
		t.Fatalf("second drain = %v", got)
	}
}
