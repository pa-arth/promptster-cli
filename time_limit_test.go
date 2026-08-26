package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// ── The announcement, and the markers that must not outlive the session ──────
//
// Detection moved into `promptster diff-watch`, whose stdout and stderr are both
// git-watcher.log. So on what is now the ORDINARY detection path, the banner and
// everything `done` prints land in a file nobody opens. The notice below is the
// durable channel that carries it to the next hook, which does own a surface the
// candidate reads.

func TestExpiryNoticeSurvivesSessionCleanupAndDrainsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", state)
	t.Setenv("PROMPTSTER_APP_URL", "http://app.test")

	writeExpiryNotice(Session{SessionID: "sess-1", Key: "PST-K"})

	// THE POINT OF THE GLOBAL DIR: `done` wipes the workspace state dir, and the
	// notice has to be readable after that or the candidate is told nothing.
	cleanupPromptsterState(state)
	if _, err := os.Stat(expiryNoticePath()); err != nil {
		t.Fatalf("notice did not survive cleanupPromptsterState: %v", err)
	}

	got := captureStderr(t, drainExpiryNotice)
	if !strings.Contains(got, "Time limit reached") {
		t.Fatalf("drain printed no announcement; got %q", got)
	}
	if !strings.Contains(got, "http://app.test/replay/sess-1?key=PST-K") {
		t.Fatalf("drain omitted the results URL; got %q", got)
	}

	// Once. A second hook must say nothing.
	if again := captureStderr(t, drainExpiryNotice); again != "" {
		t.Fatalf("second drain printed %q; the notice is one-shot", again)
	}
}

func TestCleanupRemovesExpiryMarkers(t *testing.T) {
	// stateDir() is WORKSPACE-scoped. A marker left behind makes the NEXT
	// assessment taken in the same directory return early from the expiry branch
	// and never auto-submit — the exact bug the marker was added to prevent.
	state := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", state)
	t.Setenv("HOME", t.TempDir())

	markAutoSubmitted()
	markPreDeadlineSnapshotted()
	markThresholdWarned(5)

	cleanupPromptsterState(state)

	if alreadyAutoSubmitted() {
		t.Fatal("time-auto-submitted survived cleanup — the next assessment would never auto-submit")
	}
	if alreadyPreDeadlineSnapshotted() {
		t.Fatal("pre-deadline-snapshot survived cleanup")
	}
	if alreadyWarnedForThreshold(5) {
		t.Fatal("threshold marker survived cleanup")
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what it
// wrote. The drain writes to stderr because that is the stream a hook's caller
// surfaces to the candidate.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out)
}

// THE WATCHER MUST NOT EAT ITS OWN MESSAGE.
//
// Found by running the real daemon: the watcher wrote the notice, kept polling,
// and one second later drained it into git-watcher.log — the very file the
// notice exists to escape. The hook then found nothing and the candidate was
// told nothing, which is the original defect wearing a new hat. A drain is a
// DELIVERY, so only a caller whose stderr the candidate reads may perform one.
func TestWatcherDoesNotDrainItsOwnNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	writeExpiryNotice(Session{SessionID: "sess-1", Key: "PST-K"})

	// The watcher polls again with no session on disk (`done` deleted it). Its
	// drain must be a no-op.
	if out := captureStderr(t, func() { checkTimeLimitFrom(surfaceLogFile) }); out != "" {
		t.Fatalf("watcher printed %q — that message belongs to the next hook", out)
	}
	if _, err := os.Stat(expiryNoticePath()); err != nil {
		t.Fatal("watcher consumed the notice; the hook will have nothing to surface")
	}

	// A hook, which does own a visible stream, delivers it.
	if out := captureStderr(t, func() { checkTimeLimitFrom(surfaceVisible) }); !strings.Contains(out, "Time limit reached") {
		t.Fatalf("hook did not deliver the notice; got %q", out)
	}
	if _, err := os.Stat(expiryNoticePath()); err == nil {
		t.Fatal("notice survived delivery — the next hook would repeat it")
	}
}
