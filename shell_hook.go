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
//     The active workspace is re-resolved on EVERY prompt, and this hook
//     installs itself even in a shell with no session at all. It used to do the
//     opposite — read ~/.promptster/active-workspace once at source time and
//     `return 0` if no session was live yet — and that was a bug, not an
//     optimisation. `promptster start` runs INSIDE an already-open shell, so
//     the shell a candidate is standing in when they read the printed next
//     steps was, by construction, the one shell that had registered nothing:
//     no preexec, no precmd, no PROMPTSTER_PROXY_TOKEN. Terminal capture was
//     silently off there, and codex died on "Missing environment variable:
//     PROMPTSTER_PROXY_TOKEN". A shell that DID have the hook loaded was no
//     better: it held the workspace path captured before `start` ran.
//
//  2. TTL self-eviction — a backgrounded `promptster env`, armed when a session
//     first becomes visible to the shell (not once at shell start, for the same
//     reason as above: the shell that ran `start` had none when its RC was
//     read). It exports nothing; its only job is to fire `promptster cleanup`
//     when the session's local TTL has passed, so an abandoned session's RC
//     line + shell hook disappear on the next new terminal.
//
//  3. Codex launcher — a PWD-gated `codex` shell function that routes to
//     `promptster codex` inside the workspace and to the real binary everywhere
//     else. It replaces the old PROMPTSTER_PROXY_TOKEN export: the credential
//     now goes straight into one codex process instead of into the shell, so
//     there is no exported secret to strand when a session ends, and no global
//     codex config to leave broken. Dispatch is per call, so the function goes
//     inert the moment the session does — even in a shell already open.
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

_promptster_bin="%s"

# Resolve the active workspace pointer (global, written by 'promptster start').
#
# Called from precmd on EVERY prompt, never once at shell start. 'promptster
# start' runs inside a shell that is already open, so a hook that resolved this
# once would be permanently blind in the one shell the candidate is actually
# standing in: no session when the RC was sourced, so no terminal capture and no
# codex token, for the life of that shell. Re-reading a 40-byte file per prompt
# is what it costs for that not to be true.
#
# Returns non-zero and leaves _promptster_ws empty when no session is live,
# which is the normal state for most shells on the machine.
_promptster_resolve_ws() {
  _promptster_ws=""
  [ -f "$HOME/.promptster/active-workspace" ] || return 1
  _promptster_ws="$(cat "$HOME/.promptster/active-workspace" 2>/dev/null)"
  [ -n "$_promptster_ws" ] || return 1
  if [ ! -f "$_promptster_ws/.promptster/session.json" ]; then
    _promptster_ws=""
    return 1
  fi
  # Canonicalize to the PHYSICAL path (resolve symlinks). On macOS /tmp is a
  # symlink to /private/tmp, so a workspace handed out as /tmp/foo would
  # otherwise never match a terminal whose $PWD is /private/tmp/foo, silently
  # dropping every human terminal command. Resolving both sides to their
  # physical path fixes it.
  _promptster_ws_phys="$(cd "$_promptster_ws" 2>/dev/null && pwd -P)"
  [ -n "$_promptster_ws_phys" ] && _promptster_ws="$_promptster_ws_phys"
  return 0
}

# Resolve once now, so the first command typed in this shell is captured and a
# codex launched before the first prompt already sees the token. Failure is the
# ordinary no-session case; the hooks below still install, and no-op.
_promptster_resolve_ws || :

