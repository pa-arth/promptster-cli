package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Palette ──────────────────────────────────────────────────────────────────

var (
	briefGreen  = lipgloss.Color("#22c55e")
	briefSky    = lipgloss.Color("#38bdf8")
	briefViolet = lipgloss.Color("#a78bfa")
	briefAmber  = lipgloss.Color("#f59e0b")
	briefRed    = lipgloss.Color("#ef4444")
	// briefCode: inline command text, legible on both backgrounds.
	briefCode = lipgloss.AdaptiveColor{Light: "#0369a1", Dark: "#7dd3fc"}
)

// phaseAccents cycles per phase card so each part of the assessment gets its
// own color identity.
var phaseAccents = []lipgloss.TerminalColor{briefGreen, briefSky, briefViolet, briefAmber}

const (
	briefMaxDocWidth = 84
	briefMinDocWidth = 40
)

// ── Document rendering (shared by the TUI viewport and static output) ───────

func sectionHeader(title string, accent lipgloss.TerminalColor, w int) string {
	glyph := lipgloss.NewStyle().Foreground(accent).Bold(true).Render("◆")
	label := lipgloss.NewStyle().Foreground(cStrong).Bold(true).Render(strings.ToUpper(title))
	head := glyph + " " + label + " "
	fill := w - lipgloss.Width(head)
	if fill < 0 {
		fill = 0
	}
	rule := lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("─", fill))
	return head + rule
}

// bulletItem renders a marker + wrapped body with a hanging indent.
func bulletItem(marker string, markerStyle, bodyStyle lipgloss.Style, body string, w int) string {
	m := markerStyle.Render(marker) + " "
	bw := w - lipgloss.Width(m)
	if bw < 10 {
		bw = 10
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m, bodyStyle.Width(bw).Render(body))
}

func bulletList(items []string, marker string, markerStyle, bodyStyle lipgloss.Style, w int) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, bulletItem(marker, markerStyle, bodyStyle, it, w))
	}
	return strings.Join(lines, "\n")
}

func renderCodebaseCard(c BriefCodebase, w int) string {
	inner := w - 6 // 2 border cols + 4 padding cols
	body := lipgloss.NewStyle().Foreground(cBody)
	label := lipgloss.NewStyle().Foreground(cMuted).Width(7)
	code := lipgloss.NewStyle().Foreground(briefCode)

	var rows []string
	if c.Name != "" {
		rows = append(rows, lipgloss.NewStyle().Foreground(cStrong).Bold(true).Render(c.Name))
	}
	if c.Description != "" {
		rows = append(rows, body.Width(inner).Render(c.Description))
	}
	if len(rows) > 0 && (len(c.Stack) > 0 || c.RunCommand != "" || c.TestCommand != "") {
		rows = append(rows, "")
	}
	if len(c.Stack) > 0 {
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top,
			label.Render("Stack"), body.Width(inner-7).Render(strings.Join(c.Stack, " · "))))
	}
	if c.RunCommand != "" {
		rows = append(rows, label.Render("Run")+code.Render(c.RunCommand))
	}
	if c.TestCommand != "" {
		rows = append(rows, label.Render("Test")+code.Render(c.TestCommand))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cDim).
		Padding(1, 2).
		Width(w - 2)
	return box.Render(strings.Join(rows, "\n"))
}

func renderPhaseCard(idx int, p BriefPhase, w int) string {
	accent := phaseAccents[idx%len(phaseAccents)]
	inner := w - 3 // thick left border + 2 padding

	accentStyle := lipgloss.NewStyle().Foreground(accent).Bold(true)
	strong := lipgloss.NewStyle().Foreground(cStrong).Bold(true)
	body := lipgloss.NewStyle().Foreground(cBody)
	muted := lipgloss.NewStyle().Foreground(cMuted)
	mutedItalic := muted.Italic(true)

	var rows []string
	title := accentStyle.Render(fmt.Sprintf("PART %d", idx+1)) +
		muted.Render(" · ") +
		strong.Render(strings.ToUpper(p.Name))
	rows = append(rows, title)
	if p.Goal != "" {
		rows = append(rows, body.Width(inner).Render(p.Goal))
	}
	if len(p.Tasks) > 0 {
		rows = append(rows, "", bulletList(p.Tasks, "▸", accentStyle, body, inner))
	}
	if len(p.Guidance) > 0 {
		rows = append(rows, "",
			muted.Bold(true).Render("What we're watching"),
			bulletList(p.Guidance, "·", mutedItalic, mutedItalic, inner))
	}

	card := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(accent).
		PaddingLeft(2)
	return card.Render(strings.Join(rows, "\n"))
}

