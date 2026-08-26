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
//	Codex  — a custom model_provider in ~/.codex/config.toml, which codex will
//	         not scope to a workspace, so the block is GLOBAL and the credential
//	         has to ride PROMPTSTER_PROXY_TOKEN in the environment.
//
// The asymmetry is forced by the tools, not chosen, which is exactly why a fix
// aimed at one rail keeps reaching for the other's surface. These tests are the
// tripwire: each rail's configuration step must leave the other's files alone.

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

func TestConfiguringCodexLeavesTheClaudeRailAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	ws := t.TempDir()
	// Stand up a Claude rail exactly as `start --tools claude` would.
	if err := configureClaudeProxy(ws, "https://api.example.test/v1/proxy/anthropic", "PST-TEST-KEY0"); err != nil {
		t.Fatalf("configureClaudeProxy: %v", err)
	}
	before := railFingerprint(t, filepath.Join(ws, ".claude"))
	if len(before) == 0 {
		t.Fatal("expected configureClaudeProxy to write the workspace settings")
	}

	// Now bring up the codex rail in the same session.
	if err := configureCodexProxy("https://api.example.test/v1/proxy/openai/v1"); err != nil {
		t.Fatalf("configureCodexProxy: %v", err)
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

	// And the reverse direction of the same invariant: tearing the codex rail
	// back down must not touch Claude's settings either.
	revertCodexProxy()
	afterRevert := railFingerprint(t, filepath.Join(ws, ".claude"))
	for name, content := range before {
		if afterRevert[name] != content {
			t.Errorf("codex revert modified the Claude rail file %q", name)
		}
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

// TestCodexRevertRestoresThePersonalProvider is the failure the cleanup lock
// exists to prevent, asserted directly: after the codex rail is torn down the
// user's own model_provider must be back. A concurrent second cleanup used to
// be able to run this after the state file was deleted, leaving the machine
// pointed at a provider that no longer exists.
func TestCodexRevertRestoresThePersonalProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHomeDir := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHomeDir)
	t.Setenv("PROMPTSTER_STATE_DIR", filepath.Join(home, ".promptster"))

	if err := os.MkdirAll(codexHomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	personal := "model_provider = \"my-own-thing\"\n\n[model_providers.my-own-thing]\nname = \"Mine\"\n"
	if err := os.WriteFile(filepath.Join(codexHomeDir, "config.toml"), []byte(personal), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configureCodexProxy("https://api.example.test/v1/proxy/openai/v1"); err != nil {
		t.Fatalf("configureCodexProxy: %v", err)
	}
	mid, _ := os.ReadFile(filepath.Join(codexHomeDir, "config.toml"))
	if !strings.Contains(string(mid), codexProxyMarkerBegin) {
		t.Fatal("expected the managed block to be installed")
	}

	revertCodexProxy()

	got, _ := os.ReadFile(filepath.Join(codexHomeDir, "config.toml"))
	if strings.Contains(string(got), codexProxyMarkerBegin) {
		t.Error("revert left the managed block behind")
	}
	if !strings.Contains(string(got), `model_provider = "my-own-thing"`) {
		t.Errorf("revert did not restore the user's own model_provider; got:\n%s", got)
	}
	if !strings.Contains(string(got), "[model_providers.my-own-thing]") {
		t.Errorf("revert dropped the user's own provider table; got:\n%s", got)
	}
}
