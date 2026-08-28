package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// gitExcludePathspecs scopes `git add` / `git diff` to everything except
// CLI-generated workspace files (TASK.md, .promptster/, .claude/ — hook
// settings + the /explain command the CLI installs) so the submission diff,
// periodic snapshots, and git-watcher file_diff events don't carry them.
// Candidate edits under .claude/ are still observable via the tool-call
// capture channels; only the git channels exclude them.
// Pathspec syntax requires at least one positive include (".") before the
// excludes — git ignores standalone exclude pathspecs.
//
// The excludes are emitted only for paths NOT already covered by .gitignore:
// `promptster start` appends both to the workspace .gitignore, and git (≤2.37
// at least) hard-errors on `git add` when an explicit pathspec names an
// ignored path — even an :(exclude) one.
func gitExcludePathspecs(root string) []string {
	specs := []string{"--", "."}
	for _, p := range []string{"TASK.md", ".promptster/", ".claude/"} {
		probe := strings.TrimSuffix(p, "/")
		cmd := exec.Command("git", "-C", root, "check-ignore", "-q", probe)
		if cmd.Run() == nil {
			continue // already gitignored — exclude pathspec is redundant and breaks git add
		}
		specs = append(specs, ":(exclude)"+p)
	}
	return specs
}

// secretAddExcludePathspecs keeps candidate-created credential files out of
// the index entirely: never staged means never in the diff, the snapshot's
// intent-to-add set (which would leak into git-watcher file_diff events), the
// file tree, or the bundle. Applied to `git add` ONLY — never to `git diff` —
// so files the upstream problem repo already tracks (a committed .env.test,
// testdata certs) keep flowing into the diff and the bundle the server runs
// tests against; tracked content is public anyway. Trade-off: a candidate
// creating .env.example as part of a fix loses it from the submission.
// Content-level secrets in other files are caught by the Titus scan in
// bundleWorkspace and redactBytes on diffs.
var secretAddExcludePathspecs = []string{
	":(exclude,glob)**/.env",
	":(exclude,glob)**/.env.*",
	":(exclude,glob)**/*.env",
	":(exclude,glob)**/id_rsa*",
	":(exclude,glob)**/id_dsa*",
	":(exclude,glob)**/id_ecdsa*",
	":(exclude,glob)**/id_ed25519*",
	":(exclude,glob)**/.netrc",
	":(exclude,glob)**/_netrc",
	":(exclude,glob)**/.git-credentials",
	":(exclude,glob)**/.pgpass",
	":(exclude,glob)**/.htpasswd",
	":(exclude,glob)**/.aws/credentials",
}

// gitAddExcludePathspecs is the pathspec set for `git add` calls: the shared
// excludes plus the secret-file excludes above.
func gitAddExcludePathspecs(root string) []string {
	return append(append([]string{}, gitExcludePathspecs(root)...), secretAddExcludePathspecs...)
}

// submitCodePayload is the body for POST /v1/candidate/submit-code.
// Files travel out-of-band via a Supabase Storage tarball — this payload
// just identifies the bundle and carries the diff for fast server-side
// analysis.
type submitCodePayload struct {
	SessionID    string   `json:"sessionId"`
	Diff         string   `json:"diff"`
	FileTree     []string `json:"fileTree"`
	RepoURL      string   `json:"repoUrl"`
	BaseSha      string   `json:"baseSha"`
	BundleKey    string   `json:"bundleKey"`
	BundleSize   int64    `json:"bundleSize"`
	BundleSHA256 string   `json:"bundleSha256"`
}