_promptster_in_workspace() {
  # No live session → no workspace to be inside of.
  [ -n "$_promptster_ws" ] || return 1
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

# ── Codex launcher (PWD-gated) ─────────────────────────────────────────────
# Codex needs two things Promptster supplies: a custom model provider and the
# proxy credential in its environment. 'promptster codex' supplies both, scoped
# to that one process (cmd_codex.go). This wrapper is here so that typing the
# thing muscle memory types — plain 'codex' — inside the assessment workspace
# gets the instrumented launch anyway.
#
# WHAT THIS REPLACED, and why the replacement is not the same shape. The hook
# used to put the proxy credential into the shell environment itself, because
# codex read the provider from a GLOBAL ~/.codex/config.toml that 'start' had
# rewritten. So
# every codex on the machine wanted that variable, and a session that ended
# without a clean teardown left the user's personal codex demanding a token no
# shell would ever again export. Nothing is global now: outside the workspace
# this function calls the real binary and changes nothing about it, and the
# credential never enters an environment that outlives one codex process.
#
# The dispatch is re-evaluated on every call, so this function is inert the
# moment the session ends — including in shells that are already open, which a
# config file could never manage.
# A shell upgraded from a hook that DID export the credential is still carrying
# it. Drop it here: nothing reads it any more, and it names a session that is
# almost certainly over.
[ -n "${PROMPTSTER_PROXY_TOKEN:-}" ] && unset PROMPTSTER_PROXY_TOKEN

_promptster_codex_real="$(command -v codex 2>/dev/null)"
case "$_promptster_codex_real" in
  /*)
    # Only wrap a real binary. If 'codex' already resolves to a function or an
    # alias, it is the user's, and shadowing it would be our second uninvited
    # global change to their setup.
    codex() {
      if _promptster_resolve_ws && _promptster_in_workspace; then
        "$_promptster_bin" codex "$@"
      else
        "$_promptster_codex_real" "$@"
      fi
    }
    ;;
esac

# TTL self-eviction (backgrounded, no output). 'promptster env' fires a
# background cleanup when the session's local ExpiresAt has passed; otherwise
# it does nothing. Detached so the prompt never blocks on the spawn.
#
# Armed when a session first becomes VISIBLE to this shell, re-armed if a
# different workspace shows up later — not once at source time. A source-time
# gate spawns nothing in a shell that had no session when its RC was read, and
# 'promptster start' runs inside an already-open shell, so that is precisely
# the one shell the candidate is standing in: it would never schedule the
# eviction for its entire life. Same resolve-once bug as the workspace lookup
# above, so it gets the same treatment — resolve per prompt, act on change.
#
# Keyed on the workspace rather than fired every prompt so a live session costs
# one spawn per shell, not one per command, and a shell with no session still
# costs nothing at all.
_promptster_ttl_armed=""
_promptster_ttl_check() {
  [ -n "$_promptster_ws" ] || return 0
  [ "$_promptster_ws" = "$_promptster_ttl_armed" ] && return 0
  _promptster_ttl_armed="$_promptster_ws"
  ( "$_promptster_bin" env >/dev/null 2>&1 & ) 2>/dev/null
}
_promptster_ttl_check

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
    # Before anything reads $_promptster_ws: a session may have been started
    # (or ended) since the last prompt, in this very shell.
    _promptster_resolve_ws || :
    _promptster_ttl_check
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
    # Same as zsh's precmd: re-resolve before anything reads $_promptster_ws.
    _promptster_resolve_ws || :
    _promptster_ttl_check
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
`, bin)
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

// systemShellInitPaths are the container/system-wide shell init files a hosted
// image can pre-wire (openspec §1.4 bakes the source line into the image).
var systemShellInitPaths = []string{
	"/etc/bash.bashrc",
	"/etc/bashrc",
	"/etc/zsh/zshrc",
	"/etc/zshrc",
	"/etc/profile",
	"/etc/profile.d/promptster.sh",
}

// systemShellInitSourcesHook reports whether a system-wide init file already
// sources our hook, so the per-user RC injection would be a duplicate.
//
// This is a MEASUREMENT, not an assumption about the lane. "The hosted image
// sources it" is a claim about an image built in another repo on another
// schedule; skipping the injection because the lane is hosted would silently
// drop terminal capture the day that image ships without the line. Skipping
// because the line is demonstrably there cannot.
func systemShellInitSourcesHook() bool {
	for _, p := range systemShellInitPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), shellHookMarker) {
			return true
		}
	}
	return false
}

// installShellHook writes the shell hook script and injects a source line
// into the user's shell RC file(s) so it auto-activates in every new shell.
func installShellHook() (sourceCmd string, err error) {
	return installShellHookWithRC(true)
}

// installShellHookWithRC writes the hook script and, when injectRC is true,
// injects the source line into the user's RC files.
func installShellHookWithRC(injectRC bool) (sourceCmd string, err error) {
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

	if !injectRC {
		return sourceCmd, nil
	}

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
