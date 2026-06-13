package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Lane identity: every Claude Code hook event must carry the per-process
// session_id + cwd in data.meta, so concurrent sessions in one workspace are
// distinguishable downstream (worker parallelism signals).

func laneMeta(t *testing.T, e Event) map[string]interface{} {
	t.Helper()
	data, ok := e.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("data is not a map: %+v", e.Data)
	}
	meta, _ := data["meta"].(map[string]interface{})
	if meta == nil {
		t.Fatalf("no meta on event %s: %+v", e.Kind, data)
	}
	return meta
}

func TestClaudeHookToolEventsCarryLaneMeta(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"session_id":      "lane-uuid-1",
		"cwd":             "/ws/.claude/worktrees/fix",
		"tool_name":       "Edit",
		"tool_input": map[string]interface{}{
			"file_path":  "/ws/.claude/worktrees/fix/src/app.ts",
			"old_string": "a",
			"new_string": "b",
		},
	}
	e, ok := normalize(payload, "sess-1")
	if !ok {
		t.Fatal("normalize rejected PostToolUse payload")
	}
	meta := laneMeta(t, e)
	if meta["ideSessionId"] != "lane-uuid-1" {
		t.Errorf("ideSessionId = %v", meta["ideSessionId"])
	}
	if meta["cwd"] != "/ws/.claude/worktrees/fix" {
		t.Errorf("cwd = %v", meta["cwd"])
	}
}

func TestClaudeHookPromptKeepsExistingLaneMeta(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "lane-uuid-2",
		"cwd":             "/ws",
		"prompt":          "fix the failing parser test",
	}
	e, ok := normalize(payload, "sess-1")
	if !ok {
		t.Fatal("normalize rejected UserPromptSubmit payload")
	}
	meta := laneMeta(t, e)
	if meta["ideSessionId"] != "lane-uuid-2" {
		t.Errorf("ideSessionId = %v", meta["ideSessionId"])
	}
	if meta["cwd"] != "/ws" {
		t.Errorf("cwd = %v", meta["cwd"])
	}
}

func TestClaudeTranscriptEventsCarryLaneMeta(t *testing.T) {
	p := newClaudeTranscriptProcessor("sess-1", false)
	events := processAll(t, p,
		`{"type":"user","sessionId":"lane-uuid-3","cwd":"/ws","timestamp":"2026-06-12T10:00:00Z","message":{"role":"user","content":"add cursor pagination to the export endpoint"}}`,
		`{"type":"assistant","timestamp":"2026-06-12T10:00:05Z","message":{"id":"msg_1","model":"claude-sonnet-4-6","content":[{"type":"text","text":"On it."}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","sessionId":"lane-uuid-3","cwd":"/ws","timestamp":"2026-06-12T10:00:10Z","message":{"role":"user","content":"now stream the response"}}`,
	)
	if len(events) < 2 {
		t.Fatalf("expected >=2 events, got %d", len(events))
	}
	for _, e := range events {
		meta := laneMeta(t, e)
		if meta["ideSessionId"] != "lane-uuid-3" {
			t.Errorf("%s: ideSessionId = %v", e.Kind, meta["ideSessionId"])
		}
	}
}

func TestClassifyMatchesRegisteredWorktree(t *testing.T) {
	tmp := t.TempDir()
	ws := resolvePath(filepath.Join(tmp, "ws"))
	wt := resolvePath(filepath.Join(tmp, "fix-wt"))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	transcript := filepath.Join(tmp, "wt-session.jsonl")
	line := fmt.Sprintf(`{"type":"user","cwd":"%s","timestamp":"2026-06-12T10:00:00Z","message":{"content":"hi"}}`, wt)
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sibling worktree dir is NOT under the workspace: workspace-only roots
	// must miss it, workspace+worktree roots must match it.
	if got := classifyClaudeTranscript(transcript, []string{ws}, cutoff); got != claudeMatchNo {
		t.Fatalf("workspace-only: got %v, want no", got)
	}
	if got := classifyClaudeTranscript(transcript, []string{ws, wt}, cutoff); got != claudeMatchYes {
		t.Fatalf("with worktree root: got %v, want yes", got)
	}
}

func TestWorkspaceMatchRootsIncludesWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "init")
	wt := filepath.Join(tmp, "sibling-wt")
	run("worktree", "add", "-q", wt)

	roots := workspaceMatchRoots(resolvePath(ws))
	found := false
	for _, r := range roots {
		if r == resolvePath(wt) {
			found = true
		}
	}
	if !found {
		t.Fatalf("worktree %s not in roots %v", wt, roots)
	}
}
