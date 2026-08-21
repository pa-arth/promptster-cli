package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// AI tool identifiers Promptster can instrument.
//
// Cursor was a third member until openspec changes/employer-supplied-model-key.
// It routes Agent/Edit model traffic through its own backend, so a key the
// hiring team supplies can be neither used nor metered on it — which made a
// Cursor assessment candidate-pays by construction, the one arrangement this
// product no longer offers. Cursor remains a perfectly good editor to work in;
// it is not an instrumented agent.
const (
	toolClaude = "claude"
	toolCodex  = "codex"
)

// retiredToolCursor is kept as a RECOGNISED token, not deleted.
//
// The distinction it preserves is the whole point: "cursor" is a tool we know
// about and no longer offer, which is a different fact from a typo or a token
// from a newer server. Collapsing the two would send a cursor-only assessment
// down the unknown-token path, and that path deliberately falls back to the
// full tool set — silently converting "this assessment runs on Cursor" into
// "this assessment runs on Claude Code and Codex", on the candidate's machine,
// with nobody told.
const retiredToolCursor = "cursor"

// allTools is the canonical ordering used when "all" is selected and when
// rendering labels, so output is deterministic regardless of input order.
var allTools = []string{toolClaude, toolCodex}

// normalizeToolToken maps a single user-supplied token to a canonical tool id,
// or "" if unrecognized.
func normalizeToolToken(tok string) string {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case toolClaude, "claude-code", "claudecode":
		return toolClaude
	case toolCodex, "codex-cli", "codexcli":
		return toolCodex
	default:
		return ""
	}
}

// isRetiredToolToken reports whether tok names a tool Promptster used to
// instrument and no longer does. Separate from normalizeToolToken so callers can
// tell "retired" from "unrecognised" and say something true about each.
func isRetiredToolToken(tok string) bool {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case retiredToolCursor, "cursor-cli", "cursorcli":
		return true
	default:
		return false
	}
}

// parseToolsFlag converts the --tools flag value into a normalized tool list.
// Accepts a single tool ("claude"/"codex"), a comma-separated list
// ("claude,codex"), "all" (every tool), or "both" (claude+codex, kept for
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
// tool ids in the canonical order, dropping unknown/duplicate entries.
//
// It returns the resolved set and whether the assessment is RETIRED-ONLY: the
// recruiter named tools, every one of them is a tool we no longer instrument,
// and the resolved set is therefore empty. That is not the same as "unknown
// tokens" and must not be treated as it.
//
// THE THREE EMPTY CASES ARE THREE DIFFERENT FACTS.
//
//   - `allowed == nil` — the server never sent the field (pre-allowedTools
//     server). Unconstrained; fall back to every tool, as before.
//   - `allowed` names only tokens we do not recognise — a newer server naming a
//     tool this CLI predates. Fall back to every tool rather than locking the
//     candidate out over a version skew.
//   - `allowed` names only RETIRED tools — a cursor-only assessment. Return
//     EMPTY. Falling back here would take an assessment the recruiter configured
//     for Cursor and silently run it on Claude Code and Codex instead, on the
//     candidate's machine, billed to the org, with nobody told. The candidate
//     gets an explicit stop and the recruiter gets told to fix the assessment.
//
// A non-nil, genuinely empty `allowed` is the server having filtered a retired
// tool out before sending, which is the same fact as the third case.
func resolveAllowedTools(allowed []string) (tools []string, retiredOnly bool) {
	if allowed == nil {
		return append([]string(nil), allTools...), false
	}
	seen := map[string]bool{}
	sawRetired := false
	sawUnknown := false
	for _, a := range allowed {
		if t := normalizeToolToken(a); t != "" {
			seen[t] = true
			continue
		}
		if isRetiredToolToken(a) {
			sawRetired = true
			continue
		}
		sawUnknown = true
	}
	var ordered []string
	for _, t := range allTools {
		if seen[t] {
			ordered = append(ordered, t)
		}
	}
	if len(ordered) > 0 {
		return ordered, false
	}
	// Nothing usable resolved. A server that sent an explicitly empty list, or a
	// list of nothing but retired tools, is telling us this assessment has no
	// instrumented agent — say so. Only an unrecognised-token skew falls back.
	if sawRetired || len(allowed) == 0 {
		return nil, true
	}
	if sawUnknown {
		return append([]string(nil), allTools...), false
	}
	return nil, true
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
//   - allowed is normalized to the canonical {claude,codex} subset; a nil
//     allowed falls back to both (older server, unconstrained).
//   - An assessment whose allowed set is nothing but RETIRED tools stops here
//     with an explanation. See resolveAllowedTools for why that case must not
//     fall back.
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
	allowedSet, retiredOnly := resolveAllowedTools(allowed)

	warn := lipgloss.NewStyle().Foreground(cWarnText).Bold(true)
	dim := lipgloss.NewStyle().Foreground(cDim)

	if retiredOnly {
		// Say what is true and who can fix it. The candidate cannot: allowed_tools
		// is the recruiter's setting, and there is no flag that overrides it.
		fmt.Fprintf(os.Stderr, "error: this assessment has no instrumented AI tool configured.\n")
		fmt.Fprintf(os.Stderr, "  It was set up for Cursor, which Promptster no longer instruments — Cursor\n")
		fmt.Fprintf(os.Stderr, "  routes model traffic through its own backend, so the hiring team's key\n")
		fmt.Fprintf(os.Stderr, "  cannot be used or metered on it.\n\n")
		fmt.Fprintf(os.Stderr, "  You have done nothing wrong and there is nothing to retry. Ask the person\n")
		fmt.Fprintf(os.Stderr, "  who sent you this assessment to switch it to Claude Code or Codex.\n")
		return nil
	}

	// A candidate whose instructions still say `--tools cursor` gets told, rather
	// than silently dropped into the menu as if they had asked for nothing.
	for _, part := range strings.Split(toolsFlag, ",") {
		if isRetiredToolToken(part) {
			fmt.Printf("  %s %s\n",
				warn.Render("!"),
				warn.Render("Cursor is no longer instrumented by Promptster — ignoring it. You can still work in Cursor as your editor."))
			break
		}
	}

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
	default:
		return tool
	}
}

// toolBetaSuffix returns "(beta)" for the newer integrations, "" otherwise.
func toolBetaSuffix(tool string) string {
	switch tool {
	case toolCodex:
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
