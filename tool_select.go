package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// AI tool identifiers Promptster can instrument.
const (
	toolClaude = "claude"
	toolCodex  = "codex"
	toolCursor = "cursor"
)

// allTools is the canonical ordering used when "all" is selected and when
// rendering labels, so output is deterministic regardless of input order.
var allTools = []string{toolClaude, toolCodex, toolCursor}

// normalizeToolToken maps a single user-supplied token to a canonical tool id,
// or "" if unrecognized.
func normalizeToolToken(tok string) string {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case toolClaude, "claude-code", "claudecode":
		return toolClaude
	case toolCodex, "codex-cli", "codexcli":
		return toolCodex
	case toolCursor, "cursor-cli", "cursorcli":
		return toolCursor
	default:
		return ""
	}
}

// parseToolsFlag converts the --tools flag value into a normalized tool list.
// Accepts a single tool ("claude"/"codex"/"cursor"), a comma-separated list
// ("claude,cursor"), "all" (every tool), or "both" (claude+codex, kept for
// back-compat with the codex rollout). Returns nil for an empty/unrecognized
// value so the caller can fall back to the interactive menu.
func parseToolsFlag(v string) []string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return nil
	}
	switch s {
	case "all":
		return append([]string(nil), allTools...)
	case "both":
		// Historical alias from the codex PR: claude + codex.
		return []string{toolClaude, toolCodex}
	}

	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(s, ",") {
		t := normalizeToolToken(part)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	// Re-order into the canonical sequence for stable downstream behavior.
	if len(out) == 0 {
		return nil
	}
	var ordered []string
	for _, t := range allTools {
		if seen[t] {
			ordered = append(ordered, t)
		}
	}
	return ordered
}

// hasTool reports whether tool is present in the selected list.
func hasTool(tools []string, tool string) bool {
	for _, t := range tools {
		if t == tool {
			return true
		}
	}
	return false
}

// resolveAllowedTools normalizes the recruiter-chosen allowed set into canonical
// tool ids in the canonical order, dropping unknown/duplicate entries. An
// empty/nil input (older server that doesn't send allowedTools) falls back to
// all three tools, preserving the pre-allowedTools behavior.
func resolveAllowedTools(allowed []string) []string {
	if len(allowed) == 0 {
		return append([]string(nil), allTools...)
	}
	seen := map[string]bool{}
	for _, a := range allowed {
		if t := normalizeToolToken(a); t != "" {
			seen[t] = true
		}
	}
	var ordered []string
	for _, t := range allTools {
		if seen[t] {
			ordered = append(ordered, t)
		}
	}
	if len(ordered) == 0 {
		// allowedTools contained only unknown tokens — treat as unconstrained
		// rather than locking the candidate out entirely.
		return append([]string(nil), allTools...)
	}
	return ordered
}

// intersectTools returns the elements of tools that are also in allowed,
// preserving the order of tools.
func intersectTools(tools, allowed []string) []string {
	var out []string
	for _, t := range tools {
		if hasTool(allowed, t) {
			out = append(out, t)
		}
	}
	return out
}

