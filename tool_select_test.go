package main

import (
	"reflect"
	"testing"
)

func TestParseToolsFlag(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"claude", []string{toolClaude}},
		{"codex", []string{toolCodex}},
		{"codex-cli", []string{toolCodex}},
		{"all", []string{toolClaude, toolCodex}},
		{"both", []string{toolClaude, toolCodex}}, // back-compat alias
		{"claude,codex", []string{toolClaude, toolCodex}},
		{"codex,claude", []string{toolClaude, toolCodex}}, // canonical order
		{"claude,claude", []string{toolClaude}},           // dedup
		{"bogus", nil},
		// Retired tokens parse to nothing — same as an unknown token here. The
		// difference between the two is made where it matters (resolveAllowedTools
		// and the selectTools warning), not in flag parsing.
		{"cursor", nil},
		{"cursor-cli", nil},
		{"claude,cursor", []string{toolClaude}},
	}
	for _, tc := range cases {
		got := parseToolsFlag(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseToolsFlag(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestIsRetiredToolToken(t *testing.T) {
	for _, tok := range []string{"cursor", "Cursor", " cursor ", "cursor-cli", "cursorcli"} {
		if !isRetiredToolToken(tok) {
			t.Errorf("isRetiredToolToken(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"claude", "codex", "bogus", ""} {
		if isRetiredToolToken(tok) {
			t.Errorf("isRetiredToolToken(%q) = true, want false", tok)
		}
	}
}

func TestResolveAllowedTools(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		want        []string
		wantRetired bool
	}{
		{"nil is unconstrained", nil, []string{toolClaude, toolCodex}, false},
		{"canonical order", []string{"codex", "claude"}, []string{toolClaude, toolCodex}, false},
		{"single", []string{"codex"}, []string{toolCodex}, false},
		{"dedup", []string{"claude", "claude"}, []string{toolClaude}, false},
		// Version skew with a newer server: fall back rather than lock the
		// candidate out over a token this CLI predates.
		{"only-unknown falls back", []string{"bogus"}, []string{toolClaude, toolCodex}, false},
		{"known plus unknown keeps known", []string{"claude", "bogus"}, []string{toolClaude}, false},
		// The case the retired/unknown split exists for: a cursor-only assessment
		// must NOT fall back to claude+codex.
		{"cursor-only is retired, not unknown", []string{"cursor"}, nil, true},
		{"cursor plus unknown is still retired", []string{"cursor", "bogus"}, nil, true},
		{"cursor alongside a live tool keeps the live tool", []string{"cursor", "claude"}, []string{toolClaude}, false},
		// A server that filtered the retired tool out before sending is telling us
		// the same thing.
		{"explicitly empty is retired-only", []string{}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, retired := resolveAllowedTools(tc.in)
			if !reflect.DeepEqual(got, tc.want) || retired != tc.wantRetired {
				t.Errorf("resolveAllowedTools(%#v) = (%#v, %v), want (%#v, %v)",
					tc.in, got, retired, tc.want, tc.wantRetired)
			}
		})
	}
}

func TestSelectTools(t *testing.T) {
	cases := []struct {
		name      string
		toolsFlag string
		allowed   []string
		want      []string
	}{
		{
			name:      "flag respected when allowed",
			toolsFlag: "claude",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude},
		},
		{
			name:      "flag requesting disallowed tool is dropped",
			toolsFlag: "claude,codex",
			allowed:   []string{"claude"},
			want:      []string{toolClaude}, // codex dropped (not allowed)
		},
		{
			name:      "single allowed tool auto-selects (no flag)",
			toolsFlag: "",
			allowed:   []string{"codex"},
			want:      []string{toolCodex},
		},
		{
			name:      "empty allowed falls back to all (no flag, non-interactive)",
			toolsFlag: "",
			allowed:   nil,
			want:      []string{toolClaude, toolCodex},
		},
		{
			name:      "flag fully disjoint from allowed → nil (error)",
			toolsFlag: "codex",
			allowed:   []string{"claude"},
			want:      nil,
		},
		{
			name:      "multiple allowed, no flag, non-interactive → all allowed",
			toolsFlag: "",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude, toolCodex},
		},
		// A cursor-only assessment stops. Falling back here would run it on
		// Claude Code and Codex with nobody told.
		{
			name:      "cursor-only assessment stops rather than falling back",
			toolsFlag: "",
			allowed:   []string{"cursor"},
			want:      nil,
		},
		{
			name:      "cursor-only assessment stops even with an explicit flag",
			toolsFlag: "claude",
			allowed:   []string{"cursor"},
			want:      nil,
		},
		// `--tools cursor` is warned about and ignored; the assessment's own
		// allowed set still decides.
		{
			name:      "--tools cursor is ignored, not fatal",
			toolsFlag: "cursor",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude, toolCodex},
		},
		{
			name:      "--tools claude,cursor keeps claude",
			toolsFlag: "claude,cursor",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectTools(tc.toolsFlag, tc.allowed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectTools(%q, %#v) = %#v, want %#v", tc.toolsFlag, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestToolsLabel(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "none"},
		{[]string{toolClaude}, "Claude Code"},
		{[]string{toolCodex}, "Codex CLI (beta)"},
		{[]string{toolCodex, toolClaude}, "Claude Code + Codex CLI (beta)"}, // canonical order
	}
	for _, tc := range cases {
		if got := toolsLabel(tc.in); got != tc.want {
			t.Errorf("toolsLabel(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
