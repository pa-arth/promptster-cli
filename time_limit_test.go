package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// The expiry handler used to record nothing about having run, so every hook
// after the deadline spawned another `promptster done` — concurrently, since the
// spawn does not wait. These cover the marker, the deadline-aware wake, and the
// one refusal the client must NOT report as a failure.

func withStateDir(t *testing.T) {
	t.Helper()
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
}

func TestAutoSubmitMarkerFiresOnce(t *testing.T) {
	withStateDir(t)

	if alreadyAutoSubmitted() {
		t.Fatal("fresh state dir should carry no expiry marker")
	}
	markAutoSubmitted()
	if !alreadyAutoSubmitted() {
		t.Fatal("marker written but not observed — every later hook would re-spawn `done`")
	}
}

func TestPreDeadlineSnapshotMarkerFiresOnce(t *testing.T) {
	withStateDir(t)

	if alreadyPreDeadlineSnapshotted() {
		t.Fatal("fresh state dir should carry no pre-deadline snapshot marker")
	}
	markPreDeadlineSnapshotted()
	if !alreadyPreDeadlineSnapshotted() {
		t.Fatal("marker written but not observed")
	}
}

func TestSessionDeadline(t *testing.T) {
	start := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	if _, ok := sessionDeadline(Session{StartedAt: start}); ok {
		t.Fatal("no time limit means no deadline")
	}
	if _, ok := sessionDeadline(Session{TimeLimitMinutes: 60}); ok {
		t.Fatal("no start time means no deadline")
	}
	got, ok := sessionDeadline(Session{StartedAt: start, TimeLimitMinutes: 60})
	if !ok || !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("deadline = %v, ok = %v; want %v", got, ok, start.Add(time.Hour))
	}
}

// The whole point of moving the clock into the watcher: the loop must wake ON
// the deadline, not up to a full poll interval past it.
func TestNextWakeInShortensToTheDeadline(t *testing.T) {
	now := time.Now()

	noLimit := Session{StartedAt: now}
	if got := nextWakeIn(noLimit); got != gitWatchInterval {
		t.Fatalf("no deadline should poll normally: got %v, want %v", got, gitWatchInterval)
	}

	// Deadline 10s out: wake for it, not in 60s.
	soon := Session{StartedAt: now.Add(-time.Hour + 10*time.Second), TimeLimitMinutes: 60}
	if got := nextWakeIn(soon); got > 11*time.Second {
		t.Fatalf("wake %v overshoots a deadline 10s away", got)
	}

	// Deadline far out: ordinary interval, no busy-loop.
	far := Session{StartedAt: now, TimeLimitMinutes: 600}
	if got := nextWakeIn(far); got != gitWatchInterval {
		t.Fatalf("distant deadline should poll normally: got %v", got)
	}

	// Already past: never busy-loop.
	past := Session{StartedAt: now.Add(-2 * time.Hour), TimeLimitMinutes: 60}
	if got := nextWakeIn(past); got < time.Second {
		t.Fatalf("wake %v would busy-loop", got)
	}
}

func TestApiErrorCode(t *testing.T) {
	if got := apiErrorCode([]byte(`{"error":"Invalid or expired key","code":"assessment_closed"}`)); got != "assessment_closed" {
		t.Fatalf("code = %q", got)
	}
	// An older server sends no code; that must read as "unknown", not as closed.
	if got := apiErrorCode([]byte(`{"error":"Invalid or expired key"}`)); got != "" {
		t.Fatalf("missing code should be empty, got %q", got)
	}
	if got := apiErrorCode([]byte("not json")); got != "" {
		t.Fatalf("unparseable body should be empty, got %q", got)
	}
}

// A refusal that names the assessment as already complete is the ordinary end of
// a timed assessment. `apiComplete` returns the sentinel AND the completedAt, so
// the caller can print the submitted box instead of "your work was not saved".
func TestApiCompleteTreatsAlreadyClosedAsSubmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error":       "Session is already completed",
			"code":        "assessment_closed",
			"completedAt": "2026-08-26T12:00:00.000Z",
		})
	}))
	defer srv.Close()
	os.Setenv("PROMPTSTER_API_URL", srv.URL)
	defer os.Unsetenv("PROMPTSTER_API_URL")

	resp, err := apiComplete("sess-1", "PST-TEST")
	if err == nil {
		t.Fatal("expected the closed sentinel so callers can branch on it")
	}
	if !isAssessmentClosed(err) {
		t.Fatalf("err = %v; want errAssessmentClosed", err)
	}
	if !resp.AlreadyComplete {
		t.Fatal("response should report the assessment as already complete")
	}
	if resp.CompletedAt != "2026-08-26T12:00:00.000Z" {
		t.Fatalf("completedAt = %q — the submitted box would print a blank line", resp.CompletedAt)
	}
}

// Any other refusal still means the work did not reach us, and must stay fatal.
func TestApiCompleteKeepsOtherRefusalsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "sessionId is required"}) //nolint:errcheck
	}))
	defer srv.Close()
	os.Setenv("PROMPTSTER_API_URL", srv.URL)
	defer os.Unsetenv("PROMPTSTER_API_URL")

	_, err := apiComplete("", "PST-TEST")
	if err == nil {
		t.Fatal("expected an error")
	}
	if isAssessmentClosed(err) {
		t.Fatal("a validation failure must not be reported to the candidate as submitted")
	}
}
