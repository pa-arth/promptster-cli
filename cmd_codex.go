package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// cmdCodex handles `promptster codex [args...]` — launch codex wired to the
// Promptster proxy for exactly this one process.
//
// WHY THIS COMMAND EXISTS. Codex has no apiKeyHelper equivalent and no
// per-workspace config, so instrumenting it means (a) selecting a custom model
// provider and (b) putting the credential in the process environment. Both used
// to be done OUT OF BAND — the provider written into the user's global
// ~/.codex/config.toml, the credential exported by the shell hook — and both
// leaked past the thing they were meant to scope. The provider hijacked every
// codex on the machine and survived any teardown that did not run; the shell
// export only ever existed in shells that sourced the hook AFTER `start`, which
// is never the shell the candidate is standing in.
//
// Doing both here, per launch, is the whole fix. The overlay dies with the
// process and the token comes straight from the 0600 session.json, so codex
// outside this command is exactly the codex the candidate had before Promptster
// was installed.
func cmdCodex(args []string) {
	// A machine that ran CLI ≤1.9 may still carry that version's managed block in
	// the global codex config. Heal it here too: this is a codex command, so it
	// is the most likely place a candidate with a broken personal codex lands.
	purgeLegacyCodexProxyBlock()

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
	// Same split as auth-token: refusing the launch is unconditional, tearing
	// the session down is not. §8.6, task 2.5e.
	if session.expired() {
		if session.selfEvictArmed() {
			fireBackgroundCleanup("expired")
		}
		codexLaunchFail("this session has expired",
			"Run: promptster start PST-XXXX-XXXX (or promptster reset)")
	}

	// A session started without codex has no consent to meter codex traffic
	// against the hiring team's key, and its rollout watcher is not running — so
	// the run would be billed to somebody and captured by nobody. Refusing beats
	// launching something that silently records nothing.
	//
	// Session.Tools is the authority. It used to be "is our block in the global
	// codex config", which answered a different question: that file is shared by
	// every session the machine has ever run, so a leftover from last week read
	// as consent for today.
	//
	// An EMPTY Tools list is not a wildcard. It means a session recorded before
	// tool selection existed — which is before codex was instrumented at all, so
	// it is Claude-only by definition (see Session.Tools) and has no codex
	// watcher running. Treating empty as "anything goes" was the same permissive
	// read as the old config check, and it landed in the same place: a codex run
	// billed to the hiring team and captured by nobody.
	if !hasTool(session.Tools, toolCodex) {
		started := "claude — this session predates codex support"
		if len(session.Tools) > 0 {
			started = strings.Join(session.Tools, ", ")
		}
		codexLaunchFail("codex is not instrumented for this session — it was started with "+started,
			"Run: promptster start PST-XXXX-XXXX --tools codex")
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

	// Provider overlay first, then the user's args verbatim, so
	// `promptster codex exec "..."`, `promptster codex --model ...` and friends
	// behave as the bare binary would. codex accepts -c both before and after a
	// subcommand; before is the position that works for every subcommand.
	//
	// A user-supplied `-c model_provider=...` later in argv would win, which is
	// correct: an explicit override is a deliberate act, and codex would then be
	// off-proxy and visibly so (it prints the provider at startup), rather than
	// silently pinned by a file they never edited.
	argv := codexLaunchArgv(bin, codexProxyBaseURL(), args)
	if err := execCodex(bin, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not launch codex: %v\n", err)
		os.Exit(1)
	}
}

// codexLaunchArgv builds the child's argv: the binary, our provider overlay,
// then the user's arguments untouched.
func codexLaunchArgv(bin, baseURL string, args []string) []string {
	argv := append([]string{bin}, codexProxyArgs(baseURL)...)
	return append(argv, args...)
}

// codexProxyBaseURL is the Responses-API prefix codex POSTs to (<base>/responses).
func codexProxyBaseURL() string { return apiURL() + "/v1/proxy/openai/v1" }

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
	key := codexProxyTokenEnv + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+token)
}