// selectTools resolves which AI tools to install hooks for, constrained to the
// recruiter-chosen allowed set.
//
//   - allowed is normalized to the canonical {claude,codex,cursor} subset; an
//     empty/nil allowed falls back to all three (older server, unconstrained).
//   - If toolsFlag is set (non-interactive / CI), it is parsed and INTERSECTED
//     with the allowed set: tools the recruiter didn't allow are dropped with a
//     warning; a fully-disjoint request errors out (returns nil after printing
//     which tools are allowed).
//   - With no flag: a single allowed tool auto-selects (no menu); multiple
//     allowed tools show an interactive menu scoped to the allowed set, and a
//     non-interactive/empty response defaults to ALL allowed tools.
//
// Returns nil when the candidate's --tools request shares nothing with the
// allowed set — the caller treats that as a fatal selection error.
func selectTools(toolsFlag string, allowed []string) []string {
	allowedSet := resolveAllowedTools(allowed)

	warn := lipgloss.NewStyle().Foreground(cWarnText).Bold(true)
	dim := lipgloss.NewStyle().Foreground(cDim)

	if parsed := parseToolsFlag(toolsFlag); parsed != nil {
		// Warn about any requested tool the recruiter didn't allow, then drop it.
		for _, t := range parsed {
			if !hasTool(allowedSet, t) {
				fmt.Printf("  %s %s\n",
					warn.Render("!"),
					warn.Render(toolDisplayName(t)+" is not allowed for this assessment — skipping it."))
			}
		}
		resolved := intersectTools(parsed, allowedSet)
		if len(resolved) == 0 {
			fmt.Fprintf(os.Stderr, "error: none of the requested tools are allowed for this assessment.\n")
			fmt.Fprintf(os.Stderr, "  Allowed: %s\n", toolsLabel(allowedSet))
			return nil
		}
		return resolved
	}

	// No --tools flag. If the recruiter pinned exactly one tool, force it.
	if len(allowedSet) == 1 {
		fmt.Println()
		fmt.Printf("  %s\n", dim.Render("This assessment uses "+toolDisplayName(allowedSet[0])+"."))
		return append([]string(nil), allowedSet...)
	}

	heading := lipgloss.NewStyle().Bold(true).Foreground(cStrong)
	num := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")).Width(3).Align(lipgloss.Right)
	body := lipgloss.NewStyle().Foreground(cBody)
	prompt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))

	fmt.Println()
	fmt.Println(heading.Render("Which AI coding tool will you use?"))
	// The menu lists ONLY the allowed tools, numbered 1..N in canonical order,
	// with a trailing "All" entry (the default on Enter).
	for i, t := range allowedSet {
		label := body.Render(toolBaseName(t))
		if suffix := toolBetaSuffix(t); suffix != "" {
			label += " " + dim.Render(suffix)
		}
		fmt.Printf("  %s  %s\n", num.Render(fmt.Sprintf("%d", i+1)), label)
	}
	fmt.Printf("  %s  %s %s\n", num.Render(fmt.Sprintf("%d", len(allowedSet)+1)), body.Render("All"), dim.Render("(default)"))
	fmt.Printf("  %s ", prompt.Render("❯"))

	choice := ""
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		choice = strings.TrimSpace(scanner.Text())
	}

	// Map a numeric choice within range to that single allowed tool; anything
	// else (the "All" entry, empty Enter, out-of-range, or non-interactive with
	// no input) defaults to ALL allowed tools.
	for i, t := range allowedSet {
		if choice == fmt.Sprintf("%d", i+1) {
			return []string{t}
		}
	}
	return append([]string(nil), allowedSet...)
}

// toolBaseName renders a tool's name without the "(beta)" suffix, for menu rows
// where the suffix is rendered separately/dimmed.
func toolBaseName(tool string) string {
	switch tool {
	case toolClaude:
		return "Claude Code"
	case toolCodex:
		return "Codex CLI"
	case toolCursor:
		return "Cursor"
	default:
		return tool
	}
}

// toolBetaSuffix returns "(beta)" for the newer integrations, "" otherwise.
func toolBetaSuffix(tool string) string {
	switch tool {
	case toolCodex, toolCursor:
		return "(beta)"
	default:
		return ""
	}
}

// toolDisplayName renders a single tool's human-readable name, with a "(beta)"
// suffix for the newer integrations.
func toolDisplayName(tool string) string {
	switch tool {
	case toolClaude:
		return "Claude Code"
	case toolCodex:
		return "Codex CLI (beta)"
	case toolCursor:
		return "Cursor (beta)"
	default:
		return tool
	}
}

// toolsLabel renders a human-readable summary of the selected tools in the
// canonical order, e.g. "Claude Code + Cursor (beta)".
func toolsLabel(tools []string) string {
	var parts []string
	for _, t := range allTools {
		if hasTool(tools, t) {
			parts = append(parts, toolDisplayName(t))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " + ")
}
