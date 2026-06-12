package main

import (
	"fmt"
	"os"
	"path/filepath"

	flag "github.com/spf13/pflag"
)

// cmdReset is the recovery command: it wipes Promptster's local configuration
// so a candidate whose setup got into a bad state (stale proxy config, broken
// hooks, leaked env, half-finished start) can get back to a clean slate without
// hand-deleting files.
//
// It is the heavier sibling of `abort` (cmdCleanup):
//   - `abort` discards the *current session* + its hooks; it depends on a
//     readable session.json to find the workspace.
//   - `reset` is the "recover from broken setup" path — it can't assume the
//     session is readable, so it resolves the workspace from the active-workspace
//     pointer as a fallback, then sweeps the global ~/.promptster config too.
//
// What it removes (default):
//   - everything `abort` removes: shell hook + RC source lines, decision/git
//     watchers, <ws>/.promptster state, <ws>/.claude/settings.local.json
//   - every entry under ~/.promptster EXCEPT bin/ (the installed binary), so the
//     next `start` rebuilds config without a reinstall
//
// With --purge it additionally removes ~/.promptster/bin — a full uninstall that
// requires reinstalling the CLI before next use.
//
// It deliberately does NOT submit code or touch server-side key status (the
// server has its own TTL + cron sweep).
func cmdReset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "Also remove the installed binary (~/.promptster/bin) — full uninstall")
	verbose := fs.Bool("verbose", false, "Print each step")
	fs.Parse(args) //nolint:errcheck

	// Resolve the workspace even when session.json is missing or corrupt — reset
	// is the recovery path, so it can't depend on loadSession() succeeding.
	workspace := ""
	if s, err := loadSession(); err == nil {
		workspace = s.TaskRoot
	}
	if workspace == "" {
		workspace = readActiveWorkspace()
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "promptster reset: workspace=%q purge=%v\n", workspace, *purge)
	}

	// Same teardown as `abort`: shell hook + RC lines, watchers, per-session
	// state, workspace .claude config, and the active-workspace pointer.
	removeShellHook()
	stopDecisionWatchers()
	stopGitWatcher()
	stopCodexWatcher()
	stopClaudeWatcher()
	revertCodexProxy() // strip our block from ~/.codex/config.toml before state is wiped
	cleanupPromptsterState(workspace)

	dir := globalPromptsterDir()
	failed := 0
	if *purge {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "promptster reset: warning: could not remove %s: %v\n", dir, err)
			failed++
		} else if *verbose {
			fmt.Fprintf(os.Stderr, "promptster reset: removed %s (incl. binary)\n", dir)
		}
	} else {
		failed = removeGlobalConfigKeepBin(dir, *verbose)
	}

	if failed > 0 {
		fmt.Println("Promptster configuration partially reset — some files could not be removed (see warnings above); you may need to delete them manually.")
		return
	}
	fmt.Println("Promptster configuration reset.")
	if *purge {
		fmt.Println("The CLI binary was removed — reinstall Promptster before using it again.")
	} else {
		fmt.Println("Run `promptster start PST-XXXX-XXXX` to begin a fresh assessment.")
	}
	printShellProxyEnvClearHint()
}

// removeGlobalConfigKeepBin removes every entry under ~/.promptster except the
// bin/ directory, so config is wiped but the installed binary survives and the
// next `start` doesn't have to reinstall. Returns the number of entries that
// could not be removed so the caller can avoid reporting a false success — this
// is the recovery command, so a silent failure would strand the user.
func removeGlobalConfigKeepBin(dir string, verbose bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // nothing to clean (dir absent) — already in the desired state
	}
	failed := 0
	for _, e := range entries {
		if e.Name() == "bin" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintf(os.Stderr, "promptster reset: warning: could not remove %s: %v\n", path, err)
			failed++
		} else if verbose {
			fmt.Fprintf(os.Stderr, "promptster reset: removed %s\n", path)
		}
	}
	return failed
}
