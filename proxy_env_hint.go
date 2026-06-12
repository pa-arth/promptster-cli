package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// shellHasLeakedProxyEnv reports whether ANTHROPIC_* proxy routing is still
// exported in this process. As of 1.2.0 the proxy is wired via apiKeyHelper and
// nothing is exported into the shell, so this only ever fires for a candidate
// who upgraded from a pre-1.2.0 CLI mid-session and still has the old vars set
// (they out-rank the helper). removeShellHook() stops future shells from
// loading creds but can't unset vars already in the current environment.
func shellHasLeakedProxyEnv() bool {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) != "" {
		return true
	}
	return false
}

// printShellProxyEnvClearHint reminds the user to drop proxy env in this shell.
func printShellProxyEnvClearHint() {
	if !shellHasLeakedProxyEnv() {
		return
	}
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	fmt.Println()
	fmt.Println(dim.Render("This terminal still has assessment proxy env (can hijack personal Claude Code)."))
	fmt.Println(dim.Render("Clear it before running claude or audit scripts:"))
	fmt.Println("  eval \"$(promptster env --clear)\"")
}
