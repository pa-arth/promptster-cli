package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempHome points globalPromptsterDir() at a scratch HOME so the lock tests
// never touch the developer's real ~/.promptster.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestCleanupLockIsSingleFlight(t *testing.T) {
	withTempHome(t)

	release, ok := acquireCleanupLock()
	if !ok {
		t.Fatal("first acquire must succeed")
	}

	// THE PROPERTY. An expired session fires cleanup from the apiKeyHelper
	// (once per Claude API request), the shell hook (once per prompt) and
	// `promptster codex` — concurrently. The second runner must do nothing:
	// cleanup reverts ~/.codex/config.toml from a state file it then deletes,
	// so a second pass finds no state and silently drops the user's own
	// model_provider.
	if _, ok := acquireCleanupLock(); ok {
		t.Error("a second cleanup acquired the lock while the first still held it")
	}

	release()

	release2, ok := acquireCleanupLock()
	if !ok {
		t.Fatal("acquire must succeed once the previous holder released")
	}
	release2()
}

func TestCleanupLockTakenOverWhenStale(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".promptster", "cleanup.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	// A cleanup killed mid-teardown leaves its lock behind. Refusing forever
	// would mean an abandoned session can never be evicted again.
	stale := time.Now().Add(-cleanupLockStale - time.Minute).Unix()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("99999 %d\n", stale)), 0o600); err != nil {
		t.Fatal(err)
	}

	release, ok := acquireCleanupLock()
	if !ok {
		t.Fatal("a lock older than cleanupLockStale must be taken over")
	}
	release()
}

func TestCleanupLockHeldWhenFresh(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".promptster", "cleanup.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("99999 %d\n", time.Now().Unix())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := acquireCleanupLock(); ok {
		t.Error("a fresh lock must not be taken over")
	}
}

func TestCleanupLockCorruptCountsAsStale(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".promptster", "cleanup.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A corrupt byte must not permanently disable cleanup.
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, ok := acquireCleanupLock()
	if !ok {
		t.Fatal("an unparseable lock must be treated as stale, not as a permanent block")
	}
	release()
}

// TestCleanupLockLivesOutsideTheWorkspace guards the placement, not the logic.
// stateDir() resolves to <workspace>/.promptster while a session is live, and
// cleanup deletes that directory partway through its own run — a lock kept
// there would release itself in the middle of the window it protects.
func TestCleanupLockLivesOutsideTheWorkspace(t *testing.T) {
	home := withTempHome(t)
	ws := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(ws, ".promptster"))

	got := cleanupLockPath()
	if want := filepath.Join(home, ".promptster", "cleanup.lock"); got != want {
		t.Errorf("cleanup lock must live in the global dir\n got: %s\nwant: %s", got, want)
	}
	if got == filepath.Join(stateDir(), "cleanup.lock") {
		t.Error("cleanup lock must not live in stateDir() — cleanup deletes that directory mid-run")
	}
}
