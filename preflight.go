package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type toolCheck struct {
	name     string // binary name
	label    string // display name
	required bool   // hard fail vs warning
	installHint string
}

func preflightChecks() {
	// Only git is universally required here. The AI coding tool binary
	// (claude/codex/cursor) is checked later, after tool selection, against the
	// tool(s) the candidate actually picks — see ensureToolBinaries. Requiring
	// `claude` here would lock out Codex- or Cursor-only assessments.
	checks := []toolCheck{
		{
			name:        "git",
			label:       "Git",
			required:    true,
			installHint: gitInstallHint(),
		},
		{
			name:        "node",
			label:       "Node.js",
			required:    false,
			installHint: nodeInstallHint(),
		},
	}

	checkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	warnStyle := lipgloss.NewStyle().Foreground(cGold).Bold(true)
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(cMuted)

	var missing []toolCheck

	for _, c := range checks {
		path, err := exec.LookPath(c.name)
		if err != nil {
			missing = append(missing, c)
			if c.required {
				fmt.Printf("  %s %s not found\n", errorStyle.Render("✗"), c.label)
			} else {
				fmt.Printf("  %s %s not found %s\n", warnStyle.Render("!"), c.label, dimStyle.Render("(optional)"))
			}
		} else {
			ver := getToolVersion(c.name)
			detail := dimStyle.Render(path)
			if ver != "" {
				detail = dimStyle.Render(ver)
			}
			fmt.Printf("  %s %s %s\n", checkStyle.Render("✓"), c.label, detail)
		}
	}

	// Hard fail if required tools are missing
	hasRequiredMissing := false
	for _, c := range missing {
		if c.required {
			hasRequiredMissing = true
			fmt.Printf("\n  %s is required. Install it:\n", c.label)
			fmt.Printf("  %s\n", dimStyle.Render(c.installHint))
		}
	}
	if hasRequiredMissing {
		fmt.Println()
		errorStyle2 := lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
		fmt.Printf("  %s\n\n", errorStyle2.Render("Install the missing tools above and try again."))
		// Don't os.Exit here — let cmdStart handle it
	}

	// Soft warnings for optional tools
	for _, c := range missing {
		if !c.required {
			fmt.Printf("\n  %s %s is recommended. Install:\n", warnStyle.Render("!"), c.label)
			fmt.Printf("  %s\n", dimStyle.Render(c.installHint))
		}
	}

	if len(missing) > 0 {
		fmt.Println()
	}
}

func getToolVersion(name string) string {
	var args []string
	switch name {
	case "git":
		args = []string{"--version"}
	case "node":
		args = []string{"--version"}
	case "claude":
		args = []string{"--version"}
	default:
		return ""
	}

	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	ver := strings.TrimSpace(string(out))
	// Clean up common prefixes
	ver = strings.TrimPrefix(ver, "git version ")
	ver = strings.TrimPrefix(ver, "v")
	return ver
}

func gitInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "xcode-select --install  (or: brew install git)"
	case "linux":
		return "sudo apt install git  (or: sudo yum install git)"
	case "windows":
		return "https://git-scm.com/download/win"
	default:
		return "https://git-scm.com"
	}
}

// hasRequiredTools returns true if the universally-required tools are available.
// The AI coding tool binary is gated separately by ensureToolBinaries, after the
// candidate has chosen which tool(s) to use.
func hasRequiredTools() bool {
	required := []string{"git"}
	for _, name := range required {
		if _, err := exec.LookPath(name); err != nil {
			return false
		}
	}
	return true
}

// toolBinaryName maps a canonical tool id to the executable Promptster expects on
// PATH to launch it.
func toolBinaryName(tool string) string {
	switch tool {
	case toolClaude:
		return "claude"
	case toolCodex:
		return "codex"
	case toolCursor:
		return "cursor"
	default:
		return tool
	}
}

// toolInstallHint returns an install/PATH hint for a tool whose binary is missing.
func toolInstallHint(tool string) string {
	switch tool {
	case toolClaude:
		return claudeInstallHint()
	case toolCodex:
		return "npm install -g @openai/codex\n  Or visit: https://github.com/openai/codex"
	case toolCursor:
		return "Install Cursor from https://cursor.com, then run\n  \"Shell Command: Install 'cursor' command in PATH\" from the command palette"
	default:
		return ""
	}
}

// cursorInstalled reports whether Cursor appears to be present. Cursor's hooks
// fire from the IDE itself, so the `cursor` PATH shim (an optional convenience
// the user installs from the command palette) is not required for capture — we
// also accept the macOS app bundle as evidence the editor is there.
func cursorInstalled() bool {
	if _, err := exec.LookPath("cursor"); err == nil {
		return true
	}
	if runtime.GOOS == "darwin" {
		candidates := []string{"/Applications/Cursor.app"}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, "Applications", "Cursor.app"))
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}
	return false
}

