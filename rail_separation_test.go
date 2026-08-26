package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two AI rails are wired through completely different mechanisms and MUST
// stay that way, because a session can run either one alone:
//
//	Claude — apiKeyHelper in <workspace>/.claude/settings.local.json. Workspace
//	         scoped; the credential is fetched by Claude Code at request time
//	         and never enters the shell.
//	Codex  — `-c` overrides passed by `promptster codex` on the launch itself,
//	         plus PROMPTSTER_PROXY_TOKEN in that one child's environment. Codex
//	         will not scope a provider to a workspace, so the only scope narrow
//	         enough is the process.
//
// The asymmetry is forced by the tools, not chosen, which is exactly why a fix
// aimed at one rail keeps reaching for the other's surface. These tests are the
// tripwire: each rail's setup must leave the other's files alone — and the codex
// rail must now leave EVERY file alone, its own included.

// railFingerprint records every path under root, with contents, so a test can
// assert an operation changed nothing there.
func railFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return nil
	})
	return out
}

func TestBringingUpTheCodexRailWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHomeDir := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHomeDir)
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(home, ".promptster"))

	ws := t.TempDir()
	// Stand up a Claude rail exactly as `start --tools claude` would.
	if err := configureClaudeProxy(ws, "https://api.example.test/v1/proxy/anthropic", "PST-TEST-KEY0"); err != nil {
		t.Fatalf("configureClaudeProxy: %v", err)
	}
	before := railFingerprint(t, filepath.Join(ws, ".claude"))
	if len(before) == 0 {
		t.Fatal("expected configureClaudeProxy to write the workspace settings")
	}

	// The user's own codex config, which the codex rail used to rewrite.
	if err := os.MkdirAll(codexHomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	personal := "model_provider = \"my-own-thing\"\n\n[model_providers.my-own-thing]\nname = \"Mine\"\n"
	cfg := filepath.Join(codexHomeDir, "config.toml")
	if err := os.WriteFile(cfg, []byte(personal), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bring up the codex rail in the same session. This is the whole of it: the
	// legacy purge, and the overlay handed to one child process.
	purgeLegacyCodexProxyBlock()
	if args := codexProxyArgs("https://api.example.test/v1/proxy/openai/v1"); len(args) == 0 {
		t.Fatal("expected the codex overlay to carry the provider")
	}

	after := railFingerprint(t, filepath.Join(ws, ".claude"))
	if len(after) != len(before) {
		t.Errorf("codex setup changed the file set under <workspace>/.claude: %d files before, %d after", len(before), len(after))
	}
	for name, content := range before {
		if after[name] != content {
			t.Errorf("codex setup modified the Claude rail file %q", name)
		}
	}

	// And its own global file, which is the leak this rewrite removed: a block
	// written here outlives the session and breaks codex machine-wide.
	if got := mustRead(t, cfg); got != personal {
		t.Errorf("codex setup modified the user's global codex config\n got: %q\nwant: %q", got, personal)
	}
}

func TestConfiguringClaudeLeavesTheCodexRailAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHomeDir := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHomeDir)

	// A pre-existing personal codex config with the user's OWN provider
	// selected. This is the thing a Claude-side change must never disturb —
	// the config is global, so damage here breaks codex everywhere on the
	// machine, not just in the assessment.
	if err := os.MkdirAll(codexHomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	personal := "model_provider = \"my-own-thing\"\n\n[model_providers.my-own-thing]\nname = \"Mine\"\n"
	if err := os.WriteFile(filepath.Join(codexHomeDir, "config.toml"), []byte(personal), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	if err := configureClaudeProxy(ws, "https://api.example.test/v1/proxy/anthropic", "PST-TEST-KEY0"); err != nil {
		t.Fatalf("configureClaudeProxy: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(codexHomeDir, "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if string(got) != personal {
		t.Errorf("Claude rail setup modified the user's global codex config\n got: %q\nwant: %q", got, personal)
	}
	if strings.Contains(string(got), codexProxyMarkerBegin) {
		t.Error("Claude rail setup installed the managed codex block")
	}
}

// TestPurgeRestoresThePersonalProvider is the failure the cleanup lock exists to
// prevent, asserted directly: on a machine a pre-1.10 session left rewritten,
// the purge must put the user's own model_provider back. A concurrent second
// cleanup used to be able to run after the state file was deleted, leaving the
// machine pointed at a provider that no longer exists.
func TestPurgeRestoresThePersonalProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHomeDir := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHomeDir)
	stateDirPath := filepath.Join(home, ".promptster")
	t.Setenv("PROMPTSTER_STATE_DIR", stateDirPath)

	if err := os.MkdirAll(codexHomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDirPath, 0o700); err != nil {
		t.Fatal(err)
	}

	// Exactly what CLI ≤1.9 left on disk: its block prepended, the user's own
	// root model_provider lifted out and parked in the sidecar.
	cfg := filepath.Join(codexHomeDir, "config.toml")
	body := legacyCodexProxyBlock("https://api.example.test/v1/proxy/openai/v1") +
		"\n\n[model_providers.my-own-thing]\nname = \"Mine\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyState(t, stateDirPath, codexProxyState{
		ConfigPath:        cfg,
		HadConfig:         true,
		HadModelProvider:  true,
		PrevModelProvider: "my-own-thing",
	})

	purgeLegacyCodexProxyBlock()

	got := mustRead(t, cfg)
	if strings.Contains(got, codexProxyMarkerBegin) {
		t.Error("purge left the managed block behind")
	}
	if !strings.Contains(got, `model_provider = "my-own-thing"`) {
		t.Errorf("purge did not restore the user's own model_provider; got:\n%s", got)
	}
	if !strings.Contains(got, "[model_providers.my-own-thing]") {
		t.Errorf("purge dropped the user's own provider table; got:\n%s", got)
	}
}
