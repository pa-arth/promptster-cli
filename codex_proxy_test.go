package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCodexBaseURL = "https://api.test.promptster.ai/v1/proxy/openai/v1"

// isolateCodexEnv points CODEX_HOME and the per-session state dir at temp dirs
// so codex config mutation + the sidecar state file never touch the real machine.
func isolateCodexEnv(t *testing.T) (codexHomeDir, stateDirPath string) {
	t.Helper()
	codexHomeDir = t.TempDir()
	stateDirPath = t.TempDir()
	t.Setenv("CODEX_HOME", codexHomeDir)
	t.Setenv("PROMPTSTER_STATE_DIR", stateDirPath)
	return codexHomeDir, stateDirPath
}

func TestConfigureCodexProxy_NewFile(t *testing.T) {
	isolateCodexEnv(t)

	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatalf("configureCodexProxy: %v", err)
	}

	data, err := os.ReadFile(codexConfigPath())
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		codexProxyMarkerBegin,
		codexProxyMarkerEnd,
		`model_provider = "promptster"`,
		`[model_providers.promptster]`,
		`base_url = "` + testCodexBaseURL + `"`,
		`wire_api = "responses"`,
		`env_key = "PROMPTSTER_PROXY_TOKEN"`,
		`requires_openai_auth = false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %q\n---\n%s", want, s)
		}
	}
	// The token must NEVER be written to the global config file.
	if strings.Contains(s, "PST-") || strings.Contains(strings.ToLower(s), "bearer") {
		t.Errorf("config appears to contain a credential:\n%s", s)
	}

	if !codexProxyConfigured() {
		t.Error("codexProxyConfigured() = false after configure")
	}

	// We created the file → revert must delete it entirely.
	revertCodexProxy()
	if _, err := os.Stat(codexConfigPath()); !os.IsNotExist(err) {
		t.Errorf("expected config.toml removed after revert, stat err = %v", err)
	}
}

func TestConfigureCodexProxy_PreservesUserContentAndRevertsExactly(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	original := "# my codex config\n\n[tools]\nweb_search = true\n"
	cfg := filepath.Join(codexHomeDir, "config.toml")
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatalf("configureCodexProxy: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if !strings.Contains(string(got), "[tools]") || !strings.Contains(string(got), "web_search = true") {
		t.Errorf("user content lost:\n%s", got)
	}
	if !strings.HasPrefix(string(got), codexProxyMarkerBegin) {
		t.Errorf("managed block should be prepended:\n%s", got)
	}

	revertCodexProxy()
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("config deleted but should be preserved: %v", err)
	}
	if string(after) != original {
		t.Errorf("revert not exact:\nwant %q\ngot  %q", original, string(after))
	}
}

func TestConfigureCodexProxy_DisplacesAndRestoresModelProvider(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	original := "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\n\n[tools]\nweb_search = true\n"
	cfg := filepath.Join(codexHomeDir, "config.toml")
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatalf("configureCodexProxy: %v", err)
	}
	got := mustRead(t, cfg)
	// Exactly one model_provider assignment, and it's ours.
	if strings.Count(got, "model_provider = ") != 1 {
		t.Errorf("expected exactly one model_provider line:\n%s", got)
	}
	if !strings.Contains(got, `model_provider = "promptster"`) {
		t.Errorf("our model_provider not set:\n%s", got)
	}
	// The user's other root key + table must survive.
	if !strings.Contains(got, `model = "gpt-5.5"`) || !strings.Contains(got, "[tools]") {
		t.Errorf("user content lost:\n%s", got)
	}

	revertCodexProxy()
	after := mustRead(t, cfg)
	if !strings.Contains(after, `model_provider = "openai"`) {
		t.Errorf("original model_provider not restored:\n%s", after)
	}
	if strings.Contains(after, "promptster") {
		t.Errorf("managed traces remain after revert:\n%s", after)
	}
	if !strings.Contains(after, `model = "gpt-5.5"`) || !strings.Contains(after, "[tools]") {
		t.Errorf("user content lost after revert:\n%s", after)
	}
}

func TestConfigureCodexProxy_IdempotentReapply(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	cfg := filepath.Join(codexHomeDir, "config.toml")
	if err := os.WriteFile(cfg, []byte("model_provider = \"openai\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := configureCodexProxy(testCodexBaseURL); err != nil {
			t.Fatalf("configureCodexProxy #%d: %v", i, err)
		}
	}
	got := mustRead(t, cfg)
	if n := strings.Count(got, codexProxyMarkerBegin); n != 1 {
		t.Errorf("expected exactly one managed block after re-apply, got %d:\n%s", n, got)
	}

	// Original model_provider must still round-trip after multiple re-applies.
	revertCodexProxy()
	after := mustRead(t, cfg)
	if !strings.Contains(after, `model_provider = "openai"`) {
		t.Errorf("original model_provider lost across re-apply:\n%s", after)
	}
}

func TestReconcileCodexProxyIfStale_NoSessionStripsBlock(t *testing.T) {
	isolateCodexEnv(t)
	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatal(err)
	}
	// No session.json in the state dir → stale → reconcile strips the block.
	reconcileCodexProxyIfStale()
	if codexProxyConfigured() {
		t.Error("stale managed block was not stripped by reconcile")
	}
}

func TestReconcileCodexProxyIfStale_LiveSessionKeepsBlock(t *testing.T) {
	isolateCodexEnv(t)
	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatal(err)
	}
	// A live, unexpired session owns the block → reconcile must keep it.
	if err := saveSession(Session{
		SessionID:    "sess-live",
		SessionToken: "PST-LIVE-TOKEN",
		TaskRoot:     t.TempDir(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	reconcileCodexProxyIfStale()
	if !codexProxyConfigured() {
		t.Error("live session's managed block was wrongly stripped")
	}
}

func TestReconcileCodexProxyIfStale_ExpiredSessionStripsBlock(t *testing.T) {
	isolateCodexEnv(t)
	if err := configureCodexProxy(testCodexBaseURL); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(Session{
		SessionID:    "sess-expired",
		SessionToken: "PST-OLD",
		TaskRoot:     t.TempDir(),
		ExpiresAt:    time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	reconcileCodexProxyIfStale()
	if codexProxyConfigured() {
		t.Error("expired session's managed block was not stripped")
	}
}

func TestStripCodexProxyBlock(t *testing.T) {
	content := "before\n" + codexProxyBlock(testCodexBaseURL) + "\nafter\n"
	cleaned, had := stripCodexProxyBlock(content)
	if !had {
		t.Error("expected block detected")
	}
	if strings.Contains(cleaned, codexProxyMarkerBegin) || strings.Contains(cleaned, "promptster") {
		t.Errorf("block not fully removed: %q", cleaned)
	}
	if !strings.Contains(cleaned, "before") || !strings.Contains(cleaned, "after") {
		t.Errorf("surrounding content lost: %q", cleaned)
	}

	// Absent block → unchanged, had=false.
	plain := "model = \"gpt-5.5\"\n"
	cleaned2, had2 := stripCodexProxyBlock(plain)
	if had2 || cleaned2 != plain {
		t.Errorf("strip altered block-free content: had=%v got=%q", had2, cleaned2)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
