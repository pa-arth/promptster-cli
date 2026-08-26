package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesEqualSameSizeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("abcdef"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("abcxyz"), 0o755); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if filesEqual(src, dst) {
		t.Fatal("filesEqual returned true for different same-size content")
	}
}

func TestInstallBinaryOverwritesStaleSameSizeBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "bin")
	dest := filepath.Join(destDir, "promptster")

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir destDir: %v", err)
	}
	if err := os.WriteFile(src, []byte("fresh!"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dest, []byte("stale!"), 0o755); err != nil {
		t.Fatalf("write dest: %v", err)
	}

	if err := installBinary(src, dest); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "fresh!" {
		t.Fatalf("dest contents = %q, want %q", string(got), "fresh!")
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dest mode = %o, want 755", info.Mode().Perm())
	}
}

// taskRootWithinRepo is shared by the clone path and the adopt path precisely
// because they used to compute the task boundary independently — and the adopt
// path simply did not, so a hosted monorepo assessment captured, diffed and
// bundled the whole repository.
func TestTaskRootWithinRepo(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "w", "repo")
	cases := []struct {
		name   string
		subdir string
		want   string
	}{
		{"no subdir is the repo root", "", root},
		{"blank subdir is the repo root", "   ", root},
		{"dot is the repo root", ".", root},
		{"a plain subdir", "services/api", filepath.Join(root, "services", "api")},
		{"an untidy subdir is cleaned", "./services//api/", filepath.Join(root, "services", "api")},
		{"a leading separator is not an absolute path", string(filepath.Separator) + "services", filepath.Join(root, "services")},
		// The server supplies this value, but a task root outside the repository
		// would bundle the candidate's home directory. The root is the safe answer.
		{"climbing out falls back to the root", "../elsewhere", root},
		{"a bare .. falls back to the root", "..", root},
		{"a name merely starting with dots is kept", "..hidden", filepath.Join(root, "..hidden")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskRootWithinRepo(root, c.subdir); got != c.want {
				t.Fatalf("taskRootWithinRepo(%q, %q) = %q, want %q", root, c.subdir, got, c.want)
			}
		})
	}
}

// TestNextStepLaunchCodexCarriesCredential pins the asymmetry that fixes the
// reported bug: `claude` is printed bare because its credential rides an
// apiKeyHelper read at runtime, while codex is printed as `promptster codex`
// because codex reads PROMPTSTER_PROXY_TOKEN from its process environment and
// the shell that just ran `start` has never had it. Printing a bare `codex`
// here is what produced "Missing environment variable: PROMPTSTER_PROXY_TOKEN".
func TestNextStepLaunchCodexCarriesCredential(t *testing.T) {
	t.Run("codex only", func(t *testing.T) {
		cmd, desc, noun := nextStepLaunch(false, true)
		if cmd != "promptster codex" {
			t.Errorf("codex-only launch = %q, want %q", cmd, "promptster codex")
		}
		if !strings.Contains(desc, "credential") {
			t.Errorf("codex launch description should say why it is not a bare codex; got %q", desc)
		}
		if noun != "Codex" {
			t.Errorf("noun = %q, want Codex", noun)
		}
	})

	t.Run("claude only stays bare", func(t *testing.T) {
		cmd, desc, _ := nextStepLaunch(true, false)
		if cmd != "claude" {
			t.Errorf("claude-only launch = %q, want claude", cmd)
		}
		// The note is codex-specific; claude needs no explanation.
		if strings.Contains(desc, "credential") {
			t.Errorf("claude launch should not carry the codex note; got %q", desc)
		}
	})

	t.Run("both", func(t *testing.T) {
		cmd, _, noun := nextStepLaunch(true, true)
		if !strings.Contains(cmd, "promptster codex") {
			t.Errorf("combined launch must route codex through promptster; got %q", cmd)
		}
		if strings.Contains(cmd, "or: codex") {
			t.Errorf("combined launch must not offer a bare codex; got %q", cmd)
		}
		if !strings.Contains(noun, "Codex") || !strings.Contains(noun, "Claude Code") {
			t.Errorf("noun = %q, want both tools named", noun)
		}
	})

	t.Run("neither falls back to claude", func(t *testing.T) {
		if cmd, _, _ := nextStepLaunch(false, false); cmd != "claude" {
			t.Errorf("fallback launch = %q, want claude", cmd)
		}
	})
}
