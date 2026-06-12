package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCodexProgress(t *testing.T, match map[string]string) {
	t.Helper()
	p := codexWatchProgress{Offsets: map[string]int64{}, Match: match}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexWatchProgressPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRolloutActiveSince(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	rollout := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh write on a workspace-matched rollout → active.
	writeCodexProgress(t, map[string]string{rollout: "yes"})
	if !codexRolloutActiveSince(time.Now().Add(-time.Minute)) {
		t.Error("freshly written matched rollout should report active")
	}

	// Same file but written before the window → inactive.
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(rollout, old, old); err != nil {
		t.Fatal(err)
	}
	if codexRolloutActiveSince(time.Now().Add(-time.Minute)) {
		t.Error("stale rollout should not report active")
	}

	// A non-matched rollout never counts, however fresh.
	otherRollout := filepath.Join(t.TempDir(), "rollout-other.jsonl")
	if err := os.WriteFile(otherRollout, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCodexProgress(t, map[string]string{otherRollout: "no"})
	if codexRolloutActiveSince(time.Now().Add(-time.Minute)) {
		t.Error("rollout matched 'no' must not report active")
	}

	// No progress file at all → inactive (Claude-only sessions).
	if err := os.Remove(codexWatchProgressPath()); err != nil {
		t.Fatal(err)
	}
	if codexRolloutActiveSince(time.Now().Add(-time.Minute)) {
		t.Error("missing progress file should report inactive")
	}
}
