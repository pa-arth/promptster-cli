package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestResolveBriefLegacyFallback(t *testing.T) {
	s := Session{TaskBrief: "Fix the flaky login test."}
	b := resolveBrief(s)
	if b.Scenario != s.TaskBrief {
		t.Fatalf("legacy TaskBrief should land in Scenario, got %q", b.Scenario)
	}
	if len(b.Phases) != 0 {
		t.Fatalf("legacy brief should have no phases, got %d", len(b.Phases))
	}
}

func TestResolveBriefPrefersStructured(t *testing.T) {
	structured := &Brief{Scenario: "structured", Phases: []BriefPhase{{Name: "Build"}}}
	s := Session{TaskBrief: "legacy", Brief: structured}
	b := resolveBrief(s)
	if b.Scenario != "structured" || len(b.Phases) != 1 {
		t.Fatalf("structured brief should win over TaskBrief, got %+v", b)
	}
}

func TestResolveBriefIgnoresEmptyStructured(t *testing.T) {
	s := Session{TaskBrief: "legacy", Brief: &Brief{}}
	if b := resolveBrief(s); b.Scenario != "legacy" {
		t.Fatalf("empty structured brief should fall back to TaskBrief, got %+v", b)
	}
}

func TestRenderBriefDocSections(t *testing.T) {
	session, brief := demoBriefSession()
	doc := renderBriefDoc(brief, session, 80)

	for _, want := range []string{
		"THE SITUATION", "THE CODEBASE", "YOUR TASK",
		"PART 1", "PART 2",
		"WHAT WE'RE EVALUATING", "GROUND RULES", "DELIVERABLES", "LOGISTICS",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("rendered brief missing section %q", want)
		}
	}
}

func TestRenderBriefDocLegacyOnly(t *testing.T) {
	s := Session{TaskBrief: "Do the thing.", TaskRoot: "/tmp/ws", TimeLimitMinutes: 60}
	doc := renderBriefDoc(resolveBrief(s), s, 80)
	if !strings.Contains(doc, "Do the thing.") {
		t.Error("legacy brief text missing from rendered doc")
	}
	if !strings.Contains(doc, "promptster done") {
		t.Error("logistics submit hint missing from rendered doc")
	}
}

func TestRenderTimeLine(t *testing.T) {
	now := time.Now()

	noLimit := renderTimeLine(Session{}, now)
	if !strings.Contains(noLimit, "no time limit") {
		t.Errorf("expected no-limit message, got %q", noLimit)
	}

	running := renderTimeLine(Session{TimeLimitMinutes: 90, StartedAt: now.Add(-30 * time.Minute)}, now)
	if !strings.Contains(running, "1h 00m left") {
		t.Errorf("expected remaining time, got %q", running)
	}

	exceeded := renderTimeLine(Session{TimeLimitMinutes: 30, StartedAt: now.Add(-time.Hour)}, now)
	if !strings.Contains(exceeded, "exceeded") {
		t.Errorf("expected exceeded message, got %q", exceeded)
	}
}

func TestFormatBriefDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:            "<1m",
		5 * time.Minute:             "5m",
		90 * time.Minute:            "1h 30m",
		2*time.Hour + 5*time.Minute: "2h 05m",
	}
	for in, want := range cases {
		if got := formatBriefDuration(in); got != want {
			t.Errorf("formatBriefDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBriefTUISmoke(t *testing.T) {
	session, brief := demoBriefSession()
	m := newBriefTUIModel(session, brief)

	// Before the first WindowSizeMsg the view must be empty, not panic.
	if v := m.View(); v != "" {
		t.Errorf("expected empty view before sizing, got %d bytes", len(v))
	}

	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(briefTUIModel)
	view := m.View()
	for _, want := range []string{"PROMPTSTER", "Acme Robotics", "left", "scroll"} {
		if !strings.Contains(view, want) {
			t.Errorf("sized view missing %q", want)
		}
	}

	// Tick advances the clock and reschedules.
	next, cmd := m.Update(briefTickMsg(time.Now()))
	m = next.(briefTUIModel)
	if cmd == nil {
		t.Error("tick should reschedule itself")
	}

	// Quit keys terminate the program.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should quit")
	}

	// Tiny terminal must not panic.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	_ = next.(briefTUIModel).View()
}

func TestShellQuoteArg(t *testing.T) {
	if got := shellQuoteArg("/path/with 'quote'"); got != `'/path/with '\''quote'\'''` {
		t.Errorf("unexpected quoting: %s", got)
	}
}
