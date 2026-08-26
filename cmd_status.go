package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	statusBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(1, 2).
			Width(62)

	// 14, not 12: the longest label is "Claude hooks:" at 13 characters, and at
	// Width(12) lipgloss wrapped it onto two lines mid-word ("Claude" / "hooks:")
	// with the value trailing after. Observed on the hosted lane 2026-08-26.
	statusLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Width(14)

	statusValueStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("255"))
)

func cmdStatus(args []string) {
	jsonOutput := false
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
		}
	}

	session, err := loadSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	elapsed := time.Since(session.StartedAt).Round(time.Second)

	if jsonOutput {
		out := map[string]interface{}{
			"sessionId":        session.SessionID,
			"startedAt":        session.StartedAt.UTC().Format(time.RFC3339),
			"elapsedSeconds":   int(elapsed.Seconds()),
			"timeLimitMinutes": session.TimeLimitMinutes,
			"taskRoot":         session.TaskRoot,
		}

		if session.TimeLimitMinutes > 0 {
			remaining := time.Duration(session.TimeLimitMinutes)*time.Minute - elapsed
			if remaining < 0 {
				remaining = 0
			}
			out["remainingSeconds"] = int(remaining.Seconds())
		}

		nudge := loadNudgeState()
		if !nudge.LastExplainAt.IsZero() {
			out["lastExplainSecondsAgo"] = int(time.Since(nudge.LastExplainAt).Seconds())
		}

		sessionData, err := apiGetSession(session.SessionID, session.SessionToken)
		if err == nil {
			if count, ok := sessionData["eventCount"].(float64); ok {
				out["eventCount"] = int(count)
			}
			if status, ok := sessionData["status"].(string); ok {
				out["status"] = status
			}
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	// Human-readable output (original).
	row := func(label, value string) string {
		return statusLabelStyle.Render(label) + statusValueStyle.Render(value)
	}

	rows := []string{
		row("Session ID:", session.SessionID),
		row("Started:", session.StartedAt.Local().Format("2006-01-02 15:04:05")),
		row("Elapsed:", elapsed.String()),
	}
	if value, show := claudeHooksStatus(session.TaskRoot, session.Tools); show {
		rows = append(rows, row("Claude hooks:", value))
	}

	nudge := loadNudgeState()
	if !nudge.LastExplainAt.IsZero() {
		sinceExplain := time.Since(nudge.LastExplainAt).Round(time.Second)
		rows = append(rows, row("Last explain:", sinceExplain.String()+" ago"))
	}

	if hasTool(session.Tools, toolCodex) {
		// Codex capture is a background daemon, and its failure mode is silence:
		// a watcher left running by a PREVIOUS session keeps the liveness check
		// satisfied while matching every rollout against the old workspace, so
		// nothing is captured and nothing says so. Report ownership, not just
		// liveness — the distinction is the whole bug.
		rows = append(rows, row("Codex:", codexCaptureStatus(session)))
	}

	if session.TimeLimitMinutes > 0 {
		remaining := time.Duration(session.TimeLimitMinutes)*time.Minute - elapsed
		if remaining < 0 {
			remaining = 0
		}
		rows = append(rows, row("Remaining:", remaining.Round(time.Second).String()))
	}

	// Try to fetch live event count and status from the API.
	sessionData, err := apiGetSession(session.SessionID, session.SessionToken)
	if err == nil {
		if count, ok := sessionData["eventCount"].(float64); ok {
			rows = append(rows, row("Events:", fmt.Sprintf("%.0f", count)))
		}
		if status, ok := sessionData["status"].(string); ok {
			rows = append(rows, row("Status:", status))
		}
	}

	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("Assessment Status")
	content := header + "\n\n" + strings.Join(rows, "\n")

	fmt.Println()
	fmt.Println(statusBoxStyle.Render(content))
}

// codexCaptureStatus describes the codex rollout watcher for THIS session in one
// line: running for us, running for someone else, or not running at all.
func codexCaptureStatus(session Session) string {
	state, alive := isCodexWatcherRunning()
	switch {
	case !alive:
		return "not running — codex work is NOT being recorded (fix: promptster start)"
	case !codexWatcherOwns(state, session):
		return fmt.Sprintf("pid %d belongs to another session — codex work is NOT being recorded (fix: promptster start)", state.PID)
	default:
		return fmt.Sprintf("watching (pid %d, %d events sent)", state.PID, state.EventsSent)
	}
}

// claudeHooksStatus reports what the Claude hook file's state actually IS,
// rather than the path it would have been written to.
//
// The row it feeds used to be gated on `taskRoot != ""` alone, so it printed a
// path unconditionally. That is wrong in both directions:
//
//   - `cmd_start.go` writes the file only when Claude is one of the instrumented
//     tools (`if useClaude`, from the same hasTool predicate used here). On a
//     Codex-only assessment the file is deliberately never written, and the old
//     row announced a Claude hooks path anyway — observed 2026-08-26 on a hosted
//     box, where the panel read as "Claude capture is configured" on a session
//     with no Claude wiring at all.
//   - Even when Claude IS instrumented, configureProxyEnv only WARNS if the write
//     fails. So "instrumented but unconfigured" is reachable, and printing the
//     intended path makes it indistinguishable from success.
//
// Hence: stat the file. A path is shown only when there is a file at it.
//
// The order of the checks matters for sessions started by an older CLI, whose
// persisted session.json predates `Tools` and so carries none. Those would
// report as "no Claude" if the tool list were consulted first, hiding a hook
// file that is really there — so an existing file wins outright, and the tool
// list is consulted only to decide whether an ABSENT file is worth reporting.
func claudeHooksStatus(taskRoot string, tools []string) (string, bool) {
	if taskRoot == "" {
		return "", false
	}
	path := filepath.Join(taskRoot, ".claude", "settings.local.json")
	if _, err := os.Stat(path); err == nil {
		return path, true
	}
	if hasTool(tools, toolClaude) {
		// Claude is instrumented and the file is not there: the one case worth
		// saying out loud, because it is capture silently not happening.
		return "not written (expected " + path + ")", true
	}
	// Claude is not instrumented for this session. Nothing was meant to be
	// written and nothing was; a row here would only invent a problem.
	return "", false
}
