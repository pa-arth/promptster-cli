package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A watcher resolves its session, its workspace root and its start cutoff ONCE
// at boot and never again. Its state file, by contrast, is resolved per write
// through ~/.promptster/active-workspace. So the moment a new `start` moves that
// pointer, the PREVIOUS session's watcher begins heartbeating into the NEW
// session's state file — the liveness check passes, no watcher is launched for
// the new session, and the one that is running matches every rollout against the
// old workspace and classifies it "no match".
//
// The result is silent: the candidate works for hours in a correctly set up
// workspace and the reviewer sees an empty session. These tests pin the
// ownership rule that makes "is a watcher running" answerable as "is a watcher
// running FOR THIS SESSION".

func writeCodexWatcherStateFile(t *testing.T, dir string, s codexWatcherState) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-watcher.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCodexWatcherOwnership(t *testing.T) {
	cur := Session{SessionID: "sess-new"}

	t.Run("a watcher booted for this session is ours", func(t *testing.T) {
		if !codexWatcherOwns(codexWatcherState{PID: 1, SessionID: "sess-new"}, cur) {
			t.Error("watcher stamped with this session was not recognised")
		}
	})

	t.Run("a watcher from the previous session is not", func(t *testing.T) {
		// THE BUG. This watcher is alive, heartbeating into this session's state
		// file, and matching rollouts against the previous workspace.
		if codexWatcherOwns(codexWatcherState{PID: 1, SessionID: "sess-old", Workspace: "/old/ws"}, cur) {
			t.Error("a previous session's watcher was accepted as this session's")
		}
	})

	t.Run("an unstamped watcher is foreign", func(t *testing.T) {
		// Written by CLI ≤1.9, which recorded no session. It cannot be shown to
		// belong here, and the cost of being wrong is a restart — against a
		// session that captures nothing.
		if codexWatcherOwns(codexWatcherState{PID: 1}, cur) {
			t.Error("an unstamped watcher was assumed to be ours")
		}
	})
}

func TestClaudeAndGitWatcherOwnership(t *testing.T) {
	cur := Session{SessionID: "sess-new"}
	if claudeWatcherOwns(claudeWatcherState{PID: 1, SessionID: "sess-old"}, cur) {
		t.Error("claude: a previous session's watcher was accepted")
	}
	if !claudeWatcherOwns(claudeWatcherState{PID: 1, SessionID: "sess-new"}, cur) {
		t.Error("claude: this session's watcher was not recognised")
	}
	if claudeWatcherOwns(claudeWatcherState{PID: 1}, cur) {
		t.Error("claude: an unstamped watcher was assumed to be ours")
	}
	if gitWatcherOwns(gitWatcherState{PID: 1, SessionID: "sess-old"}, cur) {
		t.Error("git: a previous session's watcher was accepted")
	}
	if !gitWatcherOwns(gitWatcherState{PID: 1, SessionID: "sess-new"}, cur) {
		t.Error("git: this session's watcher was not recognised")
	}
	if gitWatcherOwns(gitWatcherState{PID: 1}, cur) {
		t.Error("git: an unstamped watcher was assumed to be ours")
	}
}

// A watcher standing down because a new session took over must not delete the
// INCOMING watcher's state — and especially not its progress file, whose byte
// offsets are the only thing preventing a full re-scan and re-ingest.
func TestReleaseWatcherStateOnlyClearsOurOwn(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PROMPTSTER_STATE_DIR", dir)
		writeCodexWatcherStateFile(t, dir, codexWatcherState{PID: 4242, SessionID: "sess-new"})
		progress := filepath.Join(dir, "codex-watcher-progress.json")
		if err := os.WriteFile(progress, []byte(`{"Offsets":{"a":10}}`), 0o644); err != nil {
			t.Fatal(err)
		}

		// The OUTGOING watcher (a different pid) exits.
		releaseCodexWatcherState(1111)

		if _, err := os.Stat(filepath.Join(dir, "codex-watcher.json")); err != nil {
			t.Errorf("outgoing watcher deleted the incoming watcher's state: %v", err)
		}
		if _, err := os.Stat(progress); err != nil {
			t.Errorf("outgoing watcher deleted the incoming watcher's progress: %v", err)
		}

		// And the owning watcher's own exit does clean up.
		releaseCodexWatcherState(4242)
		if _, err := os.Stat(filepath.Join(dir, "codex-watcher.json")); !os.IsNotExist(err) {
			t.Errorf("owning watcher did not clear its own state, stat err = %v", err)
		}
	})

	t.Run("git", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PROMPTSTER_STATE_DIR", dir)
		data, _ := json.Marshal(gitWatcherState{PID: 4242, SessionID: "sess-new"})
		if err := os.WriteFile(filepath.Join(dir, "git-watcher.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		releaseGitWatcherState(1111)
		if _, err := os.Stat(filepath.Join(dir, "git-watcher.json")); err != nil {
			t.Errorf("outgoing git watcher deleted the incoming watcher's state: %v", err)
		}
	})
}

// The scenario end to end, at the level the ensure* functions decide it: a live
// watcher from a previous session must not satisfy the launch check for a new
// one.
func TestLiveWatcherFromPreviousSessionDoesNotSatisfyTheNewOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	// The stale watcher: alive (this test process), stamped for the old session,
	// heartbeating into the NEW session's state dir — exactly what was found on
	// the reporting machine.
	writeCodexWatcherStateFile(t, dir, codexWatcherState{
		PID:           os.Getpid(),
		SessionID:     "sess-previous",
		Workspace:     "/Users/x/promptster-test",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano),
	})

	state, alive := isCodexWatcherRunning()
	if !alive {
		t.Fatal("fixture broken: the stale watcher should read as alive")
	}
	// Liveness alone said "a watcher is running, do nothing" — and that is how an
	// entire assessment captured zero events.
	if codexWatcherOwns(state, Session{SessionID: "sess-current", TaskRoot: "/Users/x/promptster-test-2"}) {
		t.Error("a live watcher from the previous session satisfied the new session's check")
	}
}
