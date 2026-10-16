package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fileDiffEvent builds a minimal file_diff event for dedup tests.
func fileDiffEvent(path, diff string) Event {
	e := newEvent("file_diff", "sess-1")
	e.Data = map[string]interface{}{"path": path, "diff": diff}
	return e
}

func TestDedupeFileDiffAcrossChannels(t *testing.T) {
	// Isolate the ledger under a temp state dir.
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Channel 1 (AI) emits first → wins.
	ai := fileDiffEvent("a.txt", "@@\n+hello world")
	if !dedupeFileDiff(ws, &ai) {
		t.Fatal("first emission should be allowed")
	}

	// Channel 2 (git watcher) sees the SAME resulting content → deduped.
	git := fileDiffEvent("a.txt", "diff --git a/a.txt b/a.txt\n+hello world")
	if dedupeFileDiff(ws, &git) {
		t.Error("second emission of identical content should be deduped")
	}

	// A genuine human edit changes the content → NOT deduped (override signal).
	if err := os.WriteFile(target, []byte("hello world EDITED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	human := fileDiffEvent("a.txt", "diff --git a/a.txt b/a.txt\n+EDITED")
	if !dedupeFileDiff(ws, &human) {
		t.Error("a new resulting content must be emitted, not deduped")
	}
}

func TestDedupeIgnoresNonFileDiff(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)
	cmd := newEvent("command", "sess-1")
	cmd.Data = map[string]interface{}{"command": "ls"}
	if !dedupeFileDiff("/ws", &cmd) {
		t.Error("non-file_diff events must always pass through")
	}
}

// Regression: the git watcher re-diffs unchanged AI files on every poll when
// its baseline can't advance. An aged claim must still dedupe, or that AI work
// leaks through as ai_revised_by_human once it's old enough.
func TestDedupeHoldsAfterClaimAges(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("ai wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ai := fileDiffEvent("a.txt", "+ai wrote this")
	if !dedupeFileDiff(ws, &ai) {
		t.Fatal("first emission should be allowed")
	}

	// Age the claim an hour into the past.
	ledger := map[string]diffDedupEntry{}
	data, _ := os.ReadFile(diffDedupLedgerPath())
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	key := "sess-1\x00a.txt"
	e := ledger[key]
	e.TsMs = time.Now().Add(-time.Hour).UnixMilli()
	ledger[key] = e
	data, _ = json.Marshal(ledger)
	if err := os.WriteFile(diffDedupLedgerPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}

	git := fileDiffEvent("a.txt", "diff --git a/a.txt b/a.txt\n+ai wrote this")
	if dedupeFileDiff(ws, &git) {
		t.Error("unchanged content must stay deduped however old the claim is")
	}
}

// Without a TTL, claims must not outlive their session: a reused workspace
// whose file returns to a previously-claimed content is new work this session.
func TestDedupeDoesNotCarryAcrossSessions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(tmp, "buffer.jsonl"))
	t.Setenv("PROMPTSTER_STATE_DIR", tmp)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := fileDiffEvent("a.txt", "+same")
	if !dedupeFileDiff(ws, &first) {
		t.Fatal("first emission should be allowed")
	}
	next := newEvent("file_diff", "sess-2")
	next.Data = map[string]interface{}{"path": "a.txt", "diff": "+same"}
	if !dedupeFileDiff(ws, &next) {
		t.Error("a new session must not inherit the previous session's claim")
	}
}
