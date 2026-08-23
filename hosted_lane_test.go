package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInCodespaceDetection(t *testing.T) {
	cases := []struct {
		name       string
		codespaces string
		nameVar    string
		want       bool
	}{
		{"unset", "", "", false},
		{"codespaces true", "true", "", true},
		{"codespaces TRUE", "TRUE", "", true},
		{"codespaces 1", "1", "", true},
		{"codespaces false but name set", "false", "fluffy-space-doodle", true},
		{"name only", "", "fluffy-space-doodle", true},
		{"codespaces false, no name", "false", "", false},
		{"blank name", "", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODESPACES", tc.codespaces)
			t.Setenv("CODESPACE_NAME", tc.nameVar)
			if got := inCodespace(); got != tc.want {
				t.Fatalf("inCodespace() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The whole point of --adopt defaulting on is that the candidate types the same
// command on both lanes. If this ever needs a lane-specific flag, the hosted lane
// has re-added the setup step it exists to delete.
func TestHostedLaneNeedsNoExtraFlag(t *testing.T) {
	t.Setenv("CODESPACES", "true")
	t.Setenv("CODESPACE_NAME", "fluffy-space-doodle")
	if !inCodespace() {
		t.Fatal("hosted lane not detected from env — --adopt would not default on")
	}
	t.Setenv("CODESPACES", "")
	t.Setenv("CODESPACE_NAME", "")
	if inCodespace() {
		t.Fatal("local lane detected as hosted — --adopt would default on for laptops")
	}
}

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
	t.Setenv("CODESPACE_VSCODE_FOLDER", "")
	if _, err := resolveAdoptWorkspace(dir); err == nil {
		t.Fatal("adopted a path that is not a git checkout")
	}
}

func TestHostedSetupMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hosted-setup-complete")
	t.Setenv("PROMPTSTER_HOSTED_SETUP_MARKER", marker)

	if hostedSetupCompleted() {
		t.Fatal("reported setup complete with no marker on disk")
	}
	if _, ok := readHostedSetupMarker(); ok {
		t.Fatal("read a marker that does not exist")
	}

	// The §3.9 body: key=value lines, exactly as scripts/lib/devcontainer.mjs
	// emits them.
	written := "schema=1\nstartedAt=2026-08-23T10:00:00Z\ncompletedAt=2026-08-23T10:00:42Z\n" +
		"setupSeconds=42\nprebuildEnvRaw=\nprebuildEnvSet=no\n" +
		"codespaceName=octocat-space-1\nissueId=prometheus-prometheus-15141\n"
	if err := os.WriteFile(marker, []byte(written), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if !hostedSetupCompleted() {
		t.Fatal("marker present but setup reported incomplete")
	}
	body, ok := readHostedSetupMarker()
	if !ok {
		t.Fatal("marker present but not read")
	}
	if body.Schema != 1 {
		t.Fatalf("schema = %d, want 1", body.Schema)
	}
	if body.SetupSeconds == nil || *body.SetupSeconds != 42 {
		t.Fatalf("setupSeconds = %v, want 42", body.SetupSeconds)
	}
	if body.CompletedAt != "2026-08-23T10:00:42Z" {
		t.Fatalf("completedAt = %q", body.CompletedAt)
	}
	if body.IssueID != "prometheus-prometheus-15141" {
		t.Fatalf("issueId = %q", body.IssueID)
	}
	// Evidence, never a verdict: an unset CODESPACE_PREBUILD must not read as a
	// statement that this was not a prebuild.
	if body.PrebuildEnvSet {
		t.Fatal("prebuildEnvSet true for an unset variable")
	}

	// An empty (touched) marker still means the setup command finished. Presence
	// is the signal; the body is extra.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("truncate marker: %v", err)
	}
	if _, ok := readHostedSetupMarker(); !ok {
		t.Fatal("an empty marker was treated as absent")
	}
}

func TestHostedLaneActiveSurvivesStrippedEnv(t *testing.T) {
	t.Setenv("CODESPACES", "")
	t.Setenv("CODESPACE_NAME", "")
	if hostedLaneActive(Session{}) {
		t.Fatal("local session reported as hosted")
	}
	// `done` can run from a shell that never inherited the codespace env. The
	// session flag is what keeps the commit suppression and the wind-down from
	// silently reverting to local-lane behaviour.
	if !hostedLaneActive(Session{HostedLane: true}) {
		t.Fatal("hosted session lost its lane when the env was stripped")
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

func TestDeleteHostingCodespaceRefusesWithoutName(t *testing.T) {
	t.Setenv("CODESPACE_NAME", "")
	if err := deleteHostingCodespace(); err == nil {
		t.Fatal("attempted a delete with no codespace name")
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

// The submission commit is what triggers GitHub's automatic PUBLIC FORK under a
// candidate's own account when they commit from a read-only mirror codespace —
// a repo named after the assessment problem, on their profile. Suppressing it is
// the fork-prevention mechanism, so assert on the commit graph, not on a flag.
func TestSubmissionCommitSuppressedOnHostedLane(t *testing.T) {
	t.Setenv("CODESPACES", "")
	t.Setenv("CODESPACE_NAME", "")

	countCommits := func(dir string) string {
		out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-list: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	hosted := t.TempDir()
	gitInitSeeded(t, hosted)
	before := countCommits(hosted)
	if recordSubmissionCommit(Session{HostedLane: true}, hosted) {
		t.Fatal("recordSubmissionCommit reported an attempt on the hosted lane")
	}
	if after := countCommits(hosted); after != before {
		t.Fatalf("hosted lane made a commit: %s -> %s", before, after)
	}

	local := t.TempDir()
	gitInitSeeded(t, local)
	before = countCommits(local)
	if !recordSubmissionCommit(Session{}, local) {
		t.Fatal("recordSubmissionCommit skipped the commit on the local lane")
	}
	if after := countCommits(local); after == before {
		t.Fatalf("local lane made no commit: still %s", after)
	}
}

// The suppression must not cost the submission anything: the bundle reads the
// INDEX and the diff compares the WORKING TREE to the base, so both still see
// the candidate's work with no commit in between.
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
	if recordSubmissionCommit(Session{HostedLane: true}, dir) {
		t.Fatal("commit was attempted")
	}

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
// detaches HEAD; run against a codespace's checkout it would silently move the
// candidate off the ref their container was built from. The adopt path may only
// look.
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
