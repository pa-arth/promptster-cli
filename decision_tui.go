package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type tuiState int

const (
	statePrompt tuiState = iota
	stateInput
	stateDone
)

type tuiResult struct {
	Action    string // "capture", "skip", "later", "quit"
	Rationale string
}

type decisionTUIModel struct {
	candidates []decisionCandidate
	index      int
	state      tuiState
	textarea   textarea.Model
	results    []tuiResult
	err        error
	width      int
	height     int
}

// Styles
var (
	tuiBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(1, 2).
			Width(60)

	tuiTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("62"))

	tuiLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Width(12)

	tuiValueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("255"))

	tuiHintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Italic(true)

	tuiImpactFull  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render("\u2588")
	tuiImpactEmpty = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("\u2591")
)

func renderImpactBar(score int) string {
	s := sanitizeImpact(score)
	var parts []string
	for i := 1; i <= 5; i++ {
		if i <= s {
			parts = append(parts, tuiImpactFull)
		} else {
			parts = append(parts, tuiImpactEmpty)
		}
	}
	return strings.Join(parts, "") + fmt.Sprintf(" %d/5", s)
}

func newDecisionTUIModel(candidates []decisionCandidate) decisionTUIModel {
	ta := textarea.New()
	ta.Placeholder = "Explain why you made this choice..."
	ta.CharLimit = 2000
	ta.SetWidth(50)
	ta.SetHeight(5)
	ta.ShowLineNumbers = false

	return decisionTUIModel{
		candidates: candidates,
		index:      0,
		state:      statePrompt,
		textarea:   ta,
		results:    make([]tuiResult, 0, len(candidates)),
	}
}

func (m decisionTUIModel) Init() tea.Cmd {
	return nil
}

func (m decisionTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case statePrompt:
			return m.handlePromptKey(msg)
		case stateInput:
			return m.handleInputKey(msg)
		}
	}

	if m.state == stateInput {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m decisionTUIModel) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.state = stateInput
		m.textarea.Reset()
		m.textarea.Focus()
		return m, m.textarea.Focus()
	case "n", "N":
		m.results = append(m.results, tuiResult{Action: "skip"})
		return m.advance()
	case "l", "L":
		m.results = append(m.results, tuiResult{Action: "later"})
		return m.advance()
	case "q", "Q", "ctrl+c":
		m.results = append(m.results, tuiResult{Action: "quit"})
		m.state = stateDone
		return m, tea.Quit
	}
	return m, nil
}

func (m decisionTUIModel) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+d":
		rationale := strings.TrimSpace(m.textarea.Value())
		if rationale == "" {
			return m, nil
		}
		m.results = append(m.results, tuiResult{Action: "capture", Rationale: rationale})
		return m.advance()
	case "esc":
		m.state = statePrompt
		return m, nil
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m decisionTUIModel) advance() (tea.Model, tea.Cmd) {
	m.index++
	if m.index >= len(m.candidates) {
		m.state = stateDone
		return m, tea.Quit
	}
	m.state = statePrompt
	return m, nil
}

func (m decisionTUIModel) View() string {
	if m.state == stateDone || m.index >= len(m.candidates) {
		return ""
	}

	candidate := m.candidates[m.index]

	switch m.state {
	case statePrompt:
		return m.renderPromptView(candidate)
	case stateInput:
		return m.renderInputView(candidate)
	default:
		return ""
	}
}

