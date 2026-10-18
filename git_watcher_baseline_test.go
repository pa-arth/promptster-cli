package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Session c9cb72a6 had five file_diff events: three Claude edits and these
// two setup writes stamped likely_human by git-poll (40% manual, 3% of lines).
func TestGitWatcherBaselineIncludesHostedSetup(t *testing.T) {
	for _, intentToAdd := range []bool{false, true} {
		name := "untracked"
		if intentToAdd {
			name = "intent-to-add"
		}
		t.Run(name, func(t *testing.T) {
			root, _ := initTestRepo(t)
			t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return string(out)
			}
			write := func(path, text string) {
				t.Helper()
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The problem's tracked ignore file, followed by provisioning writes.
			write(".gitignore", ".venv/\n*.egg-info/\n__pycache__/\n")
			git("add", ".gitignore")
			git("commit", "-m", "problem ignore rules")
			write(".gitignore", ".venv/\n*.egg-info/\n__pycache__/\n\n.promptster/\n")
			write(".vscode/settings.json", "{\n  \"python.defaultInterpreterPath\": \"/home/user/task/.venv/bin/python\",\n  \"python.terminal.activateEnvironment\": true\n}")
			write("TASK.md", "Hosted assessment brief\n")
			if intentToAdd {
				git("add", "-N", ".vscode/settings.json")
			}
			indexBefore := git("ls-files", "--stage")
			saveCurrentTreeHash(root)
			if got := git("ls-files", "--stage"); got != indexBefore {
				t.Fatal("baseline mutated the candidate index")
			}
			// A real poll must also ignore setup before snapshot capture stages it.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
			defer server.Close()
			t.Setenv("PROMPTSTER_API_URL", server.URL)
			t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir(), "buffer.jsonl"))
			session := Session{SessionID: "c9cb72a6", SessionToken: "test", TaskRoot: root}
			if n, err := pollGitDiffs(session, server.Client()); err != nil || n != 0 {
				t.Fatalf("setup poll: sent %d, error %v", n, err)
			}
			// Snapshot capture exposes untracked setup files to git diff later.
			git("add", "-N", ".vscode/settings.json")
			args := append([]string{"diff", loadLastTreeHash()}, gitExcludePathspecs(root)...)
			if got := git(args...); got != "" {
				t.Fatalf("setup emitted as candidate changes:\n%s", got)
			}

			// Preserve genuine edits to the same paths rather than excluding configs.
			write(".vscode/settings.json", "{\"editor.tabSize\": 2}\n")
			write(".gitignore", ".venv/\n*.egg-info/\n__pycache__/\n\n.promptster/\n*.log\n")
			diff := git(args...)
			if !strings.Contains(diff, "+*.log") || !strings.Contains(diff, "+{\"editor.tabSize\": 2}") {
				t.Fatalf("lost manual edits:\n%s", diff)
			}
			if n, err := pollGitDiffs(session, server.Client()); err != nil || n != 2 {
				t.Fatalf("manual poll: sent %d, error %v", n, err)
			}
			if n, err := pollGitDiffs(session, server.Client()); err != nil || n != 0 {
				t.Fatalf("repeated poll: sent %d, error %v", n, err)
			}
			// An intent-to-add entry must not reset the baseline to HEAD every tick.
			saveCurrentTreeHash(root)
			args = append([]string{"diff", loadLastTreeHash()}, gitExcludePathspecs(root)...)
			if got := git(args...); got != "" {
				t.Fatalf("poll repeats old changes:\n%s", got)
			}
		})
	}
}
