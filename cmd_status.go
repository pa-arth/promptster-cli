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

	statusLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Width(12)

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
			"sessionId":      session.SessionID,
			"startedAt":      session.StartedAt.UTC().Format(time.RFC3339),
			"elapsedSeconds":  int(elapsed.Seconds()),
			"timeLimitMinutes": session.TimeLimitMinutes,
			"taskRoot":        session.TaskRoot,
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
	if session.TaskRoot != "" {
		rows = append(rows, row("Claude hooks:", filepath.Join(session.TaskRoot, ".claude", "settings.local.json")))
	}

	nudge := loadNudgeState()
	if !nudge.LastExplainAt.IsZero() {
		sinceExplain := time.Since(nudge.LastExplainAt).Round(time.Second)
		rows = append(rows, row("Last explain:", sinceExplain.String()+" ago"))
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
