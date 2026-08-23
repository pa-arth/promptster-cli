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
	// AheadUnknown is set when the commit comparison could not be run at all.
	// Reporting "0 commits" in that case is how committed work goes missing, so
	// an unknown is treated as a positive rather than as a zero.
	AheadUnknown bool
}

func (s strandedWork) summary() string {
	parts := []string{}
	if s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) beyond the assessment base", s.Ahead))
	}
	if s.AheadUnknown {
		parts = append(parts, "commits that could not be compared to the assessment base")
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
// empty baseRef falls back to taskRoot's HEAD. repoURL is the assessment repo,
// used to recognise a nested checkout as a copy of it.
func detectStrandedWork(taskRoot, baseRef, repoURL string) []strandedWork {
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

	// Linked worktrees need no identity check: `git worktree list` run inside
	// taskRoot only ever names worktrees of taskRoot's own repository.
	for _, p := range linkedWorktreePaths(taskRoot) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if w, ok := inspectCheckout(p, "linked worktree", baseRef); ok {
			found = append(found, w)
		}
	}
	// Nested checkouts are a different story. A workspace can be any directory
	// the candidate chose, so it may well contain repositories that have nothing
	// to do with the assessment. Blocking `done` on someone's unrelated dirty
	// side project would strand a valid submission, so only checkouts that are
	// demonstrably copies of the assessment repo count.
	for _, p := range nestedRepoPaths(rootAbs) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if !isAssessmentCheckout(p, baseRef, repoURL) {
			continue
		}
		if w, ok := inspectCheckout(p, "nested clone", baseRef); ok {
			found = append(found, w)
		}
	}
	return found
}

// isAssessmentCheckout reports whether path is a copy of the assessment repo,
// by either of two independent signals: it holds the pinned base commit, or its
// origin points at the same place. Either alone is enough — a shallow clone has
// the remote but not the object, and a clone made from the local workspace has
// the object but not the remote.
func isAssessmentCheckout(path, baseRef, repoURL string) bool {
	if hasCommit(path, baseRef) {
		return true
	}
	if strings.TrimSpace(repoURL) == "" {
		return false
	}
	out, err := runCommand(path, "git", "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	return normalizeRepoURL(string(out)) == normalizeRepoURL(repoURL)
}

func hasCommit(path, rev string) bool {
	if strings.TrimSpace(rev) == "" {
		return false
	}
	_, err := runCommand(path, "git", "rev-parse", "--verify", "--quiet", strings.TrimSpace(rev)+"^{commit}")
	return err == nil
}

// normalizeRepoURL reduces the forms git accepts for the same remote —
// https://host/org/repo.git, git@host:org/repo, ssh://git@host/org/repo/ — to a
// single comparable host/org/repo string.
func normalizeRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	s = strings.Replace(s, ":", "/", 1)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.ToLower(strings.TrimSuffix(s, "/"))
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
	w.Ahead, w.AheadUnknown = commitsBeyondBase(path, baseRef)

	if out, err := runCommand(path, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		w.Branch = strings.TrimSpace(string(out))
	}
	if w.Dirty == 0 && w.Ahead == 0 && !w.AheadUnknown {
		return strandedWork{}, false
	}
	return w, true
}

// commitsBeyondBase counts commits in path that are not part of the assessment
// base, returning (count, unknown). The unknown flag matters: a candidate who
// committed everything in a shallow clone has Dirty == 0, so treating a failed
// comparison as "zero commits" would discard the checkout and submit the
// pristine workspace — precisely the loss this file exists to prevent.
func commitsBeyondBase(path, baseRef string) (int, bool) {
	// An unborn HEAD genuinely has no commits; that is a known zero, not an
	// unknown, and must not be reported as stranded work.
	if !hasCommit(path, "HEAD") {
		return 0, false
	}
	if hasCommit(path, baseRef) {
		out, err := runCommand(path, "git", "rev-list", "--count", strings.TrimSpace(baseRef)+"..HEAD")
		if err != nil {
			return 0, true
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0, true
		}
		return n, false
	}
	// The base object is missing — a shallow or partial clone. Fall back to
	// commits that no remote-tracking ref can reach, which is what the candidate
	// wrote locally.
	out, err := runCommand(path, "git", "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return 0, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, true
	}
	return n, false
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
		// A nested clone is usually on a detached HEAD, where `git checkout HEAD`
		// would be a no-op that looks like it worked. Only name a branch when
		// there is a real one to name.
		if b := strings.TrimSpace(found[0].Branch); b != "" && b != "HEAD" {
			fmt.Fprintf(os.Stderr, "    git checkout %s -- .   # if the work is on that branch here\n", b)
		}
		fmt.Fprintf(os.Stderr, "    # or copy the changed files across, e.g.\n")
		fmt.Fprintf(os.Stderr, "    #   rsync -a --exclude .git %s/ %s/\n", strings.TrimRight(found[0].Path, "/"), strings.TrimRight(taskRoot, "/"))
		fmt.Fprintln(os.Stderr, "    # then re-run `promptster done`")
	}
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintln(os.Stderr)
}