// submitWorkspaceCode bundles the candidate's workspace as a tar.gz, uploads
// it to Supabase Storage, and then notifies the API via /v1/candidate/submit-code.
// Returns true on success.
// submitWorkspaceCode bundles and delivers the candidate's workspace.
//
// Returns (uploaded, closedByServer). `uploaded` is true only when bytes were
// ACCEPTED, so it must never be true for a path where the work did not reach us.
// It used to gate the irreversible codespace teardown; 2.4i removed that
// teardown, and the rule stands on its own — `done` tells the candidate they
// submitted, and that sentence must be true. `closedByServer`
// says the refusal was the server's ordinary close of an expired assessment
// rather than a failure, which is the difference between telling the candidate
// "submitted" and telling them their work was lost.
func submitWorkspaceCode(session Session, autoSubmit bool) (bool, bool) {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	success := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))

	fmt.Printf("  %s Preparing submission...\n", dim.Render("●"))

	taskRoot := session.TaskRoot

	if !isGitRepository(taskRoot) {
		fmt.Fprintf(os.Stderr, "  error: workspace is not a git repository, cannot bundle\n")
		return false, false
	}

	// Stage everything so the bundle (git ls-files) reflects the candidate's
	// final working tree.
	addArgs := append([]string{"add", "-A"}, gitAddExcludePathspecs(taskRoot)...)
	if _, err := runCommand(taskRoot, "git", addArgs...); err != nil {
		fmt.Fprintf(os.Stderr, "  error: git add failed: %v\n", err)
		return false, false
	}
	// Commit for audit trail. Disable gpgsign + skip hooks: a candidate's global
	// commit.gpgsign=true or a repo pre-commit hook would otherwise fail this
	// silently. The diff below does NOT depend on this commit succeeding —
	// it diffs the working tree directly — so a failure here is non-fatal.
	//
	// ⛔ THIS USED TO BE SUPPRESSED ON THE HOSTED LANE, and 2.4i is what makes it
	// run everywhere. The suppression was a privacy mechanism, not a tidy-up
	// (design.md §2): the candidate had read-only access to the mirror, and GitHub
	// creates a PUBLIC FORK under their account when a commit is made from such a
	// codespace — a repo named after the assessment problem, on their profile,
	// disclosing their job search to their current employer.
	//
	// **That hazard is GitHub's, and it left with GitHub.** A provisioned box has
	// no `origin` we do not control, no account attached to the candidate, and
	// nothing that forks on commit. Keeping the suppression would have cost the
	// audit-trail commit on the lane that will carry most assessments, to protect
	// against a mechanism that is no longer in the picture.
	//
	// Restoring it also costs nothing, for the same reason suppressing it did:
	// the commit is best-effort by design. `git ls-files` reads the INDEX (after
	// `git add -A` above) and `git diff <base>` compares the WORKING TREE to the
	// base, so neither the bundle nor the diff has ever depended on a commit
	// existing.
	recordSubmissionCommit(taskRoot)

	// Unified diff: compare the *working tree* (== index after `git add -A`)
	// to the known base commit. Crucially this does NOT require the commit
	// above to have succeeded — staged-but-uncommitted changes still show up.
	// Falls back to HEAD~1 when there's no recorded base (BYO repos) and
	// HEAD~1 is reachable (skipped in --depth 1 clones).
	var diff string
	base := diffBaseFor(session)
	if base != "" {
		diffArgs := append([]string{"diff", base}, gitExcludePathspecs(taskRoot)...)
		diffOut, err := runCommand(taskRoot, "git", diffArgs...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: diff against %s failed: %v\n", base, err)
		} else {
			diff = string(diffOut)
		}
	} else if hasHeadParent(taskRoot) {
		diffArgs := append([]string{"diff", "HEAD~1"}, gitExcludePathspecs(taskRoot)...)
		diffOut, _ := runCommand(taskRoot, "git", diffArgs...)
		diff = string(diffOut)
	}
	// An empty diff is ambiguous: the candidate wrote nothing, or they wrote it
	// somewhere this bundle cannot see (a linked worktree, or a second clone made
	// by following setupInstructions). Those are opposite outcomes and must not
	// look identical, so go looking before accepting the empty result.
	if diff == "" {
		// Same base the diff and the bundle used, NOT session.RepoCommit. On an
		// adopted checkout RepoCommit is upstream's brokenSha and names no object
		// in the seeded tree, so isAssessmentCheckout's commit signal never fires
		// and its URL signal compares upstream against a different origin — the
		// guard would be silently inert on the one lane where a second checkout is
		// easiest to create. Off that lane diffBaseFor returns RepoCommit, so this
		// is the same value it always was.
		if stranded := detectStrandedWork(taskRoot, base, session.RepoURL); len(stranded) > 0 {
			reportStrandedWork(taskRoot, stranded)
			if !autoSubmit {
				return false, false
			}
			// --auto is the time-limit path. Refusing here would strand the
			// session open forever, so submit and let the loud block above stand
			// as the record of what happened.
			fmt.Fprintln(os.Stderr, "  --auto: submitting anyway so the time limit still closes the session.")
		} else {
			fmt.Fprintf(os.Stderr, "  warning: computed diff is empty — replay will show no modified files\n")
		}
	}

	// Build the tarball.
	fmt.Printf("  %s Bundling workspace...\n", dim.Render("●"))
	bundle, err := bundleWorkspace(taskRoot, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		return false, false
	}
	defer os.Remove(bundle.Path)

	// Request a signed PUT URL.
	urlResp, err := apiGetUploadURL(session.SessionToken, session.SessionID)
	if err != nil {
		// The server has already closed this assessment and the delivery window
		// has passed. Nothing here failed; there is simply nowhere to put the
		// bytes any more. Say so quietly and let the caller report the outcome —
		// `uploaded` stays FALSE, because nothing was accepted.
		if errors.Is(err, errAssessmentClosed) {
			return false, true
		}
		fmt.Fprintf(os.Stderr, "  error: could not get upload URL: %v\n", err)
		return false, false
	}

	// Upload the tarball.
	sizeMB := float64(bundle.Size) / (1024 * 1024)
	fmt.Printf("  %s Uploading bundle (%.1f MB, %d files)...\n",
		dim.Render("●"), sizeMB, bundle.FileCount)
	if err := uploadBundle(urlResp.UploadURL, bundle.Path, bundle.Size); err != nil {
		fmt.Fprintf(os.Stderr, "  error: upload failed: %v\n", err)
		return false, false
	}

	// Notify the API.
	payload := submitCodePayload{
		SessionID: session.SessionID,
		// The diff feeds github_diff_v1 and the replay viewer — scrub it like
		// any other upload. (The tarball bundle uploads verbatim; see PR #next.)
		Diff:         string(redactBytes([]byte(diff))),
		FileTree:     bundle.FileTree,
		RepoURL:      session.RepoURL,
		BaseSha:      base,
		BundleKey:    urlResp.ObjectKey,
		BundleSize:   bundle.Size,
		BundleSHA256: bundle.SHA256Hex,
	}
	if err := apiSubmitCode(session.SessionToken, payload); err != nil {
		if errors.Is(err, errAssessmentClosed) {
			return false, true
		}
		fmt.Fprintf(os.Stderr, "  error: submit-code failed: %v\n", err)
		return false, false
	}

	fmt.Printf("  %s Code submitted (%d files, %.1f MB)\n",
		success.Render("✓"), bundle.FileCount, sizeMB)
	return true, false
}

