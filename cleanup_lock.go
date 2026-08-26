package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cleanupLockStale is how long a held lock is trusted before another runner may
// take it over. A cleanup is seconds of local file work; a lock older than this
// belongs to a process that was killed mid-teardown, and refusing forever would
// mean an abandoned session could never be evicted again.
const cleanupLockStale = 2 * time.Minute

// cleanupLockPath lives in the GLOBAL dir, never stateDir(). stateDir() resolves
// to <workspace>/.promptster while a session is live, and cleanup deletes that
// directory partway through its own run — a lock kept there would release
// itself in the middle of the window it exists to protect.
func cleanupLockPath() string {
	return filepath.Join(globalPromptsterDir(), "cleanup.lock")
}

// acquireCleanupLock makes cleanup single-flight across processes. It returns a
// release func and true on success; false means another cleanup is already
// running and this caller must do nothing.
//
// WHY THIS EXISTS. Every caller of fireBackgroundCleanup is on a hot path:
// Claude Code runs the `auth-token` apiKeyHelper once per API request, the
// shell hook runs the same command once per prompt, and `promptster codex`
// runs it on launch. All three fire cleanup when ExpiresAt has passed, so the
// moment a session expires they race — and cleanup is NOT safe to run twice at
// once. revertCodexProxy() restores the user's own `model_provider` from a
// state file it then deletes; a second runner that reads after that delete
// finds no state, restores nothing, and silently leaves the user's personal
// codex pointed at a dead provider. That is the exact failure the state file
// was written to prevent.
//
// It is also the one place the two rails can break each other: cleanup tears
// down BOTH <workspace>/.claude/settings.local.json and the ~/.codex block, so
// a storm triggered by the codex path takes the Claude rail with it.
func acquireCleanupLock() (release func(), ok bool) {
	path := cleanupLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		// Can't create the lock dir — proceed unguarded rather than make an
		// unwritable home mean a session can never be cleaned up.
		return func() {}, true
	}

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d %d\n", os.Getpid(), time.Now().Unix())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		if !os.IsExist(err) {
			return func() {}, true // unexpected FS error: don't block teardown
		}
		if attempt > 0 || !cleanupLockIsStale(path) {
			return nil, false
		}
		// Stale: drop it and take one more run at O_EXCL. If someone else wins
		// that race, the next iteration sees a fresh lock and yields.
		_ = os.Remove(path)
	}
	return nil, false
}

// cleanupLockIsStale reports whether the lock's recorded start time is older
// than cleanupLockStale. An unparseable or unreadable lock counts as stale —
// the alternative is a corrupt byte permanently disabling cleanup.
func cleanupLockIsStale(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return true
	}
	started, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(started, 0)) > cleanupLockStale
}
