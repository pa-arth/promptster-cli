package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A candidate's work can end up in a checkout that is NOT session.TaskRoot, and
// every bundling path in code_submit.go looks only at TaskRoot. Two ways it
// happens, both silent and both observed in practice:
//
//   - The generated setupInstructions still read
//     `git clone <url> && cd <repo> && git checkout <sha> && …`, but `promptster
//     start` has already cloned and checked out the repo. A candidate who runs
//     the instructions anyway lands in a SECOND clone nested inside the
//     workspace, and TaskRoot stays pristine.
//   - An agent following a user-level instruction to "always work in a git
//     worktree" creates a linked worktree. `git -C <taskRoot> diff` cannot see
//     another worktree's HEAD, so the work is invisible to both the watcher and
//     the bundler.
//
// Either way the candidate finishes, `done` computes an empty diff, prints a
// warning nobody reads, and submits nothing. This finds that work so `done` can
// say so in terms the candidate can act on.

type strandedWork struct {
	Path   string
	Kind   string // "linked worktree" | "nested clone"
	Dirty  int    // uncommitted entries
	Ahead  int    // commits not reachable from the base ref
	Branch string
}

func (s strandedWork) summary() string {
	parts := []string{}
	if s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) beyond the assessment base", s.Ahead))
	}
	if s.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted change(s)", s.Dirty))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// detectStrandedWork returns checkouts under (or linked to) taskRoot that carry
// work, excluding taskRoot itself. baseRef is the assessment's pinned commit; an
// empty baseRef falls back to taskRoot's HEAD.
func detectStrandedWork(taskRoot, baseRef string) []strandedWork {
	if taskRoot == "" || !isGitRepository(taskRoot) {
		return nil
	}
	rootAbs := absPath(taskRoot)

	if strings.TrimSpace(baseRef) == "" {
		if out, err := runCommand(taskRoot, "git", "rev-parse", "HEAD"); err == nil {
			baseRef = strings.TrimSpace(string(out))
		}
	}

	seen := map[string]bool{rootAbs: true}
	var found []strandedWork

	for _, p := range linkedWorktreePaths(taskRoot) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if w, ok := inspectCheckout(p, "linked worktree", baseRef); ok {
			found = append(found, w)
		}
	}
	for _, p := range nestedRepoPaths(rootAbs) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if w, ok := inspectCheckout(p, "nested clone", baseRef); ok {
			found = append(found, w)
		}
	}
	return found
}

func linkedWorktreePaths(taskRoot string) []string {
	out, err := runCommand(taskRoot, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			paths = append(paths, absPath(rest))
		}
	}
	return paths
}

// Directories we never descend into: they routinely contain vendored git repos
// that are not the candidate's work.
var strandedSkipDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "vendor": true,
	"target": true, "dist": true, "build": true, ".tox": true,
	"site-packages": true, ".mypy_cache": true, "__pycache__": true,
}

const strandedMaxDepth = 4

func nestedRepoPaths(rootAbs string) []string {
	var paths []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > strandedMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strandedSkipDirs[name] {
				continue
			}
			child := filepath.Join(dir, name)
			if name == ".git" {
				if parent := absPath(dir); parent != rootAbs {
					paths = append(paths, parent)
				}
				continue
			}
			walk(child, depth+1)
		}
	}
	walk(rootAbs, 0)
	return paths
}

func inspectCheckout(path, kind, baseRef string) (strandedWork, bool) {
	if !isGitRepository(path) {
		return strandedWork{}, false
	}
	w := strandedWork{Path: path, Kind: kind}

	if out, err := runCommand(path, "git", "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) != "" {
				w.Dirty++
			}
		}
	}
	if baseRef != "" {
		if out, err := runCommand(path, "git", "rev-list", "--count", baseRef+"..HEAD"); err == nil {
			w.Ahead, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
	}
	if out, err := runCommand(path, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		w.Branch = strings.TrimSpace(string(out))
	}
	if w.Dirty == 0 && w.Ahead == 0 {
		return strandedWork{}, false
	}
	return w, true
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		if r, err := filepath.EvalSymlinks(a); err == nil {
			return r
		}
		return a
	}
	return p
}

// reportStrandedWork prints an unmissable block naming every checkout that holds
// work the bundle will not contain. Deliberately loud: the failure it describes
// is otherwise indistinguishable from a candidate who wrote no code.
func reportStrandedWork(taskRoot string, found []strandedWork) {
	bar := strings.Repeat("─", 68)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintln(os.Stderr, "  YOUR WORK IS NOT IN THE SUBMISSION")
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintf(os.Stderr, "  The assessment workspace has no changes:\n    %s\n\n", taskRoot)
	fmt.Fprintln(os.Stderr, "  But these checkouts do. Only the workspace above is submitted:")
	for _, w := range found {
		fmt.Fprintf(os.Stderr, "    • %s\n      %s", w.Path, w.Kind)
		if w.Branch != "" && w.Branch != "HEAD" {
			fmt.Fprintf(os.Stderr, " on %s", w.Branch)
		}
		fmt.Fprintf(os.Stderr, " — %s\n", w.summary())
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  To fix, move the work into the workspace and re-run `promptster done`:")
	if len(found) > 0 {
		fmt.Fprintf(os.Stderr, "    cd %s\n", taskRoot)
		fmt.Fprintf(os.Stderr, "    git checkout %s -- .   # if the work is on a branch here\n", strings.TrimSpace(found[0].Branch))
		fmt.Fprintln(os.Stderr, "    # or copy the changed files across by hand, then re-run done")
	}
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintln(os.Stderr)
}