// renderBriefDoc renders the full brief document (everything below the
// header) at the given width.
func renderBriefDoc(b Brief, s Session, w int) string {
	body := lipgloss.NewStyle().Foreground(cBody)
	muted := lipgloss.NewStyle().Foreground(cMuted)
	strong := lipgloss.NewStyle().Foreground(cStrong)
	amber := lipgloss.NewStyle().Foreground(briefAmber).Bold(true)
	code := lipgloss.NewStyle().Foreground(briefCode)

	var sections []string

	if b.Scenario != "" {
		sections = append(sections,
			sectionHeader("The situation", briefGreen, w)+"\n\n"+
				body.Width(w).Render(b.Scenario))
	}

	if !b.Codebase.isEmpty() {
		sections = append(sections,
			sectionHeader("The codebase", briefSky, w)+"\n\n"+
				renderCodebaseCard(b.Codebase, w))
	}

	if s.SetupInstructions != "" {
		sections = append(sections,
			sectionHeader("Setup", briefSky, w)+"\n\n"+
				body.Width(w).Render(s.SetupInstructions))
	}

	if len(b.Phases) > 0 {
		cards := make([]string, 0, len(b.Phases))
		for i, p := range b.Phases {
			cards = append(cards, renderPhaseCard(i, p, w))
		}
		sections = append(sections,
			sectionHeader("Your task", briefGreen, w)+"\n\n"+
				strings.Join(cards, "\n\n"))
	}

	if len(b.Evaluation) > 0 {
		sections = append(sections,
			sectionHeader("What we're evaluating", briefViolet, w)+"\n\n"+
				bulletList(b.Evaluation, "›", lipgloss.NewStyle().Foreground(briefViolet).Bold(true), body, w))
	}

	if len(b.GroundRules) > 0 {
		sections = append(sections,
			sectionHeader("Ground rules", briefAmber, w)+"\n\n"+
				bulletList(b.GroundRules, "▸", amber, body, w))
	}

	if len(b.Deliverables) > 0 {
		sections = append(sections,
			sectionHeader("Deliverables", briefGreen, w)+"\n\n"+
				bulletList(b.Deliverables, "☐", strong.Bold(true), body, w))
	}

	// Logistics tail — session facts, always last.
	var rows []string
	label := muted.Width(11)
	if s.TaskRoot != "" {
		rows = append(rows, label.Render("Workspace")+body.Render(s.TaskRoot))
		rows = append(rows, label.Render("Task file")+body.Render("TASK.md")+muted.Render(" — this brief, in your workspace"))
	}
	rows = append(rows, label.Render("Submit")+code.Render("promptster done")+muted.Render(" — when everything is committed"))
	rows = append(rows, label.Render("Rationale")+code.Render("promptster explain")+muted.Render(" — record why behind key decisions"))
	sections = append(sections, sectionHeader("Logistics", briefSky, w)+"\n\n"+strings.Join(rows, "\n"))

	return strings.Join(sections, "\n\n")
}

// ── Header / time bar ────────────────────────────────────────────────────────

func formatBriefDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func renderTimeBar(frac float64, cells int, color lipgloss.TerminalColor) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(cells) + 0.5)
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("░", cells-filled))
}

func renderTimeLine(s Session, now time.Time) string {
	muted := lipgloss.NewStyle().Foreground(cMuted)

	if s.TimeLimitMinutes <= 0 {
		return muted.Render("∞  no time limit — pace yourself")
	}
	total := time.Duration(s.TimeLimitMinutes) * time.Minute
	if s.StartedAt.IsZero() {
		return muted.Render("⏱  time limit: " + formatBriefDuration(total))
	}

	remaining := total - now.Sub(s.StartedAt)
	if remaining <= 0 {
		return lipgloss.NewStyle().Foreground(briefRed).Bold(true).Render("⏱  time limit exceeded") +
			muted.Render(" — wrap up and run promptster done")
	}

	frac := float64(remaining) / float64(total)
	color := lipgloss.TerminalColor(briefGreen)
	switch {
	case frac <= 0.15:
		color = briefRed
	case frac <= 0.35:
		color = briefAmber
	}
	return lipgloss.NewStyle().Foreground(color).Bold(true).Render("⏳ "+formatBriefDuration(remaining)+" left") +
		"  " + renderTimeBar(frac, 24, color) +
		"  " + muted.Render("of "+formatBriefDuration(total))
}

