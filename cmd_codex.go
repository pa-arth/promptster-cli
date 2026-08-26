package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdCodex handles `promptster codex [args...]` — launch codex with the proxy
// credential already in its process environment.
//
// WHY THIS COMMAND EXISTS. Codex has no apiKeyHelper equivalent, so our managed
// provider block in ~/.codex/config.toml reads the credential from
// $PROMPTSTER_PROXY_TOKEN (codex_proxy.go). The only thing that ever exported
// that variable was the shell hook — and the hook is sourced from an RC file, so
// the shell the candidate ran `promptster start` in never had it. Following the
// printed next steps in that same shell produced codex's
// "Missing environment variable: PROMPTSTER_PROXY_TOKEN" every time.
//
// The hook is fixed too (shell_hook.go now re-resolves per prompt), but a hook
// can only ever fix SOME shells: it needs an RC file, an interactive shell, and
// a supported shell family. This command depends on none of that — it reads the
// 0600 session.json and hands the token to the child directly — which is why it
// is what `start` prints as step 2.
func cmdCodex(args []string) {
	// Same guard sequence as cmdAuthToken (cmd_env.go), including the expired
	// self-eviction. Difference: auth-token stays silent because Claude Code
	// treats empty stdout as "no credential" and falls back; here a human is
	// waiting on a launch, so every refusal says what to run instead.
	session, err := loadSession()
	if err != nil {
		codexLaunchFail("no active Promptster session",
			"Run: promptster start PST-XXXX-XXXX --tools codex")
	}
	if session.SessionToken == "" || session.TaskRoot == "" {
		codexLaunchFail("this session has no token or workspace recorded (redeem ran but start never finished)",
			"Run: promptster start PST-XXXX-XXXX --tools codex")
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		fireBackgroundCleanup("expired")
		codexLaunchFail("this session has expired",
			"Run: promptster start PST-XXXX-XXXX (or promptster reset)")
	}

	// No managed block means this session was not started with --tools codex, so
	// codex would run on the candidate's own OpenAI auth: no proxy, no metering,
	// and — because the model traffic never reaches us — no captured prompts.
	// Refusing beats launching something that silently records nothing.
	if !codexProxyConfigured() {
		codexLaunchFail("codex is not instrumented for this session — the Promptster provider is not in "+codexConfigPath(),
			"Run: promptster start --tools codex")
	}

	bin, err := exec.LookPath(toolBinaryName(toolCodex))
	if err != nil {
		hint := toolInstallHint(toolCodex)
		codexLaunchFail("codex not found on PATH", "Install it:\n  "+hint)
	}

	// The codex rollout watcher matches captured sessions by the cwd recorded in
	// the rollout JSONL (codexRolloutMatchesWorkspace in cmd_codex_watch.go), so
	// a codex started outside the workspace is classified "no match" and emits
	// zero events — a session that reads to a reviewer as a candidate who did
	// nothing. Move there rather than launch an uncaptured run.
	if cwd, cwdErr := os.Getwd(); cwdErr != nil || !pathWithin(resolvePath(cwd), resolvePath(session.TaskRoot)) {
		fmt.Fprintf(os.Stderr, "  Not in the assessment workspace — starting codex in %s\n", session.TaskRoot)
		if chErr := os.Chdir(session.TaskRoot); chErr != nil {
			codexLaunchFail(fmt.Sprintf("could not enter the workspace %s: %v", session.TaskRoot, chErr),
				"Run: promptster start PST-XXXX-XXXX --tools codex")
		}
	}

	env := envWithProxyToken(os.Environ(), session.SessionToken)

	// Extra args are forwarded verbatim so `promptster codex exec "..."`,
	// `promptster codex --model ...` and friends behave as the bare binary would.
	argv := append([]string{bin}, args...)
	if err := execCodex(bin, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not launch codex: %v\n", err)
		os.Exit(1)
	}
}

// codexLaunchFail prints a refusal plus the command that fixes it, and exits
// non-zero. Never returns.
func codexLaunchFail(problem, fix string) {
	fmt.Fprintf(os.Stderr, "error: %s\n  %s\n", problem, fix)
	os.Exit(1)
}

// envWithProxyToken returns env with PROMPTSTER_PROXY_TOKEN set to token,
// replacing any existing value rather than appending a second entry — a
// duplicate key is resolved differently across platforms, and the stale one
// winning is exactly the 401 this command exists to prevent.
func envWithProxyToken(env []string, token string) []string {
	const key = "PROMPTSTER_PROXY_TOKEN="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+token)
}
