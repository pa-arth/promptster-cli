package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdEnv handles `promptster env [--clear]`.
//
// As of 1.2.0 the Anthropic proxy is wired through Claude Code's `apiKeyHelper`
// (see proxy_config.go), NOT through shell env vars — so this command no longer
// exports ANTHROPIC_AUTH_TOKEN/BASE_URL. Its two remaining jobs:
//
//	(no args)  Self-eviction trigger. The shell hook runs this on every new
//	           interactive shell; if the local ExpiresAt has passed we fire a
//	           background `promptster cleanup`, so an abandoned session's RC
//	           line + shell hook disappear on the next terminal. Prints nothing.
//
//	--clear    Prints `unset` lines for any LEGACY shell-exported proxy vars.
//	           Pre-1.2.0 CLIs (and audit tooling that sourced them) put PST
//	           tokens in the shell; this clears them so they can't out-rank the
//	           apiKeyHelper or leak into unrelated terminals. Idempotent: a no-op
//	           for shells that never had them. Safe to `eval`.
//
// Note: the apiKeyHelper itself does NOT leak across directories — it lives in
// <workspace>/.claude/settings.local.json and only applies when Claude Code
// runs with that workspace as its project root. So a `claude` invoked outside
// the workspace uses your own auth with no `--clear` needed. Inside the
// workspace the helper out-ranks a logged-in subscription, so don't expect a
// personal `claude` run in the assessment dir to fall back to your own OAuth.
func cmdEnv(args []string) {
	if len(args) > 0 && args[0] == "--clear" {
		// POSIX `unset` is idempotent for unset vars in bash/zsh.
		fmt.Println("unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL 2>/dev/null || true")
		return
	}

	session, err := loadSession()
	if err != nil {
		return // no session → nothing to do; not an error
	}
	if session.SessionToken == "" || session.TaskRoot == "" {
		return // partial state (redeem ran but start never finished)
	}

	// Local staleness check. If ExpiresAt isn't populated (older session.json
	// written by a pre-feature CLI), we have nothing to compare against — do
	// nothing and let the server-side proxy reject a stale token if needed.
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		fireBackgroundCleanup("expired")
	}
}

// cmdAuthToken prints ONLY the bare session token to stdout — no `export`, no
// trailing newline — for use as a Claude Code `apiKeyHelper`. Claude Code runs
// the helper via /bin/sh and uses its stdout as the credential, sending it on
// BOTH `Authorization: Bearer` and `X-Api-Key`.
//
// This is the parent-process auth path that replaced the shell export in 1.2.0.
// Same staleness self-eviction as cmdEnv: an expired session prints nothing,
// which Claude Code treats as "no helper credential" and falls back to its
// normal auth, so a dead session can't keep routing through the proxy.
func cmdAuthToken(args []string) {
	session, err := loadSession()
	if err != nil {
		return
	}
	if session.SessionToken == "" || session.TaskRoot == "" {
		return
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		fireBackgroundCleanup("expired")
		return
	}
	// No trailing newline: Claude Code uses the raw stdout as the credential.
	fmt.Print(session.SessionToken)
}

// fireBackgroundCleanup detaches a `promptster cleanup --reason <reason>` so an
// expired session tears down its hooks + RC lines without blocking the caller.
// Best-effort: if the binary or the command goes sideways we stay quiet, since
// callers treat no-output as "no session" anyway.
func fireBackgroundCleanup(reason string) {
	bin, _ := os.Executable()
	if bin == "" {
		bin = promptsterBin()
	}
	cmd := exec.Command(bin, "cleanup", "--reason", reason)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Start()
	if cmd.Process != nil {
		_ = cmd.Process.Release() // detach so we don't wait
	}
}

// shellQuote wraps a value in single quotes and escapes any embedded single
// quotes via the standard `'\''` POSIX pattern. Output is safe for `eval`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
