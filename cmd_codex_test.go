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
