package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The row this feeds used to print a Claude hooks path whenever a task root
// existed. These cases are written as the inverse of that behaviour, because a
// fix that leaves no assertion behind is one somebody re-does in three months.
func TestClaudeHooksStatus(t *testing.T) {
	writeHook := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".claude", "settings.local.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("codex-only session with no hook file shows no row", func(t *testing.T) {
		// The regression this whole change exists for: observed 2026-08-26 on a
		// hosted box, where `promptster status` announced a Claude hooks path on
		// a session that had deliberately never written one, because only a Codex
		// key was set. cmd_start.go gates the write on the same hasTool predicate.
		_, show := claudeHooksStatus(t.TempDir(), []string{toolCodex})
		if show {
			t.Fatal("a Codex-only session must not report a Claude hooks row")
		}
	})

	t.Run("claude instrumented but file missing says so", func(t *testing.T) {
		// Reachable for real: configureProxyEnv only WARNS when the write fails,
		// so start can complete with Claude instrumented and nothing on disk.
		// Printing the intended path here would make that indistinguishable from
		// success — capture silently not happening.
		root := t.TempDir()
		value, show := claudeHooksStatus(root, []string{toolClaude})
		if !show {
			t.Fatal("an instrumented-but-unconfigured session must report something")
		}
		if value == filepath.Join(root, ".claude", "settings.local.json") {
			t.Fatal("value must not be the bare path — that reads as configured")
		}
		if !strings.Contains(value, "not written") {
			t.Fatalf("value should say the file is not written, got %q", value)
		}
	})

	t.Run("hook file present reports its path", func(t *testing.T) {
		root := writeHook(t)
		value, show := claudeHooksStatus(root, []string{toolClaude})
		if !show {
			t.Fatal("a present hook file must be reported")
		}
		if value != filepath.Join(root, ".claude", "settings.local.json") {
			t.Fatalf("expected the hook path, got %q", value)
		}
	})

	t.Run("present file wins over an empty tool list", func(t *testing.T) {
		// A session started by a CLI older than Session.Tools carries none. If the
		// tool list were consulted first, a hook file that is really there would be
		// hidden. An existing file is ground truth; the tool list only decides
		// whether an ABSENT file is worth mentioning.
		root := writeHook(t)
		value, show := claudeHooksStatus(root, nil)
		if !show || value != filepath.Join(root, ".claude", "settings.local.json") {
			t.Fatalf("a legacy session with a real hook file must still report it, got %q show=%v", value, show)
		}
	})

	t.Run("no task root, no row", func(t *testing.T) {
		if _, show := claudeHooksStatus("", []string{toolClaude}); show {
			t.Fatal("without a task root there is no path to report")
		}
	})
}
