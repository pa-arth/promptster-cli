package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		interactiveMenu()
		return
	}

	switch os.Args[1] {
	case "redeem":
		cmdRedeem(os.Args[2:])
	case "start":
		cmdStart(os.Args[2:])
	case "done":
		cmdDone(os.Args[2:])
	case "abort", "cleanup":
		// "cleanup" is the legacy/internal name (shell-hook self-eviction calls
		// it via cmd_env.go); "abort" is the surfaced user-facing name.
		cmdCleanup(os.Args[2:])
	case "reset":
		cmdReset(os.Args[2:])
	case "env":
		cmdEnv(os.Args[2:])
	case "auth-token":
		cmdAuthToken(os.Args[2:])
	case "codex":
		// Launches codex wired to the proxy for that one process: provider as
		// `-c` overrides, credential in its environment. Everything after the
		// subcommand is forwarded to codex verbatim, so no flag parsing here.
		cmdCodex(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "doctor":
		cmdDoctor()
	case "hook":
		cmdHook(os.Args[2:])
	case "brief", "task":
		cmdBrief(os.Args[2:])
	case "explain":
		cmdExplain(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "diff-watch":
		if err := runGitWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "git watcher error: %v\n", err)
			os.Exit(1)
		}
	case "codex-watch":
		// Background daemon: tails codex rollout JSONL and ingests events.
		if err := runCodexWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "codex watcher error: %v\n", err)
			os.Exit(1)
		}
	case "claude-watch":
		// Background daemon: tails Claude Code transcript JSONL and ingests
		// events (transcript-capture / BYO-subscription mode).
		if err := runClaudeWatcher(); err != nil {
			fmt.Fprintf(os.Stderr, "claude watcher error: %v\n", err)
			os.Exit(1)
		}
	case "decide":
		fmt.Fprintln(os.Stderr, "'promptster decide' has been replaced by 'promptster explain'.")
		fmt.Fprintln(os.Stderr, "Run 'promptster explain' to document your decision rationale.")
		os.Exit(1)
	case "version", "--version", "-version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func interactiveMenu() {
	title := lipgloss.NewStyle().Bold(true).Foreground(cStrong)
	dim := lipgloss.NewStyle().Foreground(cMuted)
	num := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#22c55e")).Width(4).Align(lipgloss.Right)
	label := lipgloss.NewStyle().Foreground(cBody)
	desc := lipgloss.NewStyle().Foreground(cDim)
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)

	// Check if there's an active session
	_, sessionErr := loadSession()
	hasSession := sessionErr == nil

	fmt.Println()
	fmt.Printf("  %s  %s\n", title.Render("Promptster"), dim.Render("v"+version))
	fmt.Println()

	type menuItem struct {
		key         string
		label       string
		description string
		needSession bool
	}

	items := []menuItem{
		{"1", "Start assessment", "Redeem a key and begin", false},
		{"2", "View task brief", "Open the brief + live countdown in its own window", true},
		{"3", "Explain a decision", "Document your rationale for recent work", true},
		{"4", "Check status", "Show session info and event count", true},
		{"5", "Submit assessment", "Push code and complete the session", true},
		{"6", "Run doctor", "Diagnose setup and configuration issues", false},
		{"7", "Abort assessment", "Discard the session without submitting", true},
		{"8", "Help", "Show all available commands", false},
	}

	for _, item := range items {
		if item.needSession && !hasSession {
			fmt.Printf("  %s  %s  %s\n", num.Render(item.key), desc.Render(item.label), desc.Render("(no active session)"))
		} else {
			fmt.Printf("  %s  %s  %s\n", num.Render(item.key), label.Render(item.label), desc.Render(item.description))
		}
	}

	fmt.Println()
	fmt.Printf("  %s\n", dim.Render("Docs: https://docs.promptster.ai"))
	fmt.Println()
	fmt.Printf("  %s ", prompt.Render("❯"))

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return
	}
	choice := strings.TrimSpace(scanner.Text())

	fmt.Println()

	switch choice {
	case "1":
		fmt.Printf("  %s ", prompt.Render("Enter your assessment key (PST-XXXX-XXXX):"))
		if scanner.Scan() {
			key := strings.TrimSpace(scanner.Text())
			if key != "" {
				cmdStart([]string{key})
			}
		}
	case "2":
		cmdBrief(nil)
	case "3":
		cmdExplain([]string{})
	case "4":
		cmdStatus(nil)
	case "5":
		cmdDone(nil)
	case "6":
		cmdDoctor()
	case "7":
		cmdCleanup(nil)
	case "8":
		printUsage()
	default:
		if strings.HasPrefix(strings.ToUpper(choice), "PST-") {
			// User pasted a key directly
			cmdStart([]string{choice})
		} else if choice != "" {
			fmt.Fprintf(os.Stderr, "  Unknown option: %s\n", choice)
		}
	}
}

func printUsage() {
	fmt.Print(`Usage: promptster <command> [flags]

Commands:
  start [key]                  Redeem key, configure hooks, and start assessment
                               (prompts for the key when one isn't given)
  redeem <key>                 Validate key, accept terms, save session (standalone)
  done                         Submit your assessment when finished
  abort [--reason X]           Discard the current session + tear down hooks (no submit)
  reset [--purge]              Wipe local Promptster config to recover a broken setup
  brief [--here|--json]        Open the live task brief in a new terminal window
                               (--here shows it in this terminal instead)
  status [--json]              Show current session info and live event count
  doctor                       Check setup and diagnose configuration issues
  explain [--last 20m]         Document your decision rationale for recent work
  codex [args...]              Launch Codex wired to this session's proxy, for
                               that process only (args forwarded to codex)
  verify [sessionId|PST-...]   Verify the signed event log for a session
  version                      Print version
  help                         Show this help message

Aliases:
  task                         Same as brief
  cleanup                      Same as abort (legacy name)

Flags for start:
  --accept-tos      Accept terms of service non-interactively (scripted use)
  --workspace PATH  Use PATH as workspace directory (skip interactive prompt)
  --adopt           Use the checkout that is already here instead of cloning one
                    (defaults ON inside a GitHub Codespace; --adopt=false forces a clone)
  --seeded          Start from a session the provisioning worker already wrote to
                    disk: no redeem, no consent prompt, no key argument
  --restart         Offer to restart running editors so they reload hooks
  --verbose         Print each sub-step (paths, API URL, hook events) for debugging

Flags for done:
  --auto            Auto-submit mode (skip pending-decision check)

Flags for abort:
  --delete-codespace  Delete the GitHub Codespace too (hosted lane; off by default —
                      abort uploads nothing, so the box holds the only copy)

Flags for redeem:
  --accept-tos      Accept terms of service non-interactively (scripted use)

Flags for reset:
  --purge           Also remove the installed binary (full uninstall)

Troubleshooting:
  promptster doctor                    Verify git, Claude Code, and PATH are configured
  eval "$(promptster env --clear)"     Unset leaked ANTHROPIC_* proxy vars from this shell
  PROMPTSTER_DEBUG=1                   Enable verbose hook logging

Examples:
  promptster start PST-ABCD-1234
  promptster doctor
  promptster explain
  promptster explain --last 30m
  promptster done

Documentation:
  https://docs.promptster.ai
`)
}
