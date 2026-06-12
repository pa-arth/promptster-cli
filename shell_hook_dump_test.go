package main

import (
	"strings"
	"testing"
)

// TestShellHookScriptStructure is a regression guard against the failure modes
// that motivated the 1.1.0→1.2.0 rewrites: a global hook with an inlined token,
// and shell-exported proxy creds that leak across directories. As of 1.2.0 the
// hook touches NO Anthropic env at all — the proxy is wired via apiKeyHelper.
// If any of these assertions fail in a future edit, the secret may be leaking
// back into the shell.
func TestShellHookScriptStructure(t *testing.T) {
	script := shellHookScript()

	// 1. No literal token marker in the static template.
	if strings.Contains(script, "PST-") {
		t.Errorf("shell hook template should never contain a literal PST token; found one")
	}
	if strings.Contains(script, "promptster proxy >>>") {
		t.Errorf("shell hook should not contain the legacy Anthropic proxy block markers")
	}

	// 2. The Anthropic proxy must NEVER touch the shell — that path moved to
	//    Claude Code's apiKeyHelper in 1.2.0. Any reappearance of these strings
	//    means the Anthropic shell-leak class of bug is back. (Codex is the
	//    exception below: it has no apiKeyHelper equivalent.)
	for _, banned := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_API_KEY",
		"_promptster_sync_proxy_env",
		`eval "$(`,
	} {
		if strings.Contains(script, banned) {
			t.Errorf("shell hook must not reference %q (Anthropic proxy is via apiKeyHelper)", banned)
		}
	}

	// 2b. Codex has no credential helper, so its proxy token DOES ride the env —
	//     but tightly scoped: only inside the workspace, only while our managed
	//     codex provider block is present, sourced at runtime (never a literal),
	//     and unset on leaving the workspace. Guard those properties so a future
	//     edit can't silently widen the codex token's exposure.
	if !strings.Contains(script, "PROMPTSTER_PROXY_TOKEN") {
		t.Errorf("shell hook missing codex PROMPTSTER_PROXY_TOKEN sync")
	}
	if !strings.Contains(script, "_promptster_codex_active") {
		t.Errorf("codex token export must be gated on the managed provider block being present")
	}
	if !strings.Contains(script, "unset PROMPTSTER_PROXY_TOKEN") {
		t.Errorf("codex token must be unset when leaving the workspace")
	}
	if !strings.Contains(script, `auth-token`) {
		t.Errorf("codex token must be sourced at runtime via auth-token, not baked in")
	}

	// 3. TTL self-eviction is preserved: a backgrounded `promptster env` at
	//    shell start fires cleanup for an expired session.
	if !strings.Contains(script, `( "$_promptster_bin" env >/dev/null 2>&1 & )`) {
		t.Errorf("shell hook missing backgrounded TTL self-eviction trigger")
	}

	// 4. Command capture is gated to the workspace tree.
	if !strings.Contains(script, `_promptster_in_workspace`) {
		t.Errorf("shell hook missing _promptster_in_workspace helper")
	}
	if !strings.Contains(script, `_promptster_in_workspace || return 0`) {
		t.Errorf("shell hook should gate command capture on workspace membership")
	}

	// 5. PWD gate uses both exact-match and prefix-match arms so the user can
	//    be at any path inside the workspace.
	if !strings.Contains(script, `"$_promptster_ws"|"$_promptster_ws"/*`) {
		t.Errorf("shell hook missing PWD gate match arms")
	}

	// 6. The command-capture event sink is still wired.
	if !strings.Contains(script, `hook shell-cmd`) {
		t.Errorf("shell hook missing command-capture sink")
	}

	// 7. Non-interactive bailout is preserved (sourced by SSH/scp etc).
	if !strings.Contains(script, "case $- in") {
		t.Errorf("shell hook missing non-interactive bailout")
	}

	// Surface the full rendered script when -v is passed so a human can
	// eyeball the output without spinning up a real install.
	if testing.Verbose() {
		t.Logf("--- rendered shell-hook.sh ---\n%s\n--- end ---", script)
	}
}
