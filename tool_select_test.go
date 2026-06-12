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
		{"cursor", []string{toolCursor}},
		{"codex-cli", []string{toolCodex}},
		{"cursor-cli", []string{toolCursor}},
		{"all", []string{toolClaude, toolCodex, toolCursor}},
		{"both", []string{toolClaude, toolCodex}}, // back-compat alias
		{"claude,cursor", []string{toolClaude, toolCursor}},
		{"cursor,claude", []string{toolClaude, toolCursor}}, // canonical order
		{"codex, cursor , claude", []string{toolClaude, toolCodex, toolCursor}},
		{"claude,claude", []string{toolClaude}}, // dedup
		{"bogus", nil},
		{"cursor,bogus", []string{toolCursor}},
	}
	for _, tc := range cases {
		got := parseToolsFlag(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseToolsFlag(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestResolveAllowedTools(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, []string{toolClaude, toolCodex, toolCursor}},               // unconstrained
		{[]string{}, []string{toolClaude, toolCodex, toolCursor}},        // unconstrained
		{[]string{"cursor", "claude"}, []string{toolClaude, toolCursor}}, // canonical order
		{[]string{"codex"}, []string{toolCodex}},
		{[]string{"bogus"}, []string{toolClaude, toolCodex, toolCursor}}, // only-unknown → unconstrained
		{[]string{"claude", "bogus"}, []string{toolClaude}},
		{[]string{"claude", "claude"}, []string{toolClaude}}, // dedup
	}
	for _, tc := range cases {
		got := resolveAllowedTools(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("resolveAllowedTools(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
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
			toolsFlag: "claude,cursor",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude}, // cursor dropped (not allowed)
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
			want:      []string{toolClaude, toolCodex, toolCursor},
		},
		{
			name:      "nil allowed with flag respects flag",
			toolsFlag: "cursor",
			allowed:   nil,
			want:      []string{toolCursor},
		},
		{
			name:      "flag fully disjoint from allowed → nil (error)",
			toolsFlag: "cursor",
			allowed:   []string{"claude", "codex"},
			want:      nil,
		},
		{
			name:      "multiple allowed, no flag, non-interactive → all allowed",
			toolsFlag: "",
			allowed:   []string{"claude", "codex"},
			want:      []string{toolClaude, toolCodex},
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
		{[]string{toolCursor}, "Cursor (beta)"},
		{[]string{toolCursor, toolClaude}, "Claude Code + Cursor (beta)"}, // canonical order
		{[]string{toolClaude, toolCodex, toolCursor}, "Claude Code + Codex CLI (beta) + Cursor (beta)"},
	}
	for _, tc := range cases {
		if got := toolsLabel(tc.in); got != tc.want {
			t.Errorf("toolsLabel(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