func (m decisionTUIModel) renderPromptView(c decisionCandidate) string {
	header := tuiTitleStyle.Render("Decision Detected")

	row := func(label, value string) string {
		return tuiLabelStyle.Render(label) + tuiValueStyle.Render(value)
	}

	ctx := c.Context
	if len(ctx) > 60 {
		ctx = ctx[:57] + "..."
	}
	chosen := c.ChosenOption
	if len(chosen) > 60 {
		chosen = chosen[:57] + "..."
	}

	rows := []string{
		"",
		tuiValueStyle.Bold(true).Render(c.Title),
		"",
		row("Context   ", ctx),
		row("Chosen    ", chosen),
		row("Impact    ", renderImpactBar(c.ImpactScore)),
	}
	if c.CategoryHint != "" {
		rows = append(rows, row("Category  ", c.CategoryHint))
	}

	counter := fmt.Sprintf("%d/%d", m.index+1, len(m.candidates))
	footer := tuiHintStyle.Render(fmt.Sprintf("[Y] Capture  [N] Skip  [L] Later  [Q] Quit   %s", counter))
	rows = append(rows, "", footer)

	content := header + "\n" + strings.Join(rows, "\n")
	return "\n" + tuiBoxStyle.Render(content) + "\n"
}

func (m decisionTUIModel) renderInputView(c decisionCandidate) string {
	title := c.Title
	if len(title) > 40 {
		title = title[:37] + "..."
	}
	header := tuiTitleStyle.Render("Capturing: " + title)

	content := header + "\n\n" +
		tuiValueStyle.Render("Why did you make this choice?") + "\n\n" +
		m.textarea.View() + "\n\n" +
		tuiHintStyle.Render("Ctrl+D submit  \u00b7  Esc cancel")

	return "\n" + tuiBoxStyle.Render(content) + "\n"
}

// runDecisionTUI runs the Bubbletea TUI for a list of decision candidates.
// It returns the list of candidates that should remain queued (skipped/later/quit).
func runDecisionTUI(candidates []decisionCandidate) (captured int, remaining []decisionCandidate, err error) {
	if len(candidates) == 0 {
		return 0, nil, nil
	}

	model := newDecisionTUIModel(candidates)

	opts := []tea.ProgramOption{tea.WithAltScreen()}

	// When called from a hook context, stdin may not be a terminal.
	// Try to use /dev/tty for input.
	ttyFile, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if ttyErr == nil {
		opts = append(opts, tea.WithInput(ttyFile))
		defer ttyFile.Close()
	}

	p := tea.NewProgram(model, opts...)
	finalModel, err := p.Run()
	if err != nil {
		return 0, candidates, err
	}

	final := finalModel.(decisionTUIModel)

	// Process results
	for i, result := range final.results {
		if i >= len(candidates) {
			break
		}
		switch result.Action {
		case "capture":
			record := decisionCaptureRecord{
				Title:             candidates[i].Title,
				Context:           candidates[i].Context,
				ImpactScore:       sanitizeImpact(candidates[i].ImpactScore),
				ChosenOption:      candidates[i].ChosenOption,
				Tradeoffs:         candidates[i].TradeoffsHint,
				Rationale:         result.Rationale,
				DecisionID:        candidates[i].ID,
				SessionID:         candidates[i].SessionID,
				SourceService:     firstNonEmpty(candidates[i].Source, "hook"),
				CapturedVia:       "tui-decide",
				TriggerEventID:    candidates[i].TriggerEventID,
				TriggerKind:       candidates[i].TriggerKind,
				CategoryHint:      candidates[i].CategoryHint,
				InstructionLabel:  candidates[i].InstructionLabel,
				RationalePrompted: true,
			}
			if persistErr := persistDecisionCapture(record); persistErr != nil {
				// Keep in queue if persist fails
				remaining = append(remaining, candidates[i])
			} else {
				captured++
			}
		case "skip":
			// Remove from queue (don't add to remaining)
		case "later":
			remaining = append(remaining, candidates[i])
		case "quit":
			// Keep this and all subsequent candidates in the queue
			remaining = append(remaining, candidates[i:]...)
			return captured, remaining, nil
		}
	}

	// Any candidates not processed (shouldn't happen) stay queued
	if len(final.results) < len(candidates) {
		remaining = append(remaining, candidates[len(final.results):]...)
	}

	return captured, remaining, nil
}
