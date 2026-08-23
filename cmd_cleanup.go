package main

import (
	"fmt"
	"os"

	flag "github.com/spf13/pflag"
)

// cmdCleanup tears down all on-disk session state without submitting to the
// API. It exists for three callers:
//
//  1. The shell hook's self-eviction path when ExpiresAt has passed — fired
//     async from cmd_env, no user interaction.
//  2. Candidates (or internal dogfooders) who want to abort an assessment
//     without finalizing a junk submission. `promptster done` always submits;
//     this is the missing escape hatch.
//  3. Future: server-driven invalidation could exec this on the candidate's
//     machine via a hook event.
//
// What it removes:
//   - ~/.promptster/shell-hook.sh and the RC source line(s) it installed
//   - ~/.promptster/active-workspace pointer
//   - <workspace>/.promptster/* ephemeral state (session.json, buffers, etc.)
//   - <workspace>/.claude/settings.local.json (Claude project hook config)
//
// What it deliberately does NOT do:
//   - Submit code (no apiComplete, no bundle upload)
//   - Flip the candidate key status server-side (server has its own TTL +
//     cron sweep; we don't need a candidate-machine path)
//   - Touch the binary at ~/.promptster/bin/promptster (kept so the next
//     `pst start` doesn't have to reinstall)
func cmdCleanup(args []string) {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	reason := fs.String("reason", "manual", "Why cleanup is running (logged, surfaces in --verbose)")
	verbose := fs.Bool("verbose", false, "Print each step")
	// abort NEVER deletes the codespace on its own (openspec §2.5). Abort is the
	// path where nothing was uploaded, so the box holds the only copy of whatever
	// the candidate did — and one of abort's three callers is the shell hook's
	// unattended TTL self-eviction, which would otherwise delete a running
	// candidate's machine out from under them the moment their key aged out.
	deleteCodespace := fs.Bool("delete-codespace", false, "Also delete the GitHub Codespace this is running in (hosted lane; off by default — abort does not upload)")
	fs.Parse(args) //nolint:errcheck

	// Best-effort session load — cleanup must work even if session.json is
	// corrupt or missing (otherwise the abandoned-session case can't recover).
	var taskRoot string
	if s, err := loadSession(); err == nil {
		taskRoot = s.TaskRoot
	}

	if *verbose {
		fmt.Fprintf(os.Stderr, "promptster cleanup (reason=%s)\n", *reason)
	}

	removeShellHook()
	stopDecisionWatchers()
	stopGitWatcher()
	stopCodexWatcher()
	stopClaudeWatcher()
	revertCodexProxy() // strip our block from ~/.codex/config.toml before state is wiped
	cleanupPromptsterState(taskRoot)

	if *verbose {
		fmt.Fprintln(os.Stderr, "promptster cleanup: done")
	}
	if !inCodespace() {
		printShellProxyEnvClearHint()
	}

	if *deleteCodespace && inCodespace() {
		printCodespaceWindDown(deleteHostingCodespace())
	}
}
