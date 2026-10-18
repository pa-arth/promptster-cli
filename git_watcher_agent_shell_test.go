package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Claude Bash call (`pnpm install`, a codegen script) writes files no Edit
// hook claims. The lockfile must not be emitted at all, and the other writes
// belong to the agent, not the candidate.
func TestGitWatcherCreditsAgentShellWrites(t *testing.T) {
	root, _ := initTestRepo(t)
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	t.Setenv("PROMPTSTER_API_URL", server.URL)
	buffer := filepath.Join(stateDir(), "buffer.jsonl")
	t.Setenv("PROMPTSTER_BUFFER_PATH", buffer)
	session := Session{SessionID: "shell-writes", SessionToken: "test", TaskRoot: root}
	saveCurrentTreeHash(root)

	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	poll := func() string {
		t.Helper()
		_ = os.Remove(buffer)
		if _, err := pollGitDiffs(session, server.Client()); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(buffer)
		return string(data)
	}

	normalizeClaudeCode(map[string]interface{}{"hook_event_name": "PreToolUse", "tool_name": "Bash"}, session.SessionID)
	write("pnpm-lock.yaml", strings.Repeat("lock: 1\n", 500))
	write("generated.ts", "export const x = 1;\n")
	got := poll()
	if strings.Contains(got, "pnpm-lock.yaml") {
		t.Fatalf("lockfile emitted as a file_diff:\n%s", got)
	}
	if !strings.Contains(got, "generated.ts") || !strings.Contains(got, `"attribution":"likely_ai"`) || strings.Contains(got, "likely_human") {
		t.Fatalf("shell write not credited to the agent:\n%s", got)
	}

	// Outside the window a working-tree change is still the candidate's.
	if err := os.Remove(agentShellStatePath()); err != nil {
		t.Fatal(err)
	}
	write("generated.ts", "export const x = 2;\n")
	if got := poll(); !strings.Contains(got, `"actor":{"type":"human"`) {
		t.Fatalf("manual edit not credited to the candidate:\n%s", got)
	}
}
