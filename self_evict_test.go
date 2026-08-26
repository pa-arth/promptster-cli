package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TTL self-eviction on a box we own.
//
// openspec private-problem-sandbox-lane design.md §8.6, task 2.5e.
//
// ⛔ The failure this guards against is DESTRUCTIVE and looks like nothing. On a
// session whose ExpiresAt has passed, one interactive shell — a candidate
// clicking TERMINAL — wipes `.claude/settings.local.json`,
// `.promptster/session.json`, `active-workspace`, the codex provider block, the
// shell hook and its RC line. The shell hook backgrounds `promptster env`, which
// fires cleanup; `promptster auth-token` fires it too, and the Claude Code
// apiKeyHelper IS `auth-token` — so the agent triggers its own teardown, once
// per API request.
//
// Correct on a candidate's laptop. Wrong in a box we provisioned.
//
// ⚠ These tests observe WHETHER CLEANUP FIRED, not stdout. "Printed nothing" and
// "printed nothing and wiped the box" are the same stdout and opposite outcomes,
// so a stdout assertion cannot tell the fix from the bug.

// withCleanupSpy swaps fireBackgroundCleanup for a recorder and restores it.
func withCleanupSpy(t *testing.T) *[]string {
	t.Helper()
	var fired []string
	original := fireBackgroundCleanup
	fireBackgroundCleanup = func(reason string) { fired = append(fired, reason) }
	t.Cleanup(func() { fireBackgroundCleanup = original })
	return &fired
}

// runWithSession writes a session into an isolated state dir and runs fn,
// swallowing stdout. Returns what fn printed.
func runWithSession(t *testing.T, session Session, fn func()) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), data, 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	fn()
	w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func expiredSession() Session {
	return Session{
		SessionID:    "sess-1",
		SessionToken: "PST-STALE-TOKEN",
		TaskRoot:     "/tmp/ws",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
	}
}

func TestSelfEvictArmedByDefault(t *testing.T) {
	// The default must stay the laptop default. A session.json written by any
	// older CLI has no `noSelfEvict` field at all, and an abandoned session on
	// someone's own machine must still take our hooks out of their shell.
	var s Session
	if !s.selfEvictArmed() {
		t.Fatal("a zero-value session must have self-eviction ARMED — absent means laptop")
	}
	if (Session{NoSelfEvict: true}).selfEvictArmed() {
		t.Fatal("NoSelfEvict must disarm")
	}
}

func TestExpiredTreatsZeroExpiryAsNotExpired(t *testing.T) {
	// Unknown is not expired. Treating a missing ExpiresAt as stale would evict
	// every legacy session the moment its owner upgraded the CLI.
	if (Session{}).expired() {
		t.Fatal("a zero ExpiresAt must not count as expired")
	}
	if !(Session{ExpiresAt: time.Now().Add(-time.Minute)}).expired() {
		t.Fatal("a past ExpiresAt must count as expired")
	}
}

func TestShellHookEvictsAnExpiredLaptopSession(t *testing.T) {
	// The behaviour that must NOT regress. This is the whole reason the path
	// exists, and a guard that disarmed it everywhere would be a different bug.
	fired := withCleanupSpy(t)
	runWithSession(t, expiredSession(), func() { cmdEnv(nil) })
	if len(*fired) != 1 {
		t.Fatalf("expected the shell hook to evict an expired laptop session, fired=%v", *fired)
	}
}

func TestShellHookDoesNotEvictASeededSession(t *testing.T) {
	fired := withCleanupSpy(t)
	s := expiredSession()
	s.NoSelfEvict = true
	runWithSession(t, s, func() { cmdEnv(nil) })
	if len(*fired) != 0 {
		t.Fatalf("a seeded box must survive its own terminal, fired=%v", *fired)
	}
}

func TestAuthTokenDoesNotEvictASeededSessionButStillWithholdsTheToken(t *testing.T) {
	// ⛔ The two halves are separate decisions and only one is conditional.
	// WITHHOLDING an expired credential is honest degradation — Claude Code reads
	// empty stdout as "no helper credential" and falls back. DESTROYING the box is
	// the other half, and since the apiKeyHelper IS this command, leaving it armed
	// means the agent tears down its own environment once per API request.
	fired := withCleanupSpy(t)
	s := expiredSession()
	s.NoSelfEvict = true
	out := runWithSession(t, s, func() { cmdAuthToken(nil) })

	if len(*fired) != 0 {
		t.Fatalf("apiKeyHelper must not tear down a seeded box, fired=%v", *fired)
	}
	if out != "" {
		t.Fatalf("an expired session must still withhold its token, got %q", out)
	}
}

func TestAuthTokenEvictsAnExpiredLaptopSession(t *testing.T) {
	fired := withCleanupSpy(t)
	out := runWithSession(t, expiredSession(), func() { cmdAuthToken(nil) })
	if len(*fired) != 1 {
		t.Fatalf("expected eviction on a laptop session, fired=%v", *fired)
	}
	if out != "" {
		t.Fatalf("expected no token, got %q", out)
	}
}

func TestLiveSeededSessionStillServesItsToken(t *testing.T) {
	// Disarming eviction must not disarm the credential. A seeded box inside its
	// window is a normal, working session.
	fired := withCleanupSpy(t)
	s := expiredSession()
	s.NoSelfEvict = true
	s.ExpiresAt = time.Now().Add(time.Hour)
	out := runWithSession(t, s, func() { cmdAuthToken(nil) })

	if len(*fired) != 0 {
		t.Fatalf("nothing should fire for a live session, fired=%v", *fired)
	}
	if out != "PST-STALE-TOKEN" {
		t.Fatalf("a live seeded session must serve its token, got %q", out)
	}
}

func TestNoSelfEvictSurvivesAJsonRoundTrip(t *testing.T) {
	// The field only works if it is actually on disk. It is `omitempty`, so the
	// false case writes nothing — which is the point, and also the way a typo in
	// the json tag would go unnoticed.
	data, err := json.Marshal(Session{NoSelfEvict: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"noSelfEvict":true`)) {
		t.Fatalf("noSelfEvict did not serialise under the expected key: %s", data)
	}

	var back Session
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.selfEvictArmed() {
		t.Fatal("noSelfEvict did not survive the round trip")
	}

	plain, _ := json.Marshal(Session{})
	if bytes.Contains(plain, []byte("noSelfEvict")) {
		t.Fatalf("a laptop session must not write the field at all: %s", plain)
	}
}