func renderBriefHeader(s Session, w int, now time.Time) string {
	brand := lipgloss.NewStyle().Foreground(briefGreen).Bold(true).Render("▍PROMPTSTER")
	sub := lipgloss.NewStyle().Foreground(cMuted).Render(" · Assessment Brief")
	left := brand + sub

	right := ""
	if s.OrgName != "" {
		right = lipgloss.NewStyle().Foreground(cMuted).Bold(true).Render(s.OrgName)
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	line1 := left + strings.Repeat(" ", gap) + right

	rule := lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("─", w))
	return line1 + "\n" + renderTimeLine(s, now) + "\n" + rule
}

// ── Bubbletea model ──────────────────────────────────────────────────────────

type briefTickMsg time.Time

func briefTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return briefTickMsg(t) })
}

type briefTUIModel struct {
	session Session
	brief   Brief
	vp      viewport.Model
	width   int
	height  int
	now     time.Time
	ready   bool
}

func newBriefTUIModel(s Session, b Brief) briefTUIModel {
	return briefTUIModel{session: s, brief: b, now: time.Now()}
}

func (m briefTUIModel) docWidth() int {
	w := m.width - 4 // 2 left margin + 2 breathing room
	if w > briefMaxDocWidth {
		w = briefMaxDocWidth
	}
	if w < briefMinDocWidth {
		w = briefMinDocWidth
	}
	return w
}

func (m briefTUIModel) headerView() string {
	return lipgloss.NewStyle().Margin(1, 0, 0, 2).Render(renderBriefHeader(m.session, m.docWidth(), m.now))
}

func (m briefTUIModel) footerView() string {
	w := m.docWidth()
	muted := lipgloss.NewStyle().Foreground(cMuted)
	rule := lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("─", w))

	hints := muted.Render("↑/↓ scroll · g/G top/end · q close")
	pct := muted.Render(fmt.Sprintf("%3.0f%%", m.vp.ScrollPercent()*100))
	gap := w - lipgloss.Width(hints) - lipgloss.Width(pct)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.NewStyle().Margin(0, 0, 0, 2).Render(rule + "\n" + hints + strings.Repeat(" ", gap) + pct)
}

func (m *briefTUIModel) resize() {
	headerH := lipgloss.Height(m.headerView())
	footerH := lipgloss.Height(m.footerView())
	vh := m.height - headerH - footerH
	if vh < 3 {
		vh = 3
	}
	doc := lipgloss.NewStyle().Padding(1, 0, 1, 2).Render(renderBriefDoc(m.brief, m.session, m.docWidth()))
	if !m.ready {
		m.vp = viewport.New(m.width, vh)
		m.ready = true
	} else {
		m.vp.Width = m.width
		m.vp.Height = vh
	}
	m.vp.SetContent(doc)
}

func (m briefTUIModel) Init() tea.Cmd {
	return briefTick()
}

func (m briefTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case briefTickMsg:
		m.now = time.Time(msg)
		return m, briefTick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m briefTUIModel) View() string {
	if !m.ready {
		return ""
	}
	return m.headerView() + "\n" + m.vp.View() + "\n" + m.footerView()
}

// runBriefTUI runs the live brief viewer in the current terminal.
func runBriefTUI(s Session, b Brief) error {
	model := newBriefTUIModel(s, b)
	opts := []tea.ProgramOption{tea.WithAltScreen()}

	// Spawned terminal windows sometimes wire stdin oddly; prefer the tty.
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		opts = append(opts, tea.WithInput(tty))
		defer tty.Close()
	}

	p := tea.NewProgram(model, opts...)
	_, err := p.Run()
	return err
}

// printBriefStatic renders the brief once, without the interactive viewer —
// used for piped output and as the no-TTY fallback.
func printBriefStatic(s Session, b Brief) {
	w := 76
	doc := renderBriefHeader(s, w, time.Now()) + "\n\n" + renderBriefDoc(b, s, w)
	fmt.Println()
	fmt.Println(lipgloss.NewStyle().MarginLeft(1).Render(doc))
	fmt.Println()
}
