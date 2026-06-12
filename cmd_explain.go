package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	flag "github.com/spf13/pflag"
)

// RecentActivityResponse is the response from GET /v1/sessions/:id/recent-activity.
type RecentActivityResponse struct {
	Summary        string `json:"summary"`
	RecentActivity struct {
		FilesChanged []string `json:"filesChanged"`
		Commands     []string `json:"commands"`
		PromptCount  int      `json:"promptCount"`
	} `json:"recentActivity"`
	PeriodMinutes int `json:"periodMinutes"`
	EventCount    int `json:"eventCount"`
}

func apiGetRecentActivity(sessionID, apiKey string, minutes int) (*RecentActivityResponse, error) {
	url := fmt.Sprintf("%s/v1/sessions/%s/recent-activity?minutes=%d", apiURL(), sessionID, minutes)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result RecentActivityResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func cmdExplain(args []string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	lastFlag := fs.String("last", "20m", "Look-back window (e.g. 15m, 30m)")
	quiet := fs.Bool("quiet", false, "Suppress the activity box and ANSI styling (used by the /explain slash command so Claude's context stays clean)")
	fs.Parse(args) //nolint:errcheck

	session, err := loadSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Check if rationale was passed directly: promptster explain "my rationale"
	remaining := fs.Args()
	directRationale := strings.TrimSpace(strings.Join(remaining, " "))

	dur, err := time.ParseDuration(*lastFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid duration %q: %v\n", *lastFlag, err)
		os.Exit(1)
	}
	minutes := int(dur.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 60 {
		minutes = 60
	}

	// Fetch recent activity from the backend (non-fatal if it fails)
	var activity *RecentActivityResponse
	var activitySummary string
	fetchedActivity, fetchErr := apiGetRecentActivity(session.SessionID, session.SessionToken, minutes)
	if fetchErr != nil {
		if !*quiet {
			dim := lipgloss.NewStyle().Foreground(cDim)
			fmt.Println(dim.Render(fmt.Sprintf("  Could not fetch recent activity: %v", fetchErr)))
			fmt.Println()
		}
		activitySummary = "Activity context unavailable"
	} else {
		activity = fetchedActivity
		activitySummary = activity.Summary
		if activity.EventCount > 0 && !*quiet {
			displayActivitySummary(activity)
		}
	}

	var rationale string
	if directRationale != "" {
		// Used as one-liner: promptster explain "chose recursive approach because..."
		rationale = directRationale
	} else if *quiet {
		// Quiet mode is non-interactive (slash command). With no inline rationale
		// there is nothing to capture — bail rather than open a TUI that would hang.
		fmt.Println("No rationale provided. Usage: /explain <what you decided and why>")
		return
	} else {
		// Interactive TUI
		var tuiErr error
		rationale, tuiErr = runExplainTUI()
		if tuiErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", tuiErr)
			os.Exit(1)
		}
	}

	if strings.TrimSpace(rationale) == "" {
		fmt.Println("  No rationale provided. Skipping.")
		return
	}

	// Build and send the decision event
	record := decisionCaptureRecord{
		Title:             "Decision rationale",
		Context:           activitySummary,
		ImpactScore:       3,
		ChosenOption:      "",
		Tradeoffs:         "",
		Rationale:         rationale,
		DecisionID:        newUUID(),
		SessionID:         session.SessionID,
		SourceService:     "cli",
		CapturedVia:       "cli-explain",
		TriggerKind:       "explain",
		RationalePrompted: true,
	}

	if err := persistDecisionCapture(record); err != nil {
		fmt.Fprintf(os.Stderr, "error saving decision: %v\n", err)
		os.Exit(1)
	}

	// Update nudge timer
	recordExplain()

	if *quiet {
		fmt.Println("Decision rationale captured.")
		return
	}
	success := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	fmt.Printf("  %s Decision rationale captured.\n", success.Render("✓"))
}

func displayActivitySummary(activity *RecentActivityResponse) {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(70)

	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("12"))

	var lines []string
	lines = append(lines, heading.Render(fmt.Sprintf("Recent Activity (last %d min)", activity.PeriodMinutes)))
	lines = append(lines, "")
	lines = append(lines, dim.Render(activity.Summary))

	if len(activity.RecentActivity.FilesChanged) > 0 {
		lines = append(lines, "")
		lines = append(lines, heading.Render("Files changed:"))
		for _, f := range activity.RecentActivity.FilesChanged {
			if len(lines) > 15 {
				lines = append(lines, dim.Render(fmt.Sprintf("  ... and %d more", len(activity.RecentActivity.FilesChanged)-10)))
				break
			}
			lines = append(lines, accent.Render("  "+f))
		}
	}

	if len(activity.RecentActivity.Commands) > 0 {
		lines = append(lines, "")
		lines = append(lines, heading.Render("Commands:"))
		shown := activity.RecentActivity.Commands
		if len(shown) > 5 {
			shown = shown[len(shown)-5:]
		}
		for _, c := range shown {
			lines = append(lines, dim.Render("  $ "+c))
		}
	}

	fmt.Println()
	fmt.Println(box.Render(strings.Join(lines, "\n")))
	fmt.Println()
}

// explainTUIModel is the bubbletea model for the explain rationale input.
type explainTUIModel struct {
	textarea textarea.Model
	done     bool
	quit     bool
}

func newExplainTUIModel() explainTUIModel {
	ta := textarea.New()
	ta.Placeholder = "Explain your reasoning: what did you decide and why?"
	ta.CharLimit = 2000
	ta.SetWidth(60)
	ta.SetHeight(6)
	ta.ShowLineNumbers = false
	ta.Focus()

	return explainTUIModel{
		textarea: ta,
	}
}

func (m explainTUIModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m explainTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+d":
			m.done = true
			return m, tea.Quit
		case "esc", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m explainTUIModel) View() string {
	if m.done || m.quit {
		return ""
	}

	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62"))

	return heading.Render("What decisions did you make and why?") + "\n\n" +
		m.textarea.View() + "\n\n" +
		hint.Render("Ctrl+D submit  ·  Esc cancel")
}

func runExplainTUI() (string, error) {
	model := newExplainTUIModel()

	opts := []tea.ProgramOption{}

	// Try /dev/tty for input if stdin isn't a terminal
	ttyFile, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if ttyErr == nil {
		opts = append(opts, tea.WithInput(ttyFile))
		defer ttyFile.Close()
	}

	p := tea.NewProgram(model, opts...)
	finalModel, err := p.Run()
	if err != nil {
		return "", err
	}

	final := finalModel.(explainTUIModel)
	if final.quit {
		return "", nil
	}
	return strings.TrimSpace(final.textarea.Value()), nil
}
