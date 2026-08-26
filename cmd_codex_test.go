package main

import (
	"strings"
	"testing"
)

// TestEnvWithProxyToken — the child's environment must carry exactly one
// PROMPTSTER_PROXY_TOKEN, and it must be ours. A duplicate key resolves
// differently across platforms and libc versions, and a stale value winning is
// precisely the 401 `promptster codex` exists to prevent.
func TestEnvWithProxyToken(t *testing.T) {
	count := func(env []string) int {
		n := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, "PROMPTSTER_PROXY_TOKEN=") {
				n++
			}
		}
		return n
	}

	t.Run("injects into a clean environment", func(t *testing.T) {
		env := envWithProxyToken([]string{"PATH=/usr/bin", "HOME=/home/x"}, "PST-NEW")
		if count(env) != 1 {
			t.Fatalf("token entries = %d, want 1", count(env))
		}
		if env[len(env)-1] != "PROMPTSTER_PROXY_TOKEN=PST-NEW" {
			t.Errorf("token entry = %q", env[len(env)-1])
		}
		// Unrelated vars survive: this is the child's whole environment.
		if !contains(env, "PATH=/usr/bin") || !contains(env, "HOME=/home/x") {
			t.Errorf("existing environment was not preserved: %v", env)
		}
	})

	t.Run("replaces a stale token rather than appending", func(t *testing.T) {
		env := envWithProxyToken(
			[]string{"PROMPTSTER_PROXY_TOKEN=PST-STALE", "PATH=/usr/bin"},
			"PST-NEW",
		)
		if count(env) != 1 {
			t.Fatalf("token entries = %d, want 1 (a duplicate lets the stale one win)", count(env))
		}
		if contains(env, "PROMPTSTER_PROXY_TOKEN=PST-STALE") {
			t.Error("stale token survived into the child environment")
		}
		if !contains(env, "PROMPTSTER_PROXY_TOKEN=PST-NEW") {
			t.Error("new token missing from the child environment")
		}
	})

	t.Run("does not disturb a similarly named variable", func(t *testing.T) {
		env := envWithProxyToken([]string{"PROMPTSTER_PROXY_TOKEN_OLD=keep"}, "PST-NEW")
		if !contains(env, "PROMPTSTER_PROXY_TOKEN_OLD=keep") {
			t.Error("prefix match ate an unrelated variable")
		}
	})
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestCodexLaunchArgv — the instrumentation now lives entirely in argv, so its
// position in argv is load-bearing.
func TestCodexLaunchArgv(t *testing.T) {
	const base = "https://api.test.promptster.ai/v1/proxy/openai/v1"

	t.Run("binary first, overlay next, user args last", func(t *testing.T) {
		argv := codexLaunchArgv("/usr/local/bin/codex", base, []string{"exec", "fix the bug"})
		if argv[0] != "/usr/local/bin/codex" {
			t.Fatalf("argv[0] = %q, want the resolved binary", argv[0])
		}
		overlay := codexProxyArgs(base)
		for i, want := range overlay {
			if argv[1+i] != want {
				t.Fatalf("argv[%d] = %q, want %q", 1+i, argv[1+i], want)
			}
		}
		// codex accepts -c before a subcommand and that is the position that
		// works for every subcommand, so the user's args must come after.
		tail := argv[1+len(overlay):]
		if len(tail) != 2 || tail[0] != "exec" || tail[1] != "fix the bug" {
			t.Errorf("user args not forwarded verbatim: %v", tail)
		}
	})

	t.Run("a user override comes later and therefore wins", func(t *testing.T) {
		argv := codexLaunchArgv("codex", base, []string{"-c", `model_provider="openai"`})
		ours, theirs := -1, -1
		for i, a := range argv {
			if a == `model_provider="promptster"` {
				ours = i
			}
			if a == `model_provider="openai"` {
				theirs = i
			}
		}
		if ours == -1 || theirs == -1 {
			t.Fatalf("expected both provider selections in argv: %v", argv)
		}
		// Deliberate and visible — codex prints the provider it resolved — beats
		// silently pinning a user who asked for something else.
		if theirs < ours {
			t.Errorf("user override at %d precedes ours at %d; it must be able to win", theirs, ours)
		}
	})

	t.Run("no args still launches instrumented", func(t *testing.T) {
		argv := codexLaunchArgv("codex", base, nil)
		if len(argv) != 1+len(codexProxyArgs(base)) {
			t.Errorf("bare launch argv = %v", argv)
		}
	})
}

// `promptster codex` refuses to launch for a session that did not select codex.
// The guard is what keeps a run from being billed to the hiring team's key and
// captured by nobody — the codex rollout watcher only runs for a codex session.
func TestCodexInstrumentationGuard(t *testing.T) {
	cases := []struct {
		name    string
		tools   []string
		allowed bool
	}{
		{"codex selected", []string{"codex"}, true},
		{"both selected", []string{"claude", "codex"}, true},
		{"claude only", []string{"claude"}, false},
		// THE HOLE. An empty Tools list is a session recorded before tool
		// selection existed — i.e. before codex was instrumented at all, so it is
		// Claude-only by definition and has no watcher running. Reading empty as
		// "anything goes" let codex run uncaptured on a legacy session.
		{"legacy session with no tools recorded", nil, false},
		{"legacy session with an empty tools list", []string{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasTool(c.tools, toolCodex); got != c.allowed {
				t.Errorf("hasTool(%v, codex) = %v, want %v — this is the launch guard", c.tools, got, c.allowed)
			}
		})
	}
}
