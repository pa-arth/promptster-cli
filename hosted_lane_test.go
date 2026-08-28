package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvaluateAdoptedTree(t *testing.T) {
	const sha = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	cases := []struct {
		name      string
		expected  string
		actual    string
		readErr   error
		wantState string
		wantFatal bool
	}{
		{"match", sha, sha, nil, hostedTreeVerified, false},
		{"case insensitive", strings.ToUpper(sha), sha, nil, hostedTreeVerified, false},
		{"abbreviated expected", sha[:12], sha, nil, hostedTreeVerified, false},
		{"mismatch", sha, "0000000000000000000000000000000000000000", nil, hostedTreeMismatch, true},
		{"no expected sha", "", sha, nil, hostedTreeUnverified, false},
		{"read error", sha, "", errors.New("not a git repo"), hostedTreeUnreadable, true},
		{"read error wins over missing expectation", "", "", errors.New("boom"), hostedTreeUnreadable, true},
		{"empty actual is not a match", "", "", nil, hostedTreeUnreadable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateAdoptedTree(tc.expected, tc.actual, tc.readErr)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.fatal() != tc.wantFatal {
				t.Fatalf("fatal() = %v, want %v", got.fatal(), tc.wantFatal)
			}
		})
	}
}

// A missing expectedTreeSha must never render as a pass. The backend does not
// serve the field yet, so this is the live case, not a hypothetical one.
func TestMissingExpectedTreeShaIsNotVerified(t *testing.T) {
	got := evaluateAdoptedTree("", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", nil)
	if got.State == hostedTreeVerified {
		t.Fatal("an absent expectedTreeSha was reported as verified")
	}
	if got.State != hostedTreeUnverified {
		t.Fatalf("state = %q, want %q", got.State, hostedTreeUnverified)
	}
	if got.fatal() {
		t.Fatal("unverified must not be fatal — the backend does not serve the field yet")
	}
}

func TestTreeShasMatchRejectsShortPrefixes(t *testing.T) {
	if treeShasMatch("4b", "4b825dc642cb6eb9a060e54bf8d69288fbee4904") {
		t.Fatal("a 2-character prefix was accepted as a match")
	}
	if !treeShasMatch("4b825dc", "4b825dc642cb6eb9a060e54bf8d69288fbee4904") {
		t.Fatal("a 7-character abbreviation was rejected")
	}
	if treeShasMatch("", "") {
		t.Fatal("two empty shas matched")
	}
}

// gitInitSeeded initialises a repo AND lands a first commit, which the tree- and
// nested-checkout tests here need. Distinct from stranded_work_test.go's gitInit,
// which deliberately leaves HEAD unborn — commitsBeyondBase treats an unborn HEAD
// as a known zero, so that test file cannot use a seeded repo.
func gitInitSeeded(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@promptster.local"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestReadTreeShaMatchesGit(t *testing.T) {
	dir := t.TempDir()
	gitInitSeeded(t, dir)

	sha, err := readTreeSha(dir)
	if err != nil {
		t.Fatalf("readTreeSha: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("tree sha = %q, want 40 hex chars", sha)
	}
	if got := evaluateAdoptedTree(sha, sha, nil); got.State != hostedTreeVerified {
		t.Fatalf("state = %q, want verified", got.State)
	}
}

func TestReadTreeShaFailsOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := readTreeSha(dir); err == nil {
		t.Fatal("readTreeSha succeeded on a non-repository")
	}
	if got := evaluateAdoptedTree("abc", "", errors.New("x")); !got.fatal() {
		t.Fatal("an unreadable tree must be fatal — done cannot bundle from it")
	}
}

func TestNestedGitCheckoutsFindsSecondClone(t *testing.T) {
	root := t.TempDir()
	gitInitSeeded(t, root)
	nested := filepath.Join(root, "prometheus")
	gitInitSeeded(t, nested)

	found := nestedGitCheckouts(root, 4)
	if len(found) != 1 || found[0] != nested {
		t.Fatalf("nestedGitCheckouts = %v, want [%s]", found, nested)
	}
}

func TestNestedGitCheckoutsIgnoresRootAndVendorDirs(t *testing.T) {
	root := t.TempDir()
	gitInitSeeded(t, root)
	// A vendored dependency with its own .git must not be reported: crying wolf
	// on every node_modules would train candidates to ignore the one that matters.
	vendored := filepath.Join(root, "node_modules", "left-pad")
	gitInitSeeded(t, vendored)

	if found := nestedGitCheckouts(root, 4); len(found) != 0 {
		t.Fatalf("nestedGitCheckouts = %v, want none", found)
	}
}

func TestNestedGitCheckoutsFindsLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	gitInitSeeded(t, root)
	// A linked worktree's .git is a FILE, not a directory. Work stranded in one
	// is exactly as invisible to `done` as work in a second clone.
	wt := filepath.Join(root, "wt")
	cmd := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "side", wt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree unavailable: %v: %s", err, out)
	}
	found := nestedGitCheckouts(root, 4)
	if len(found) != 1 || found[0] != wt {
		t.Fatalf("nestedGitCheckouts = %v, want [%s]", found, wt)
	}
}

