package main

import (
	"fmt"
	"os/exec"
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
	checks := []toolCheck{
		{
			name:        "git",
			label:       "Git",
			required:    true,
			installHint: gitInstallHint(),
		},
		{
			name:        "claude",
			label:       "Claude Code",
			required:    true,
			installHint: claudeInstallHint(),
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

// hasRequiredTools returns true if all required tools are available.
func hasRequiredTools() bool {
	required := []string{"git", "claude"}
	for _, name := range required {
		if _, err := exec.LookPath(name); err != nil {
			return false
		}
	}
	return true
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
