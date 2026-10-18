package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceGitExcludes(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write(".gitignore", "node_modules/\n")
	write("app.go", "before\n")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "Initial state")
	ensureWorkspaceGitExcludes(root)
	ensureWorkspaceGitExcludes(root)
	write(".promptster/session.json", "{}")
	write(".claude/settings.local.json", "{}")
	write(".claude/commands/explain.md", "platform")
	write("TASK.md", "brief")
	if status := git("status", "--porcelain"); status != "" {
		t.Fatalf("platform changes: %s", status)
	}
	write("app.go", "candidate\n")
	write("docs/spec.md", "candidate spec\n")
	write(".claude/agent.md", "candidate agent\n")
	write("nested/TASK.md", "candidate brief\n")
	args := append([]string{"add", "-A"}, gitAddExcludePathspecs(root)...)
	git(args...)
	if diff := git("diff", "HEAD", "--name-only"); diff != ".claude/agent.md\napp.go\ndocs/spec.md\nnested/TASK.md" {
		t.Fatalf("candidate diff = %q", diff)
	}
	files := git("ls-files")
	if files != ".claude/agent.md\n.gitignore\napp.go\ndocs/spec.md\nnested/TASK.md" {
		t.Fatalf("bundle files = %q", files)
	}
}