func TestSingleSubdirectoryRefusesAmbiguity(t *testing.T) {
	root := t.TempDir()
	if got := singleSubdirectory(root); got != "" {
		t.Fatalf("empty dir returned %q", got)
	}
	only := filepath.Join(root, "repo")
	if err := os.Mkdir(only, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := singleSubdirectory(root); got != only {
		t.Fatalf("singleSubdirectory = %q, want %q", got, only)
	}
	if err := os.Mkdir(filepath.Join(root, "second"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := singleSubdirectory(root); got != "" {
		t.Fatalf("two candidates returned %q — adopt must refuse to guess", got)
	}
}

func TestResolveAdoptWorkspaceUsesFlagToplevel(t *testing.T) {
	root := t.TempDir()
	gitInitSeeded(t, root)
	sub := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := resolveAdoptWorkspace(sub)
	if err != nil {
		t.Fatalf("resolveAdoptWorkspace: %v", err)
	}
	// macOS /var -> /private/var symlinks make a string compare unreliable.
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != wantResolved {
		t.Fatalf("resolveAdoptWorkspace = %q, want the work-tree root %q", gotResolved, wantResolved)
	}
}

func TestResolveAdoptWorkspaceRejectsNonRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveAdoptWorkspace(dir); err == nil {
		t.Fatal("adopted a path that is not a git checkout")
	}
}

// 2.4i replaced `hostedLaneActive` (session flag OR `inCodespace()`) with
// `seededSession`, which reads only the session. The property that mattered
// survives the swap and is the reason this test does: `done`, `abort` and
// `doctor` routinely run from a shell that inherited no environment at all —
// a bare `sh -c`, a detached process — and the answer must not change.
//
// The old predicate needed a session flag precisely BECAUSE it also consulted
// the environment and the environment could vanish. Reading the session alone
// makes that structural rather than remembered.
func TestSeededSessionSurvivesStrippedEnv(t *testing.T) {
	os.Unsetenv("CODESPACES")
	os.Unsetenv("CODESPACE_NAME")
	if seededSession(Session{}) {
		t.Fatal("a session with no SeededAt reported as seeded")
	}
	if !seededSession(Session{SeededAt: time.Now()}) {
		t.Fatal("a seeded session lost that fact when the env was stripped")
	}
}

func TestDiffBaseForPrefersAdoptedHead(t *testing.T) {
	// RepoCommit is upstream's brokenSha, which names no object in a mirror
	// clone. Diffing against it fails and yields an EMPTY submission.
	s := Session{RepoCommit: "e6f9e2dde32db928a33b1611297c46ed293562a1"}
	if got := diffBaseFor(s); got != s.RepoCommit {
		t.Fatalf("local lane base = %q, want RepoCommit", got)
	}
	s.DiffBaseCommit = "1111111111111111111111111111111111111111"
	if got := diffBaseFor(s); got != s.DiffBaseCommit {
		t.Fatalf("adopted base = %q, want DiffBaseCommit", got)
	}
	s.DiffBaseCommit = "   "
	if got := diffBaseFor(s); got != s.RepoCommit {
		t.Fatalf("blank DiffBaseCommit should fall back to RepoCommit, got %q", got)
	}
}

func TestLooksLikeAssessmentKey(t *testing.T) {
	for _, ok := range []string{"PST-ABCD-1234", "pst-abcd-1234", "  PST-X  "} {
		if !looksLikeAssessmentKey(ok) {
			t.Fatalf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "   ", "ABCD-1234", "https://promptster.ai"} {
		if looksLikeAssessmentKey(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// A non-terminal stdin must return "" rather than blocking: the prompt is for
// humans, and turning a scripted fast-fail into a hang on a closed pipe would be
// a worse regression than the error it replaces.
func TestPromptForAssessmentKeyIsNoOpWithoutTerminal(t *testing.T) {
	if got := promptForAssessmentKeyFrom(strings.NewReader("PST-ABCD-1234\n"), false); got != "" {
		t.Fatalf("promptForAssessmentKeyFrom(non-terminal) = %q, want \"\"", got)
	}
}

func TestPromptForAssessmentKeyReadsAKey(t *testing.T) {
	got := promptForAssessmentKeyFrom(strings.NewReader("  PST-ABCD-1234  \n"), true)
	if got != "PST-ABCD-1234" {
		t.Fatalf("promptForAssessmentKeyFrom = %q, want PST-ABCD-1234", got)
	}
}

func TestPromptForAssessmentKeyRetriesOnGarbage(t *testing.T) {
	got := promptForAssessmentKeyFrom(strings.NewReader("what?\nPST-ABCD-1234\n"), true)
	if got != "PST-ABCD-1234" {
		t.Fatalf("promptForAssessmentKeyFrom = %q, want the key after one bad line", got)
	}
}

func TestPromptForAssessmentKeyGivesUpOnEOF(t *testing.T) {
	if got := promptForAssessmentKeyFrom(strings.NewReader(""), true); got != "" {
		t.Fatalf("promptForAssessmentKeyFrom(EOF) = %q, want \"\"", got)
	}
	// An empty line is a deliberate "not now" — it must not loop.
	if got := promptForAssessmentKeyFrom(strings.NewReader("\n"), true); got != "" {
		t.Fatalf("promptForAssessmentKeyFrom(blank) = %q, want \"\"", got)
	}
}

func TestHostedBriefSaysWhatChanges(t *testing.T) {
	lines := strings.Join(hostedBriefLines(), " ")
	for _, want := range []string{"do not need to commit", "promptster done"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("hosted brief is missing %q: %s", want, lines)
		}
	}
}

func TestSystemShellInitSourcesHookReadsRealPaths(t *testing.T) {
	// No system file on a developer laptop carries our marker; the hosted image
	// is what puts it there. Assert the negative so the skip in `start` cannot
	// silently fire on a machine where the injection is still needed.
	if systemShellInitSourcesHook() {
		t.Skip("a system shell init on this machine already sources the hook")
	}
	for _, p := range systemShellInitPaths {
		if !filepath.IsAbs(p) {
			t.Fatalf("system init path %q is not absolute", p)
		}
	}
}

// ⛔ THIS TEST IS INVERTED BY 2.4i, and the inversion is the point.
//
// It used to assert that `recordSubmissionCommit` made NO commit on the hosted
// lane. That suppression was a privacy mechanism (design.md §2): committing from
// a read-only GitHub Codespace makes GitHub create a public fork under the
// candidate's account, naming the assessment problem on their profile.
//
// The Codespace is gone, so the fork mechanism is gone, and suppressing the
// audit-trail commit on the lane that will carry most assessments would now be
// protecting against nothing at a real cost. The commit runs everywhere.
func TestSubmissionCommitRunsOnEveryLane(t *testing.T) {
	countCommits := func(dir string) string {
		out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-list: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	for _, name := range []string{"seeded", "local"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			gitInitSeeded(t, dir)
			before := countCommits(dir)
			recordSubmissionCommit(dir)
			if after := countCommits(dir); after == before {
				t.Fatalf("no submission commit was made: still %s", after)
			}
		})
	}
}

// The commit must not be LOad-BEARING: the bundle reads the INDEX and the diff
// compares the WORKING TREE to the base, so both see the candidate's work with
// no commit in between.
//
// This is what made suppressing the commit free before 2.4i, and it is what
// makes restoring it safe now — a `git commit` that fails (a read-only home, a
// hook that rejects, a missing identity) must not cost the candidate their
// submission. So the property is asserted with NO commit made at all.
func TestWorkIsStillVisibleWithNoSubmissionCommit(t *testing.T) {
	dir := t.TempDir()
	gitInitSeeded(t, dir)
	base, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	baseSha := strings.TrimSpace(string(base))

	if err := os.WriteFile(filepath.Join(dir, "fix.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	// Deliberately NOT calling recordSubmissionCommit: this asserts the diff and
	// the file list stand on their own.

	diff, err := exec.Command("git", "-C", dir, "diff", baseSha).Output()
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}
	if !strings.Contains(string(diff), "fix.go") {
		t.Fatalf("diff against the base does not show the work: %q", string(diff))
	}
	files, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		t.Fatalf("ls-files: %v", err)
	}
	if !strings.Contains(string(files), "fix.go") {
		t.Fatalf("bundle file list does not show the work: %q", string(files))
	}
}

// --adopt must be READ-ONLY. prepareWorkspaceCheckout rewrites origin and
// detaches HEAD; run against a seeded checkout it would silently move the
// candidate off the tree the provisioner wrote — and off the tree
// `snapshot_tree_sha` was asserted against. The adopt path may only look.
func TestAdoptDoesNotMutateTheCheckout(t *testing.T) {
	root := t.TempDir()
	gitInitSeeded(t, root)
	if out, err := exec.Command("git", "-C", root, "remote", "add", "origin", "https://github.com/promptster-assessments/pilot.git").CombinedOutput(); err != nil {
		t.Fatalf("remote add: %v: %s", err, out)
	}
	read := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	beforeRemote := read("remote", "get-url", "origin")
	beforeHead := read("rev-parse", "HEAD")
	beforeRef := read("symbolic-ref", "--quiet", "HEAD")

	adopted, err := resolveAdoptWorkspace(root)
	if err != nil {
		t.Fatalf("resolveAdoptWorkspace: %v", err)
	}
	if _, err := readTreeSha(adopted); err != nil {
		t.Fatalf("readTreeSha: %v", err)
	}

	if got := read("remote", "get-url", "origin"); got != beforeRemote {
		t.Fatalf("origin rewritten: %q -> %q", beforeRemote, got)
	}
	if got := read("rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD moved: %q -> %q", beforeHead, got)
	}
	if got := read("symbolic-ref", "--quiet", "HEAD"); got != beforeRef {
		t.Fatalf("HEAD detached: %q -> %q", beforeRef, got)
	}
}
