package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t.local"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func gitCommitFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "--no-verify", "-m", "c"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, err := runCommand(dir, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:40])
}

// The trial-2 failure: an agent makes a linked worktree and commits there, so
// `git -C taskRoot diff` sees nothing.
func TestDetectStrandedWork_LinkedWorktree(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	base := gitCommitFile(t, root, "a.txt", "base\n")

	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	cmd := exec.Command("git", "worktree", "add", "-q", "-b", "feat/x", wt)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	gitCommitFile(t, wt, "b.txt", "real work\n")

	found := detectStrandedWork(root, base)
	if len(found) != 1 {
		t.Fatalf("expected 1 stranded checkout, got %d: %+v", len(found), found)
	}
	if found[0].Kind != "linked worktree" {
		t.Errorf("kind = %q, want linked worktree", found[0].Kind)
	}
	if found[0].Ahead != 1 {
		t.Errorf("ahead = %d, want 1", found[0].Ahead)
	}
	if found[0].Branch != "feat/x" {
		t.Errorf("branch = %q, want feat/x", found[0].Branch)
	}
}

// The setupInstructions failure: the candidate re-clones inside the workspace
// and works in the copy.
func TestDetectStrandedWork_NestedClone(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	base := gitCommitFile(t, root, "a.txt", "base\n")

	// The real trap: setupInstructions say `git clone <url> && cd <repo> && …`,
	// so the candidate ends up with a second clone OF THE SAME REPO nested in the
	// workspace. The base commit is therefore reachable from it.
	nested := filepath.Join(root, "acquiremock")
	clone := exec.Command("git", "clone", "-q", root, nested)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"config", "user.email", "t@t.local"}, {"config", "user.name", "t"}, {"config", "commit.gpgsign", "false"}} {
		c := exec.Command("git", args...)
		c.Dir = nested
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCommitFile(t, nested, "solution.py", "print('work')\n")

	found := detectStrandedWork(root, base)
	if len(found) != 1 {
		t.Fatalf("expected 1 stranded checkout, got %d: %+v", len(found), found)
	}
	if found[0].Kind != "nested clone" {
		t.Errorf("kind = %q, want nested clone", found[0].Kind)
	}
	if found[0].Ahead != 1 {
		t.Errorf("ahead = %d, want 1", found[0].Ahead)
	}
}

// Uncommitted work in a nested checkout still counts — candidates do not always
// commit before running done.
func TestDetectStrandedWork_NestedDirtyOnly(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	base := gitCommitFile(t, root, "a.txt", "base\n")

	nested := filepath.Join(root, "copy")
	gitInit(t, nested)
	gitCommitFile(t, nested, "seed.txt", "x\n")
	if err := os.WriteFile(filepath.Join(nested, "uncommitted.py"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	found := detectStrandedWork(root, base)
	if len(found) != 1 || found[0].Dirty == 0 {
		t.Fatalf("expected dirty nested checkout, got %+v", found)
	}
}

// The normal case must stay silent, or the guard cries wolf on every submission.
func TestDetectStrandedWork_CleanWorkspaceFindsNothing(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	base := gitCommitFile(t, root, "a.txt", "base\n")
	gitCommitFile(t, root, "b.txt", "work done in the right place\n")

	if found := detectStrandedWork(root, base); len(found) != 0 {
		t.Fatalf("expected nothing, got %+v", found)
	}
}

// Vendored repos under node_modules are not the candidate's work.
func TestDetectStrandedWork_SkipsVendorDirs(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)
	base := gitCommitFile(t, root, "a.txt", "base\n")

	vendored := filepath.Join(root, "node_modules", "some-dep")
	gitInit(t, vendored)
	gitCommitFile(t, vendored, "index.js", "module.exports = 1\n")

	if found := detectStrandedWork(root, base); len(found) != 0 {
		t.Fatalf("expected vendor dirs to be skipped, got %+v", found)
	}
}

func TestDetectStrandedWork_NonRepoIsSafe(t *testing.T) {
	if found := detectStrandedWork(t.TempDir(), ""); found != nil {
		t.Fatalf("expected nil for a non-repo, got %+v", found)
	}
	if found := detectStrandedWork("", ""); found != nil {
		t.Fatalf("expected nil for empty path, got %+v", found)
	}
}
