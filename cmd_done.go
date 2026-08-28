package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
	flag "github.com/spf13/pflag"
)

func cmdDone(args []string) {
	fs := flag.NewFlagSet("done", flag.ExitOnError)
	autoSubmit := fs.Bool("auto", false, "Auto-submit mode (skip the optional decision-notes notice)")
	fs.Parse(args) //nolint:errcheck

	session, err := loadSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// /explain is optional — queued decision notes never block submission.
	// Surface a one-line informational notice and move on.
	if !*autoSubmit {
		if pendingCount, err := pendingDecisionCount(); err == nil && pendingCount > 0 {
			fmt.Printf(
				"  %d decision note(s) queued — add commentary with `promptster explain` if you'd like (optional), submitting either way.\n",
				pendingCount,
			)
		}
	}

	// Device continuity check — fire-and-forget
	if session.SessionToken != "" {
		fp := collectDeviceFingerprint()
		if err := apiDeviceCheck(session.SessionToken, DeviceCheckRequest{
			Checkpoint:  "done",
			Fingerprint: fp,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: device check failed: %v\n", err)
		}
	}

	// Upload bundle + notify the API. When a workspace is present, this MUST
	// succeed before we mark the session complete — otherwise the candidate
	// thinks they shipped but no code reached us. Treat any failure path as
	// fatal and bail before /complete is called.
	//
	// EXCEPT one path, which is not a failure: the server has already closed this
	// assessment because the time limit expired, and its delivery window has
	// passed. Nothing broke — there is simply nowhere left to put the bytes. That
	// used to print "code submission failed — assessment NOT marked complete" and
	// exit 1, on a loop, at a candidate whose clock had merely run out.
	//
	// `uploadConfirmed` stays FALSE on that path, deliberately. It gates the
	// codespace teardown at the bottom of this function, and deleting a box whose
	// work never reached us destroys the only copy.
	uploadConfirmed := false
	closedByServer := false
	if session.TaskRoot != "" && session.SessionToken != "" {
		uploaded, closed := submitWorkspaceCode(session, *autoSubmit)
		closedByServer = closed
		if !uploaded && !closed {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "error: code submission failed — assessment NOT marked complete")
			fmt.Fprintln(os.Stderr, "  Fix the issue above and retry `promptster done --auto`.")
			os.Exit(1)
		}
		uploadConfirmed = uploaded
	}

	fmt.Println("Submitting your assessment...")
	resp, err := apiComplete(session.SessionID, session.Key)
	if err != nil {
		// Already closed by the server is the ordinary end of a timed assessment,
		// not an error. The response still carries `completedAt`, so the box below
		// prints the same thing it would have printed had we closed it ourselves.
		if !errors.Is(err, errAssessmentClosed) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		closedByServer = true
	}

	_ = deleteSession()
	removeShellHook()
	stopDecisionWatchers()
	stopGitWatcher()
	stopCodexWatcher()
	stopClaudeWatcher()
	purgeLegacyCodexProxyBlock() // heal a global codex config a pre-1.10 session left rewritten
	cleanupPromptsterState(session.TaskRoot)

	doneBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("10")).
		Padding(1, 2).
		Width(68)

	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")).Render("Assessment submitted!")
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	urlStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Underline(true)

	// Build results URL
	appURL := os.Getenv("PROMPTSTER_APP_URL")
	if appURL == "" {
		appURL = "https://promptster.ai"
	}
	resultsURL := fmt.Sprintf("%s/replay/%s?key=%s", appURL, session.SessionID, session.Key)

	details := fmt.Sprintf("%s  %s\n%s  %s",
		dim.Render("Session:  "), session.SessionID,
		dim.Render("Completed:"), resp.CompletedAt,
	)
	details += fmt.Sprintf("\n\n%s\n%s", dim.Render("Your results:"), urlStyle.Render(resultsURL))

	// Try to copy URL to clipboard
	copied := copyToClipboard(resultsURL)
	if copied {
		details += "\n\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓ Link copied to clipboard")
	}

	fmt.Println()
	fmt.Println(doneBox.Render(heading + "\n\n" + details))
	fmt.Println()
	// Truthful about the one case that matters: the assessment IS submitted, and
	// the very last edits did not make it. Saying nothing here would let a
	// candidate believe bytes were delivered that were not; saying "submission
	// failed" would be worse, because the assessment did complete.
	if closedByServer && !uploadConfirmed {
		fmt.Println(dim.Render(
			"Note: the time limit had already closed this assessment, so your most recent\n" +
				"local edits were not included. Everything captured before the deadline was."))
		fmt.Println()
	}
	fmt.Println("Thank you for completing the assessment.")
	fmt.Println(dim.Render("Your results will be available shortly."))
	// 2.4i: the question is no longer "which lane" but "does the candidate own
	// this machine and have to clean it up". In a box we provisioned, they do not.
	ours := seededSession(session)
	if session.TaskRoot != "" && !ours {
		fmt.Printf("\n%s %s\n",
			dim.Render("You can safely delete your workspace when you're done:"),
			lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render(session.TaskRoot))
	}
	if !ours {
		// Suppressed in a box we own: this warns that a PERSONAL shell has been
		// left carrying assessment proxy env, and there is no personal shell in a
		// disposable VM. Everything it protects — the marker fences, the sidecar
		// state, the legacy purge above — still runs.
		printShellProxyEnvClearHint()
	}

	// ⛔ 2.4i deleted the teardown that used to be the last thing here:
	// `gh codespace delete`, gated on a confirmed upload because deleting a box
	// whose work never reached us destroys the only copy.
	//
	// It is not replaced, and the gate it needed is gone with it. The box's
	// lifecycle belongs to the provisioner — `boxProvision.ts` pauses it and E2B's
	// configured lifecycle reaps it — so the CLI inside it has nothing to tear
	// down and no reason to hold the process open to try. A codespace was the
	// candidate's, billed to them, and had to be deleted from inside because
	// nothing else could reach it. This box is ours.
}

// copyToClipboard copies text to the system clipboard. Returns true on success.
func copyToClipboard(text string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		// Try xclip first, fall back to xsel
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("xsel"); err == nil {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		} else {
			return false
		}
	case "windows":
		cmd = exec.Command("cmd", "/c", "clip")
	default:
		return false
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}
