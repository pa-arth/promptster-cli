package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubTTY struct {
	reader *strings.Reader
	writes bytes.Buffer
}

func newStubTTY(input string) *stubTTY {
	return &stubTTY{reader: strings.NewReader(input)}
}

func (s *stubTTY) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *stubTTY) Write(p []byte) (int, error) {
	return s.writes.Write(p)
}

func (s *stubTTY) Close() error {
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestDetectDecisionCandidateForSchemaFileDiff(t *testing.T) {
	event := newEvent("file_diff", "sess-1")
	event.Data = map[string]interface{}{
		"path": "packages/db/src/schema.ts",
		"diff": "+ decision capture fields",
	}

	candidate := detectDecisionCandidate(event)
	if candidate == nil {
		t.Fatalf("expected candidate")
	}
	if candidate.CategoryHint != "data-model" {
		t.Fatalf("category = %q", candidate.CategoryHint)
	}
	if candidate.ImpactScore != 5 {
		t.Fatalf("impact = %d", candidate.ImpactScore)
	}
}

func TestDetectDecisionCandidateForNonArchitecturalCommand(t *testing.T) {
	event := newEvent("command", "sess-1")
	event.Data = map[string]interface{}{
		"command": "ls -la",
	}

	if candidate := detectDecisionCandidate(event); candidate != nil {
		t.Fatalf("expected no candidate, got %#v", candidate)
	}
}

func TestDecisionQueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_DECISION_QUEUE_PATH", filepath.Join(dir, "decision-queue.jsonl"))

	candidate := decisionCandidate{
		ID:           "dec-1",
		SessionID:    "sess-1",
		DetectedAt:   "2026-03-09T00:00:00Z",
		Source:       "hook",
		TriggerKind:  "file_diff",
		Title:        "Changed the data model",
		ChosenOption: "Update schema",
		Context:      "Schema edit",
		ImpactScore:  4,
		Severity:     "medium",
		Reason:       "Important change",
	}

	if err := enqueueDecisionCandidate(candidate); err != nil {
		t.Fatalf("enqueueDecisionCandidate: %v", err)
	}

	items, err := loadDecisionQueue()
	if err != nil {
		t.Fatalf("loadDecisionQueue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Title != candidate.Title {
		t.Fatalf("title = %q", items[0].Title)
	}
}

func TestPersistDecisionCaptureSendsDecisionEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PROMPTSTER_API_URL", "https://promptster.test")
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(dir, "buffer.jsonl"))

	if err := saveSession(Session{
		SessionID:    "sess-live",
		SessionToken: "psk_live",
		StartedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("saveSession: %v", err)
	}

	var capturedBody string
	prevClient := httpClient
	httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			capturedBody = string(body)
			return &http.Response{
				StatusCode: 201,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		}),
	}
	defer func() { httpClient = prevClient }()

	record := decisionCaptureRecord{
		Title:             "Changed queue strategy",
		Context:           "Queue model affects deployment footprint and retry semantics.",
		ImpactScore:       4,
		ChosenOption:      "Use QStash for HTTP push jobs",
		Tradeoffs:         "latency vs simplicity",
		Rationale:         "We chose QStash because it removes Redis operational burden.",
		DecisionID:        "dec-tty",
		SessionID:         "sess-live",
		SourceService:     "hook",
		CapturedVia:       "tui-decide",
		TriggerEventID:    "evt-1",
		TriggerKind:       "file_diff",
		CategoryHint:      "queueing",
		RationalePrompted: true,
	}

	if err := persistDecisionCapture(record); err != nil {
		t.Fatalf("persistDecisionCapture: %v", err)
	}
	if !strings.Contains(capturedBody, `"kind":"decision_event"`) {
		t.Fatalf("expected decision_event payload, got %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"rationalePromptedTTY":true`) {
		t.Fatalf("expected rationalePromptedTTY=true, got %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"categoryHint":"queueing"`) {
		t.Fatalf("expected categoryHint in payload, got %s", capturedBody)
	}
}

func TestMaybeCaptureDecisionFromHookQueuesWithoutInteractiveTTY(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_DECISION_QUEUE_PATH", filepath.Join(dir, "decision-queue.jsonl"))

	prevTTY := openDecisionTTY
	openDecisionTTY = func() (io.ReadWriteCloser, error) { return nil, errors.New("no tty") }
	defer func() { openDecisionTTY = prevTTY }()

	event := newEvent("file_diff", "sess-1")
	event.Data = map[string]interface{}{
		"path": "apps/api/src/routes/sessions.ts",
		"diff": "+ add decisions endpoint",
	}

	maybeCaptureDecisionFromHook(event)

	items, err := loadDecisionQueue()
	if err != nil {
		t.Fatalf("loadDecisionQueue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Title == "" {
		t.Fatalf("expected queued decision title")
	}
}

func TestMaybeCaptureDecisionFromHookAlwaysEnqueues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_DECISION_QUEUE_PATH", filepath.Join(dir, "decision-queue.jsonl"))
	t.Setenv("PROMPTSTER_DECISION_PROMPT_STATE", filepath.Join(dir, "decision-prompt.state"))

	tty := newStubTTY("")
	prevTTY := openDecisionTTY
	openDecisionTTY = func() (io.ReadWriteCloser, error) { return tty, nil }
	defer func() { openDecisionTTY = prevTTY }()

	event := newEvent("file_diff", "sess-1")
	event.Data = map[string]interface{}{
		"path": "apps/api/src/routes/sessions.ts",
		"diff": "+ add decisions endpoint",
	}

	maybeCaptureDecisionFromHook(event)

	items, err := loadDecisionQueue()
	if err != nil {
		t.Fatalf("loadDecisionQueue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	// /explain is optional commentary — enqueue silently, never bug the
	// candidate with a TTY notice.
	if got := tty.writes.String(); got != "" {
		t.Fatalf("expected silent enqueue (no TTY notice), got %q", got)
	}
}
