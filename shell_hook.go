package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const shellHookMarker = "# >>> promptster shell hook >>>"
const shellHookMarkerEnd = "# <<< promptster shell hook <<<"

// shellHookPath returns ~/.promptster/shell-hook.sh (or .ps1 on Windows).
func shellHookPath() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return filepath.Join(home, ".promptster", "shell-hook.ps1")
	}
	return filepath.Join(home, ".promptster", "shell-hook.sh")
}

// shellHookScript generates a POSIX shell script that captures every command
// typed in the terminal inside the assessment workspace. Works with both bash
// (PROMPT_COMMAND + DEBUG trap) and zsh (preexec/precmd).
//
// As of 1.2.0 the shell hook NO LONGER touches Anthropic proxy env. The proxy
// is wired through Claude Code's apiKeyHelper in <ws>/.claude/settings.local.json
// (see proxy_config.go), which feeds the parent claude auth resolution directly
// — so there is no token to export into the shell, no PWD-gated proxy block, and
// no per-prompt eviction. That removed the entire class of "PST token leaked
// into an unrelated terminal / audit run" bugs the older hook fought.
//
// What remains:
//
//  1. Command capture — PWD-gated to the workspace tree via
//     _promptster_in_workspace, so only commands run inside the assessment are
//     recorded. Each is sent to `promptster hook shell-cmd`, which normalizes it
//     to a `command` event (same shape as AI-executed Bash tool calls).
//
//  2. TTL self-eviction — a single backgrounded `promptster env` at shell start.
//     It exports nothing now; its only job is to fire `promptster cleanup` when
//     the session's local TTL has passed, so an abandoned session's RC line +
//     shell hook disappear on the next new terminal.
func shellHookScript() string {
	bin := promptsterBin()
	return fmt.Sprintf(`#!/bin/sh
# Promptster shell hook — captures terminal commands inside the workspace.
# Auto-sourced from your shell RC file. Removed by 'promptster done' or
# 'promptster cleanup'. Self-evicts when the session's local TTL passes.
# The Anthropic proxy is configured via Claude Code's apiKeyHelper, NOT here.

# Skip non-interactive shells (scp, ssh-exec, scripts that source bashrc).
# Installing a DEBUG trap or preexec hook in those contexts breaks remote
# commands and leaks job-done messages.
case $- in
  *i*) ;;
  *) return 0 2>/dev/null || : ;;
esac

# Resolve the active workspace pointer (global, written by 'promptster start').
_promptster_ws=""
if [ -f "$HOME/.promptster/active-workspace" ]; then
  _promptster_ws="$(cat "$HOME/.promptster/active-workspace" 2>/dev/null)"
fi

# No active session → nothing to do for this shell.
if [ -z "$_promptster_ws" ] || [ ! -f "$_promptster_ws/.promptster/session.json" ]; then
  return 0 2>/dev/null || :
fi

# Canonicalize to the PHYSICAL path (resolve symlinks). On macOS /tmp is a
# symlink to /private/tmp, so a workspace handed out as /tmp/foo would otherwise
# never match a terminal whose $PWD is /private/tmp/foo, silently dropping every
# human terminal command. Resolving both sides to their physical path fixes it.
_promptster_ws_phys="$(cd "$_promptster_ws" 2>/dev/null && pwd -P)"
[ -n "$_promptster_ws_phys" ] && _promptster_ws="$_promptster_ws_phys"

_promptster_bin="%s"

_promptster_in_workspace() {
  # Fast path: logical $PWD already matches (no symlink in play).
  case "$PWD" in
    "$_promptster_ws"|"$_promptster_ws"/*) return 0 ;;
  esac
  # Fallback: compare the physical cwd so /tmp -> /private/tmp can't defeat us.
  _promptster_pwd_phys="$(pwd -P 2>/dev/null)"
  case "$_promptster_pwd_phys" in
    "$_promptster_ws"|"$_promptster_ws"/*) return 0 ;;
    *) return 1 ;;
  esac
}

# ── Codex proxy token (PWD-gated) ──────────────────────────────────────────
# Codex has NO apiKeyHelper equivalent — its custom model provider reads the
# proxy credential from $PROMPTSTER_PROXY_TOKEN. So, unlike Claude (whose token
# is fed via apiKeyHelper and never touches the shell), codex forces the token
# into the env. We scope it tightly: export ONLY inside the workspace AND only
# while our managed provider block is present in codex's config (so a Claude-
# only session never exports it), sourced from the 0600 session.json via
# 'auth-token'. It is unset the moment you leave the workspace, so it never
# leaks into an unrelated shell.
_promptster_codex_cfg="${CODEX_HOME:-$HOME/.codex}/config.toml"
_promptster_codex_active() {
  [ -f "$_promptster_codex_cfg" ] && grep -q "%s" "$_promptster_codex_cfg" 2>/dev/null
}
_promptster_sync_codex_token() {
  if _promptster_in_workspace && _promptster_codex_active; then
    if [ -z "${PROMPTSTER_PROXY_TOKEN:-}" ]; then
      PROMPTSTER_PROXY_TOKEN="$("$_promptster_bin" auth-token 2>/dev/null)"
      [ -n "$PROMPTSTER_PROXY_TOKEN" ] && export PROMPTSTER_PROXY_TOKEN
    fi
  elif [ -n "${PROMPTSTER_PROXY_TOKEN:-}" ]; then
    unset PROMPTSTER_PROXY_TOKEN
  fi
}
# Sync once now so a 'codex' run in this fresh shell sees the token immediately.
_promptster_sync_codex_token

# TTL self-eviction (backgrounded, no output). 'promptster env' fires a
# background cleanup when the session's local ExpiresAt has passed; otherwise
# it does nothing. Detached so shell start never blocks on the spawn.
( "$_promptster_bin" env >/dev/null 2>&1 & ) 2>/dev/null

if [ -n "$ZSH_VERSION" ]; then
  # ── zsh: preexec receives the full command line as $1 ──
  _promptster_last_cmd=""
  _promptster_cmd_start=""

  promptster_preexec() {
    _promptster_in_workspace || return 0
    _promptster_last_cmd="$1"
    _promptster_cmd_start="$EPOCHSECONDS"
    [ -z "$_promptster_cmd_start" ] && _promptster_cmd_start="$(date +%%s)"
  }

  promptster_precmd() {
    local exit_code=$?
    _promptster_sync_codex_token
    _promptster_in_workspace || return 0
    if [ -n "$_promptster_last_cmd" ]; then
      local now elapsed=0
      now="${EPOCHSECONDS:-$(date +%%s)}"
      [ -n "$_promptster_cmd_start" ] && elapsed=$(( now - _promptster_cmd_start ))
      # Subshell backgrounding works in both bash and zsh; avoids zsh-only
      # '&!' which bash's parser rejects at source time — even inside a
      # branch bash never takes — and used to make .bashrc error on start.
      ( "$_promptster_bin" hook shell-cmd "$_promptster_last_cmd" "$exit_code" "$elapsed" >/dev/null 2>&1 & ) 2>/dev/null
      _promptster_last_cmd=""
      _promptster_cmd_start=""
    fi
  }

  autoload -Uz add-zsh-hook
  add-zsh-hook preexec promptster_preexec
  add-zsh-hook precmd promptster_precmd

elif [ -n "$BASH_VERSION" ]; then
  # ── bash: use PROMPT_COMMAND + history to get the full command line ──
  _promptster_last_cmd=""
  _promptster_cmd_start=""

  _promptster_debug_trap() {
    _promptster_in_workspace || return 0
    # Only capture the first command in each prompt cycle (not subshells,
    # not PROMPT_COMMAND itself). Use history to get the full pipeline.
    if [ -z "$_promptster_last_cmd" ] && [ -z "$COMP_LINE" ]; then
      _promptster_last_cmd="$(HISTTIMEFORMAT= history 1 2>/dev/null | sed 's/^[ ]*[0-9]*[ ]*//')"
      _promptster_cmd_start="$SECONDS"
    fi
  }

  _promptster_prompt_cmd() {
    local exit_code=$?
    _promptster_sync_codex_token
    _promptster_in_workspace || return 0
    if [ -n "$_promptster_last_cmd" ]; then
      local elapsed=0
      [ -n "$_promptster_cmd_start" ] && elapsed=$(( SECONDS - _promptster_cmd_start ))
      ( "$_promptster_bin" hook shell-cmd "$_promptster_last_cmd" "$exit_code" "$elapsed" >/dev/null 2>&1 & ) 2>/dev/null
      _promptster_last_cmd=""
      _promptster_cmd_start=""
    fi
  }

  trap '_promptster_debug_trap' DEBUG
  # Guard against double-injection if this file is sourced more than once.
  case ";$PROMPT_COMMAND;" in
    *";_promptster_prompt_cmd;"*) ;;
    *) PROMPT_COMMAND="_promptster_prompt_cmd${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
  esac
fi
`, bin, codexProxyMarker)
}