// hasHeadParent returns true when HEAD has a reachable parent commit.
// In a --depth 1 clone of the base sha, HEAD~1 is unreachable until the
// candidate makes a commit on top of it, so we have to gate any HEAD~1
// diff on this check.
func hasHeadParent(taskRoot string) bool {
	cmd := exec.Command("git", "-C", taskRoot, "rev-parse", "--verify", "--quiet", "HEAD~1")
	return cmd.Run() == nil
}

// apiSubmitCode notifies the backend that the candidate's tarball is uploaded.
func apiSubmitCode(token string, payload submitCodePayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf("%s/v1/candidate/submit-code", apiURL())
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if apiErrorCode(body) == "assessment_closed" {
			return errAssessmentClosed
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// recordSubmissionCommit makes the audit-trail commit.
//
// It no longer takes the session, and no longer returns whether it ran: the one
// caller that made it conditional was the Codespaces fork hazard, retired by
// 2.4i. See the comment at the call site for why that is safe rather than merely
// simpler.
func recordSubmissionCommit(taskRoot string) {
	if _, err := runCommand(taskRoot,
		"git",
		"-c", "user.name=Promptster Candidate",
		"-c", "user.email=candidate@promptster.local",
		"-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "--no-verify", "-m", "fix: assessment submission",
	); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: git commit failed (continuing — diff is taken from working tree): %v\n", err)
	}
}