// toolInstalled reports whether a selected tool can actually be used. Claude and
// Codex are headless CLIs Promptster drives directly, so they need their binary
// on PATH. Cursor is detected via cursorInstalled (shim or app bundle).
func toolInstalled(tool string) bool {
	if tool == toolCursor {
		return cursorInstalled()
	}
	_, err := exec.LookPath(toolBinaryName(tool))
	return err == nil
}

// resolveUsableTools narrows the candidate's selected tools to the ones actually
// installed, warning about any that are missing, and returns the usable subset.
// A missing tool is dropped (not fatal) so that, e.g., picking Claude+Codex with
// only Codex installed proceeds with Codex. If NOTHING is usable the candidate
// has no working AI tool for this assessment, which IS fatal — we print install
// hints for every selected tool and exit.
func resolveUsableTools(tools []string) []string {
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true)
	warnStyle := lipgloss.NewStyle().Foreground(cGold).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(cMuted)

	var usable []string
	for _, t := range tools {
		if toolInstalled(t) {
			usable = append(usable, t)
			// Cursor app present but the launch shim isn't: hooks still fire, but
			// `cursor .` won't — tell the candidate to open the app manually.
			if t == toolCursor {
				if _, err := exec.LookPath("cursor"); err != nil {
					fmt.Printf("\n  %s %s\n", warnStyle.Render("!"),
						dimStyle.Render("Cursor's `cursor` command isn't on PATH — open the Cursor app manually (hooks still capture it)."))
				}
			}
			continue
		}
		fmt.Printf("\n  %s %s not found %s\n", warnStyle.Render("!"), toolBaseName(t), dimStyle.Render("(skipping — install it to use it)"))
		fmt.Printf("  %s\n", dimStyle.Render(toolInstallHint(t)))
	}

	if len(usable) == 0 {
		fmt.Println()
		fmt.Printf("  %s %s\n", errorStyle.Render("✗"),
			errorStyle.Render("None of this assessment's AI tools are installed."))
		fmt.Printf("  %s\n", dimStyle.Render("Install at least one to continue:"))
		for _, t := range tools {
			fmt.Printf("\n  %s\n  %s\n", toolBaseName(t), dimStyle.Render(toolInstallHint(t)))
		}
		fmt.Println()
		os.Exit(1)
	}
	return usable
}

// projectToolchain is the set of package managers, interpreters, and build tools
// whose absence is worth flagging when an assessment's run/test command needs
// them. Kept curated so we never warn about a candidate's own scripts.
var projectToolchain = map[string]bool{
	"node": true, "npm": true, "npx": true, "pnpm": true, "yarn": true,
	"bun": true, "deno": true, "python": true, "python3": true, "pip": true,
	"pip3": true, "uv": true, "poetry": true, "go": true, "cargo": true,
	"rustc": true, "ruby": true, "bundle": true, "rails": true, "java": true,
	"mvn": true, "gradle": true, "make": true, "docker": true, "dotnet": true,
	"php": true, "composer": true,
}

// leadingCommandBinary extracts the executable a command invokes, or "" when it
// is a path-qualified script (e.g. ./gradlew) or env-prefixed form we can't
// reason about cleanly.
func leadingCommandBinary(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	// Skip leading env-var assignments (FOO=bar cmd ...).
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.ContainsAny(fields[0], "/.") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return ""
	}
	first := fields[0]
	if strings.ContainsAny(first, "/\\") {
		return "" // path-qualified script, not a toolchain binary
	}
	return first
}

// warnMissingProjectTools advises (never fails) when the assessment's documented
// run/test commands depend on a toolchain binary that isn't installed. Honors
// the brief's "environment fights measure nothing" principle: this is an early
// heads-up, not a gate — the candidate may install the tool as part of the task.
func warnMissingProjectTools(b Brief) {
	warnStyle := lipgloss.NewStyle().Foreground(cGold).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(cMuted)

	seen := map[string]bool{}
	var missing []string
	for _, cmd := range []string{b.Codebase.RunCommand, b.Codebase.TestCommand} {
		bin := leadingCommandBinary(cmd)
		if bin == "" || seen[bin] || !projectToolchain[bin] {
			continue
		}
		seen[bin] = true
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		return
	}
	verb := "is"
	if len(missing) > 1 {
		verb = "are"
	}
	fmt.Println()
	fmt.Printf("  %s %s\n", warnStyle.Render("!"),
		dimStyle.Render(fmt.Sprintf("This assessment's run/test commands use %s, which %s not on your PATH.",
			strings.Join(missing, ", "), verb)))
	fmt.Printf("  %s\n", dimStyle.Render("You may need to install it to run or test the project."))
}

func claudeInstallHint() string {
	return "npm install -g @anthropic-ai/claude-code\n  Or visit: https://docs.anthropic.com/en/docs/claude-code/overview"
}

func nodeInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install node  (or: https://nodejs.org)"
	case "linux":
		return "sudo apt install nodejs  (or: https://nodejs.org)"
	default:
		return "https://nodejs.org"
	}
}