// shellRCPathsForInstall returns the user's shell RC files to inject the
// source line into, based on the current $SHELL. Install is targeted so
// we don't pollute RC files for shells the user doesn't use.
func shellRCPathsForInstall() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	shell := os.Getenv("SHELL")
	var paths []string
	if strings.Contains(shell, "zsh") {
		paths = append(paths, filepath.Join(home, ".zshrc"))
	} else if strings.Contains(shell, "bash") {
		// macOS uses .bash_profile for login shells, Linux uses .bashrc
		if runtime.GOOS == "darwin" {
			paths = append(paths, filepath.Join(home, ".bash_profile"))
		}
		paths = append(paths, filepath.Join(home, ".bashrc"))
	} else {
		// Unknown shell — default to zsh (macOS default) only; avoid
		// polluting .bashrc when the user isn't a bash user, which used
		// to orphan source lines across shell swaps.
		if runtime.GOOS == "darwin" {
			paths = append(paths, filepath.Join(home, ".zshrc"))
		} else {
			paths = append(paths, filepath.Join(home, ".bashrc"))
		}
	}
	return paths
}

// shellRCPathsForCleanup returns every plausible RC file. Cleanup must be
// broad so that a source line installed under one shell (e.g. bash) gets
// removed even if `promptster done` runs under a different $SHELL. A stale
// source line referencing a deleted hook file is what produces the
// "bash: ~/.promptster/shell-hook.sh: No such file or directory" error
// on every new shell.
func shellRCPathsForCleanup() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".profile"),
	}
}

