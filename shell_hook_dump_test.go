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

	// 2b. And as of 1.10.0 the CODEX token doesn't touch the shell either. It
	//     used to: codex read its provider from a GLOBAL ~/.codex/config.toml
	//     that `start` rewrote, so the credential had to be exported into the
	//     environment of any shell that might run codex — and a session that
	//     ended badly left that global config demanding a variable nothing would
	//     ever export again. The hook now installs a PWD-gated `codex` function
	//     that shells out to `promptster codex`, which puts the token in one
	//     child process and nowhere else. No exported secret, nothing to unset,
	//     nothing to strand.
	if strings.Contains(script, "export PROMPTSTER_PROXY_TOKEN") {
		t.Errorf("shell hook must not export the codex token; 'promptster codex' passes it to one child")
	}
	if strings.Contains(script, "_promptster_sync_codex_token") {
		t.Errorf("the codex token sync is gone; its reappearance means the shell-leak class of bug is back")
	}
	if !strings.Contains(script, "codex() {") {
		t.Errorf("shell hook missing the PWD-gated codex launcher")
	}
	//     Inside the workspace it must route through us; outside it must run the
	//     real binary and change nothing about it.
	if !strings.Contains(script, `"$_promptster_bin" codex "$@"`) {
		t.Errorf("codex launcher must route to 'promptster codex' inside the workspace")
	}
	if !strings.Contains(script, `"$_promptster_codex_real" "$@"`) {
		t.Errorf("codex launcher must fall through to the real binary outside the workspace")
	}
	//     Dispatch per call, not per source: a shell open when the session ends
	//     must go back to plain codex without being restarted.
	if !strings.Contains(script, "if _promptster_resolve_ws && _promptster_in_workspace; then") {
		t.Errorf("codex launcher must re-resolve the session on every call, not once at source time")
	}
	//     And it must only wrap a real binary — never shadow the user's own
	//     codex alias or function.
	if !strings.Contains(script, `_promptster_codex_real="$(command -v codex 2>/dev/null)"`) {
		t.Errorf("codex launcher must resolve the real binary before wrapping")
	}

	// 3. TTL self-eviction is preserved: a backgrounded `promptster env` fires
	//    cleanup for an expired session.
	if !strings.Contains(script, `( "$_promptster_bin" env >/dev/null 2>&1 & )`) {
		t.Errorf("shell hook missing backgrounded TTL self-eviction trigger")
	}
	//    It must still cost nothing in a sessionless shell...
	if !strings.Contains(script, `[ -n "$_promptster_ws" ] || return 0`) {
		t.Errorf("TTL self-eviction should not spawn a process in every sessionless shell")
	}
	//    ...but the gate must be a FUNCTION re-evaluated per prompt, not a bare
	//    source-time test. A source-time gate spawns nothing in the shell that
	//    ran `promptster start` — which, since start runs inside an already-open
	//    shell, is the one shell the candidate is actually using. That is the
	//    same resolve-once bug as §8, and it lived one line below the fix for it.
	if strings.Contains(script, `[ -n "$_promptster_ws" ] && ( "$_promptster_bin" env`) {
		t.Errorf("TTL self-eviction must not be gated once at source time; the start-shell would never arm it")
	}
	if !strings.Contains(script, "_promptster_ttl_check() {") {
		t.Errorf("TTL self-eviction must be a function so it can re-arm when a session appears mid-shell")
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

	// 8. THE REGRESSION. The hook used to read ~/.promptster/active-workspace
	//    once at source time and `return 0` when no session was live. Because
	//    `promptster start` runs inside a shell that is already open, that made
	//    the candidate's own shell the one shell guaranteed to have registered
	//    nothing — no command capture, and (before the launcher replaced the
	//    export) no PROMPTSTER_PROXY_TOKEN, which is how `codex` died on
	//    "Missing environment variable" immediately after start printed
	//    "Codex ready".
	//
	//    Two properties keep that from coming back: resolution is a FUNCTION
	//    (so it can be called again), and it is called from the per-prompt
	//    hooks of both shell families.
	if !strings.Contains(script, "_promptster_resolve_ws()") {
		t.Errorf("workspace resolution must be a function so it can re-run per prompt")
	}
	for _, hook := range []string{
		"promptster_precmd() {",      // zsh
		"_promptster_prompt_cmd() {", // bash
	} {
		idx := strings.Index(script, hook)
		if idx == -1 {
			t.Fatalf("shell hook missing %q", hook)
		}
		body := script[idx:]
		if end := strings.Index(body, "\n  }"); end != -1 {
			body = body[:end]
		}
		for _, call := range []string{
			"_promptster_resolve_ws", // workspace, per prompt
			"_promptster_ttl_check",  // expiry eviction, armed when a session appears
		} {
			if !strings.Contains(body, call) {
				t.Errorf("%s must call %s; a session started mid-shell is invisible otherwise", hook, call)
			}
		}
	}

	// 9. And the early bail is GONE. A shell sourcing this with no active
	//    session must still install its hooks — that is the whole point of 8.
	//    The only permitted early return is the non-interactive bailout (7).
	if strings.Contains(script, "session.json\" ]; then\n  return 0") {
		t.Errorf("hook must not skip installation when no session is live at source time")
	}
	if got := strings.Count(script, "return 0 2>/dev/null || :"); got != 1 {
		t.Errorf("expected exactly one early return (the non-interactive bailout), got %d", got)
	}

	// 10. _promptster_in_workspace must be false — not erroring, not true —
	//     when there is no session, since it now runs in shells that have none.
	if !strings.Contains(script, `[ -n "$_promptster_ws" ] || return 1`) {
		t.Errorf("_promptster_in_workspace must return false when no session is live")
	}

	// Surface the full rendered script when -v is passed so a human can
	// eyeball the output without spinning up a real install.
	if testing.Verbose() {
		t.Logf("--- rendered shell-hook.sh ---\n%s\n--- end ---", script)
	}
}
