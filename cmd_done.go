package main

import (
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
	uploadConfirmed := false
	if session.TaskRoot != "" && session.SessionToken != "" {
		if !submitWorkspaceCode(session, *autoSubmit) {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "error: code submission failed — assessment NOT marked complete")
			fmt.Fprintln(os.Stderr, "  Fix the issue above and retry `promptster done --auto`.")
			os.Exit(1)
		}
		uploadConfirmed = true
	}

	fmt.Println("Submitting your assessment...")
	resp, err := apiComplete(session.SessionID, session.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	_ = deleteSession()
	removeShellHook()
	stopDecisionWatchers()
	stopGitWatcher()
	stopCodexWatcher()
	stopClaudeWatcher()
	revertCodexProxy() // strip our block from ~/.codex/config.toml before state is wiped
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
	fmt.Println("Thank you for completing the assessment.")
	fmt.Println(dim.Render("Your results will be available shortly."))
	hosted := hostedLaneActive(session)
	if session.TaskRoot != "" && !hosted {
		fmt.Printf("\n%s %s\n",
			dim.Render("You can safely delete your workspace when you're done:"),
			lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render(session.TaskRoot))
	}
	if !hosted {
		// Suppressed on the hosted lane: this warns that a PERSONAL shell has been
		// left carrying assessment proxy env, and there is no personal shell in a
		// disposable VM that is about to be deleted. Everything it protects —
		// the marker fences, the sidecar state, revertCodexProxy above — still runs.
		printShellProxyEnvClearHint()
	}

	// LAST, and only now. Deleting the codespace tears down the process printing
	// this, so nothing may follow it, and it is gated on an upload that actually
	// succeeded: `submitWorkspaceCode` returning true and `/complete` returning.
	// Deleting a box whose work never reached us destroys the only copy.
	if hosted && uploadConfirmed && inCodespace() {
		printCodespaceWindDown(deleteHostingCodespace())
	}
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