// installShellHook writes the shell hook script and injects a source line
// into the user's shell RC file(s) so it auto-activates in every new shell.
func installShellHook() (sourceCmd string, err error) {
	p := shellHookPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(p, []byte(shellHookScript()), 0o644); err != nil {
		return "", fmt.Errorf("write shell hook: %w", err)
	}

	// Self-healing source line: if the hook file is ever removed (e.g. the
	// user deletes ~/.promptster/ manually, or shell RCs get out of sync),
	// the RC line silently no-ops instead of printing an error every shell
	// start. `.` is POSIX-portable (works in dash/bash/zsh); `source` is not.
	sourceCmd = fmt.Sprintf(`[ -r "%s" ] && . "%s"`, p, p)

	// Inject into shell RC files
	snippet := fmt.Sprintf("%s\n%s\n%s\n", shellHookMarker, sourceCmd, shellHookMarkerEnd)
	for _, rc := range shellRCPathsForInstall() {
		if alreadyHasHook(rc) {
			continue
		}
		f, err := os.OpenFile(rc, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString("\n" + snippet)
		f.Close()
	}

	return sourceCmd, nil
}

// alreadyHasHook checks if the RC file already has the promptster hook marker.
func alreadyHasHook(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), shellHookMarker)
}

// removeShellHook deletes the shell hook script and removes the source line
// from shell RC files.
func removeShellHook() {
	_ = os.Remove(shellHookPath())

	// Scan every plausible RC file — not just the current $SHELL's — so
	// source lines injected under a different shell still get removed.
	for _, rc := range shellRCPathsForCleanup() {
		removeHookFromRC(rc)
	}
}

// removeHookFromRC removes the promptster hook block from a shell RC file.
func removeHookFromRC(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []string
	inBlock := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == shellHookMarker {
			inBlock = true
			continue
		}
		if strings.TrimSpace(line) == shellHookMarkerEnd {
			inBlock = false
			continue
		}
		if !inBlock {
			lines = append(lines, line)
		}
	}

	// Trim trailing empty lines added by our injection
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	result := strings.Join(lines, "\n") + "\n"
	_ = os.WriteFile(path, []byte(result), 0o644)
}

// cmdHookShellCmd handles `promptster hook shell-cmd <command> <exitCode> <elapsedSec>`.
// It creates a `command` event with source="terminal" and sends it through
// the same ingest pipeline as editor hook events.
func cmdHookShellCmd(args []string) {
	if len(args) < 1 {
		return
	}

	command := args[0]
	exitCode := 0
	elapsedSec := 0

	if len(args) >= 2 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			exitCode = v
		}
	}
	if len(args) >= 3 {
		if v, err := strconv.Atoi(args[2]); err == nil {
			elapsedSec = v
		}
	}

	// Skip empty commands, promptster's own commands, and the source command itself
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return
	}
	if strings.HasPrefix(trimmed, "promptster ") || strings.HasPrefix(trimmed, "_promptster_") {
		return
	}
	if strings.Contains(trimmed, "shell-hook.sh") {
		return
	}

	session, err := loadSession()
	if err != nil || session.SessionID == "" || session.SessionToken == "" {
		return
	}

	// Fall back to session API URL if env var not set
	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	// Build event using the same Event struct and "command" kind as editor hooks
	event := newEvent("command", session.SessionID)
	event.Source = "terminal"
	event.Actor = humanActor()
	event.Provenance = humanProvenance()
	event.Data = map[string]interface{}{
		"command":    command,
		"exitCode":   exitCode,
		"stdout":     "",
		"stderr":     "",
		"durationMs": elapsedSec * 1000,
		"human":      true,
	}

	checkTimeLimit()

	if err := appendEventToLocalBuffer(&event); err != nil {
		hookDebugf("shell-cmd buffer error: %v", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	if err := ingestEventWithClient(client, event, session.SessionToken); err != nil {
		hookDebugf("shell-cmd send error: %v", err)
	}
}
