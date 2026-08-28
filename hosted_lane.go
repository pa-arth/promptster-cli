package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Adopting a workspace that already exists (openspec
// changes/codespaces-hosted-assessment §2, and `private-problem-sandbox-lane`
// §8.4's `start --seeded`).
//
// The single fact that shapes everything here: sometimes there is nothing to
// clone. `prepareWorkspaceCheckout` would materialise a SECOND tree beside the
// one the candidate is looking at, and TaskRoot would follow it — the candidate
// then works in one tree while `done` bundles another, and the loss is silent
// because an empty diff and an untouched tree look identical. So this path
// ADOPTS the checkout it is pointed at and verifies it by content.
//
// ⚠ WRITTEN FOR GITHUB CODESPACES, WHICH IS GONE. 2.4i repointed the hosted lane
// onto the E2B box (2026-08-27) and the Codespaces halves of this file went with
// it: `inCodespace`, `codespaceName`, the `/workspaces` probe, `gh codespace
// delete`, the `doctor` section, and the whole `hosted_boot.go` reporter — which
// read boot facts off GitHub's codespaces API and has no successor here, because
// the box's boot is reported by the WORKER that provisioned it.
//
// What survived is everything that was never about the host. `--seeded` lands in
// a box whose tree was written from the published snapshot, and it faces the
// identical hazard for the identical reason.
//
// Verification is by TREE, never by remote — and that outlived its original
// justification. It was chosen because GitHub reassigns `origin` to the
// candidate's automatic fork the moment a commit is made from a read-only
// codespace (design.md §2), so a remote check would fail for a reason unrelated
// to whether the tree was right. The box has no such behaviour, and the choice
// is still correct: a remote is a claim about where bytes came from, and the
// question is what bytes are here.

// Tree-verification states for an adopted checkout. The three failing states are
// deliberately distinct: "the tree is wrong" and "nobody told us what the tree
// should be" are opposite facts, and collapsing them is how a mis-provisioned
// codespace passes as verified.
const (
	// hostedTreeVerified — HEAD^{tree} equals the expected tree sha.
	hostedTreeVerified = "verified"
	// hostedTreeMismatch — the checkout is not the assessment. Fatal, no repair.
	hostedTreeMismatch = "mismatch"
	// hostedTreeUnverified — the session carries no expectedTreeSha, so there is
	// nothing to compare against. NOT a pass: the check did not run.
	hostedTreeUnverified = "unverified"
	// hostedTreeUnreadable — HEAD^{tree} could not be read at all (not a git
	// checkout, or a broken one). Fatal: `done` bundles via git and cannot
	// submit from a path that is not a repository.
	hostedTreeUnreadable = "unreadable"
)

// adoptOutcome is the result of checking an adopted checkout against the tree
// the session says the assessment is.
type adoptOutcome struct {
	State    string
	Expected string
	Actual   string
}

// fatal reports whether this outcome must stop `start` dead.
//
// design.md §5: --adopt verifies, it must not repair. A mismatched tree means
// the mirror is wrong, the prebuild is stale, or the candidate opened a ref we
// did not intend — every one of those is OUR provisioning fault, and a repair
// would hide it after the candidate has started working on the wrong problem.
func (o adoptOutcome) fatal() bool {
	return o.State == hostedTreeMismatch || o.State == hostedTreeUnreadable
}

// normalizeTreeSha lowercases and trims a git object id for comparison.
func normalizeTreeSha(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// treeShasMatch compares two object ids, tolerating an abbreviated one on
// either side. Git abbreviations are prefixes, so a prefix comparison is the
// correct relation — but only above a length where a prefix means something; a
// 2-character "match" is noise, not evidence.
func treeShasMatch(a, b string) bool {
	a, b = normalizeTreeSha(a), normalizeTreeSha(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	const minAbbrev = 7
	if len(a) < minAbbrev || len(b) < minAbbrev {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// evaluateAdoptedTree classifies an adopted checkout. Pure, so the four states
// are testable without a git repository.
//
// readErr non-nil (or an empty actual) ⇒ unreadable, and that is checked FIRST:
// "we could not read the tree" must never be reported as "the tree is fine",
// which is what returning verified on an empty-vs-empty comparison would do.
func evaluateAdoptedTree(expected, actual string, readErr error) adoptOutcome {
	out := adoptOutcome{Expected: normalizeTreeSha(expected), Actual: normalizeTreeSha(actual)}
	if readErr != nil || out.Actual == "" {
		out.State = hostedTreeUnreadable
		return out
	}
	if out.Expected == "" {
		out.State = hostedTreeUnverified
		return out
	}
	if treeShasMatch(out.Expected, out.Actual) {
		out.State = hostedTreeVerified
		return out
	}
	out.State = hostedTreeMismatch
	return out
}

// readTreeSha returns `git rev-parse HEAD^{tree}` for a checkout.
func readTreeSha(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitToplevel returns the root of the work tree containing dir.
func gitToplevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", fmt.Errorf("no git work tree at %s", dir)
	}
	return top, nil
}

// singleSubdirectory returns the sole visible subdirectory of root, or "" when
// there is none or more than one.
//
// "More than one" deliberately returns nothing rather than picking the first:
// two candidate checkouts under /workspaces is the exact ambiguity --adopt
// exists to refuse, and guessing would adopt the wrong one silently.
func singleSubdirectory(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if found != "" {
			return ""
		}
		found = filepath.Join(root, e.Name())
	}
	return found
}

// resolveAdoptWorkspace finds the checkout to adopt. It never prompts and never
// creates anything: on this lane the tree already exists, and a path we had to
// invent is by definition not the one the container was built from.
func resolveAdoptWorkspace(flagValue string) (string, error) {
	if strings.TrimSpace(flagValue) != "" {
		p := expandPath(flagValue)
		top, err := gitToplevel(p)
		if err != nil {
			return "", fmt.Errorf("--workspace %s is not a git checkout", p)
		}
		return top, nil
	}
	if cwd, err := os.Getwd(); err == nil {
		if top, err := gitToplevel(cwd); err == nil {
			return top, nil
		}
	}
	return "", fmt.Errorf("could not find the assessment checkout to adopt — run promptster start from inside it, or pass --workspace PATH")
}

// dirsSkippedInNestedScan are directories that legitimately contain vendored
// git checkouts and would make the nested-repo scan cry wolf on every project.
var dirsSkippedInNestedScan = map[string]bool{
	"node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"target": true, "dist": true, "build": true, ".next": true,
	"__pycache__": true, ".promptster": true, ".cache": true,
	"site-packages": true, ".tox": true, ".gradle": true,
}

// nestedGitCheckouts returns directories under root (excluding root itself) that
// are git checkouts.
//
// This is the loud-at-minute-one guard for the failure §3.2 describes from the
// other side: a candidate who follows a stale `git clone … && cd …` instruction
// ends up working in a SECOND checkout inside the workspace, while TaskRoot —
// and therefore the bundle, the diff and the hooks — stays pointed at the first.
// Discovered at minute seventy-five it is an unrecoverable empty submission.
//
// A `.git` ENTRY counts whether it is a directory or a file: a file is a linked
// worktree or a submodule, and work stranded in one of those is just as invisible.
func nestedGitCheckouts(root string, maxDepth int) []string {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var found []string
	_ = filepath.WalkDir(rootAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not a finding
		}
		if !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(rootAbs, path)
		if relErr != nil {
			return filepath.SkipDir
		}
		if rel == "." {
			return nil
		}
		name := d.Name()
		if name == ".git" {
			parent := filepath.Dir(path)
			if parent != rootAbs {
				found = append(found, parent)
			}
			return filepath.SkipDir
		}
		if dirsSkippedInNestedScan[name] {
			return filepath.SkipDir
		}
		// A `.git` FILE (linked worktree / submodule) in this directory. The walk
		// itself only ever reports `.git` DIRECTORIES — a file is filtered out by
		// the !d.IsDir() guard above — so this is the only way one is seen.
		if info, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil && !info.IsDir() {
			found = append(found, path)
			return filepath.SkipDir
		}
		if depth := len(strings.Split(rel, string(filepath.Separator))); depth >= maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// hostedBriefLines is the seeded box's replacement for the local lane's
// clone-and-open guidance. Every line here is false on the local lane, and the
// two lines it replaces are false in a box.
//
// The third line CHANGED with 2.4i and the change is not cosmetic. It used to
// promise that `done` "deletes this codespace" — true, and load-bearing, because
// the machine was the candidate's and kept billing their storage allowance for
// 30 days if merely stopped. The box is ours and the provisioner pauses it, so
// the CLI no longer tears anything down and must not say it does. A promise the
// binary has stopped keeping is worse than no promise: the candidate who
// believes it goes looking for a machine to clean up and finds nothing.
func hostedBriefLines() []string {
	return []string{
		"This workspace IS your assessment — there is nothing to clone or set up.",
		"You do not need to commit. `promptster done` captures your working tree.",
		"When you are finished, run `promptster done`: it submits your work. The machine is ours and shuts down on its own.",
	}
}

// ⛔ The setup marker and its reader are GONE with hosted_boot.go (2.4i). They
// belonged to the Codespaces devcontainer's `onCreateCommand`, and the box's
// equivalent — did setup finish, and was the tree seeded correctly — is observed
// by the WORKER at provision time and written to `candidate_keys.metadata`,
// where it is durable and not a file the candidate could delete.
//
// The historical note, kept because it is the reason the shape was what it was:
// this file once declared the marker as `{prebuilt, bootSeconds}` — a
// placeholder written before §1.3 existed, and the wrong two fields.
// `onCreateCommand` cannot know either: it finishes before there is a terminal
// to time the boot to, and on a prebuilt box it ran days earlier in a different container
// generation, so it cannot say whether THIS container came from a prebuild.
// Both are GitHub's to answer. Nothing ever wrote that shape — there are no
// mirrors yet — so this replaces a guess, not a producer.

// looksLikeAssessmentKey reports whether s has the shape of a candidate key.
// Deliberately loose — the server is the authority on validity; this only stops
// an empty line or an obvious mis-paste from being sent as a redeem.
func looksLikeAssessmentKey(s string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "PST-")
}

// promptForAssessmentKey asks for the candidate key interactively, returning ""
// when there is nobody to ask (openspec §2.7).
//
// The empty return on a non-terminal stdin is load-bearing: it preserves the
// exact pre-existing failure for scripted callers, so adding a prompt for humans
// cannot turn a fast CI failure into a hang on a closed pipe.
func promptForAssessmentKey() string {
	return promptForAssessmentKeyFrom(os.Stdin, stdinIsTerminal())
}

// promptForAssessmentKeyFrom is promptForAssessmentKey with its two inputs made
// explicit so both branches are testable without a controlling terminal.
func promptForAssessmentKeyFrom(in io.Reader, interactive bool) string {
	if !interactive {
		return ""
	}
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	fmt.Println()
	fmt.Println(dim.Render("  No session yet. Paste the assessment key from your invitation email."))
	scanner := bufio.NewScanner(in)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Printf("  %s ", label.Render("Assessment key (PST-XXXX-XXXX):"))
		if !scanner.Scan() {
			return ""
		}
		key := strings.TrimSpace(scanner.Text())
		if key == "" {
			return ""
		}
		if looksLikeAssessmentKey(key) {
			return key
		}
		fmt.Printf("  %s\n", dim.Render("That does not look like an assessment key — they start with PST-."))
	}
	return ""
}

// printAdoptFailure explains a fatal adopt verdict and stops. There is no repair
// path here on purpose (design.md §5): a mismatched tree means the mirror is
// wrong, the prebuild is stale, or the wrong ref was opened, and "fixing" it
// would leave the candidate working on a problem nobody chose, discovered at
// grading time. A loud failure at second zero is cheap.
func printAdoptFailure(o adoptOutcome, path string) {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true)
	body := lipgloss.NewStyle().Foreground(cBody)
	dim := lipgloss.NewStyle().Foreground(cMuted)

	fmt.Println()
	switch o.State {
	case hostedTreeMismatch:
		fmt.Printf("  %s\n", title.Render("✗  This is not the assessment's code."))
		fmt.Printf("  %s\n", body.Render("The checkout here does not match the tree this assessment pins."))
		fmt.Printf("  %s\n", dim.Render("expected tree "+o.Expected))
		fmt.Printf("  %s\n", dim.Render("  actual tree "+o.Actual))
		fmt.Printf("  %s\n", dim.Render("at "+path))
		fmt.Println()
		fmt.Printf("  %s\n", body.Render("Nothing has been changed and nothing is recorded. This is our fault, not"))
		fmt.Printf("  %s\n", body.Render("yours: the environment was built from the wrong source. Tell whoever sent"))
		fmt.Printf("  %s\n", body.Render("you this assessment — starting anyway would mean working on the wrong problem."))
	case hostedTreeUnreadable:
		fmt.Printf("  %s\n", title.Render("✗  Could not read the code in this workspace."))
		fmt.Printf("  %s\n", body.Render(path+" is not a readable git checkout, so your work could not be submitted."))
		fmt.Println()
		fmt.Printf("  %s\n", body.Render("Tell whoever sent you this assessment. Do not start working here."))
	}
	fmt.Println()
}

// printAdoptUnverified states the one thing this must not do silently.
//
// An unverified adopt is a check that DID NOT RUN, not a check that passed. The
// tempting shape — treat a missing expectedTreeSha as "nothing to complain
// about" — makes a wrong tree and a correct one render identically, which is the
// exact failure the verification exists to catch.
func printAdoptUnverified(o adoptOutcome) {
	warn := lipgloss.NewStyle().Foreground(lipgloss.Color("#f59e0b")).Bold(true)
	body := lipgloss.NewStyle().Foreground(cWarnText)
	dim := lipgloss.NewStyle().Foreground(cMuted)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#f59e0b")).
		Padding(0, 2).
		Width(70)

	var b strings.Builder
	b.WriteString(warn.Render("⚠  Workspace NOT verified"))
	b.WriteString("\n")
	b.WriteString(body.Render(strings.Join(wordWrap(
		"This assessment did not say which code tree to expect, so we could not "+
			"check that this workspace holds the right one. Continuing — but if the "+
			"task brief does not match the code here, stop and say so.", 64), "\n")))
	b.WriteString("\n")
	b.WriteString(dim.Render("this tree: " + o.Actual))
	fmt.Println(box.Render(b.String()))
	fmt.Println()
}

// diffBaseFor returns the commit a session's work is measured against.
//
// One helper rather than three call sites reading RepoCommit directly, because
// the bundle, the unified diff and the payload's BaseSha must agree: a diff
// taken against one base and shipped labelled with another is worse than no
// diff at all — it is a wrong one nobody can detect downstream.
func diffBaseFor(s Session) string {
	if strings.TrimSpace(s.DiffBaseCommit) != "" {
		return strings.TrimSpace(s.DiffBaseCommit)
	}
	return s.RepoCommit
}
