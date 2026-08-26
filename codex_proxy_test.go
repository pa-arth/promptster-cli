package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCodexBaseURL = "https://api.test.promptster.ai/v1/proxy/openai/v1"

// isolateCodexEnv points CODEX_HOME and the per-session state dir at temp dirs
// so nothing here can touch the real machine's codex config.
func isolateCodexEnv(t *testing.T) (codexHomeDir, stateDirPath string) {
	t.Helper()
	codexHomeDir = t.TempDir()
	stateDirPath = t.TempDir()
	t.Setenv("CODEX_HOME", codexHomeDir)
	t.Setenv("PROMPTSTER_STATE_DIR", stateDirPath)
	return codexHomeDir, stateDirPath
}

// legacyCodexProxyBlock reproduces, byte for byte, the marker-fenced block CLI
// ≤1.9 wrote into the user's GLOBAL ~/.codex/config.toml. Nothing in the CLI
// generates this any more — it lives here so the purge can be tested against
// what is actually sitting on upgraded machines.
func legacyCodexProxyBlock(baseURL string) string {
	return strings.Join([]string{
		codexProxyMarkerBegin,
		`model_provider = "promptster"`,
		"",
		"[model_providers.promptster]",
		`name = "Promptster"`,
		`base_url = "` + baseURL + `"`,
		`wire_api = "responses"`,
		`env_key = "PROMPTSTER_PROXY_TOKEN"`,
		"requires_openai_auth = false",
		codexProxyMarkerEnd,
	}, "\n")
}

func writeLegacyConfig(t *testing.T, codexHomeDir, userContent string) string {
	t.Helper()
	cfg := filepath.Join(codexHomeDir, "config.toml")
	body := legacyCodexProxyBlock(testCodexBaseURL) + "\n"
	if userContent != "" {
		body += "\n" + userContent
	}
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeLegacyState(t *testing.T, stateDirPath string, s codexProxyState) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirPath, "codex-proxy-state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The overlay is the whole instrumentation now, so it has to carry every field
// codex needs to resolve the provider — and none of the credential.
func TestCodexProxyArgs(t *testing.T) {
	args := codexProxyArgs(testCodexBaseURL)

	if len(args)%2 != 0 {
		t.Fatalf("args must be -c/value pairs, got %v", args)
	}
	var pairs []string
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("arg %d = %q, want -c (%v)", i, args[i], args)
		}
		pairs = append(pairs, args[i+1])
	}
	joined := strings.Join(pairs, "\n")

	for _, want := range []string{
		`model_provider="promptster"`,
		`model_providers.promptster.name="Promptster"`,
		`model_providers.promptster.base_url="` + testCodexBaseURL + `"`,
		`model_providers.promptster.wire_api="responses"`,
		`model_providers.promptster.env_key="PROMPTSTER_PROXY_TOKEN"`,
		`model_providers.promptster.requires_openai_auth=false`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("overlay missing %q\n---\n%s", want, joined)
		}
	}
}

// The credential rides the environment, never the overlay: argv is world-
// readable in `ps`, so a token there would be a token on the process list.
func TestCodexProxyArgs_CarriesNoCredential(t *testing.T) {
	joined := strings.Join(codexProxyArgs(testCodexBaseURL), " ")
	if strings.Contains(joined, "PST-") || strings.Contains(strings.ToLower(joined), "bearer") {
		t.Errorf("overlay appears to contain a credential: %s", joined)
	}
	// env_key NAMES the variable; it must not carry a value for it.
	if strings.Contains(joined, "PROMPTSTER_PROXY_TOKEN=P") {
		t.Errorf("overlay assigns the token instead of naming the variable: %s", joined)
	}
}

// Instrumenting codex must not write to the user's global config at all — that
// is the regression this whole change exists to prevent. Build the launch the
// way cmdCodex does and assert the file is untouched.
func TestCodexLaunchLeavesGlobalConfigUntouched(t *testing.T) {
	codexHomeDir, stateDirPath := isolateCodexEnv(t)
	cfg := filepath.Join(codexHomeDir, "config.toml")
	original := "model_provider = \"openai\"\n\n[tools]\nweb_search = true\n"
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = codexProxyArgs(codexProxyBaseURL())
	_ = envWithProxyToken(os.Environ(), "PST-LIVE")

	after := mustRead(t, cfg)
	if after != original {
		t.Errorf("global codex config was modified:\nwant %q\ngot  %q", original, after)
	}
	if entries, _ := os.ReadDir(stateDirPath); len(entries) != 0 {
		t.Errorf("launch wrote sidecar state: %v", entries)
	}
}

func TestPurgeLegacyBlock_PreservesUserContent(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	user := "# my codex config\n\n[tools]\nweb_search = true\n"
	cfg := writeLegacyConfig(t, codexHomeDir, user)

	purgeLegacyCodexProxyBlock()

	after := mustRead(t, cfg)
	if strings.Contains(after, "promptster") {
		t.Errorf("managed traces remain after purge:\n%s", after)
	}
	if after != user {
		t.Errorf("purge not exact:\nwant %q\ngot  %q", user, after)
	}
}

func TestPurgeLegacyBlock_RestoresDisplacedModelProvider(t *testing.T) {
	codexHomeDir, stateDirPath := isolateCodexEnv(t)
	cfg := writeLegacyConfig(t, codexHomeDir, "model = \"gpt-5.5\"\n\n[tools]\nweb_search = true\n")
	writeLegacyState(t, stateDirPath, codexProxyState{
		ConfigPath:        cfg,
		HadConfig:         true,
		HadModelProvider:  true,
		PrevModelProvider: "openai",
	})

	purgeLegacyCodexProxyBlock()

	after := mustRead(t, cfg)
	if !strings.Contains(after, `model_provider = "openai"`) {
		t.Errorf("original model_provider not restored:\n%s", after)
	}
	if strings.Contains(after, "promptster") {
		t.Errorf("managed traces remain:\n%s", after)
	}
	if !strings.Contains(after, `model = "gpt-5.5"`) || !strings.Contains(after, "[tools]") {
		t.Errorf("user content lost:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(stateDirPath, "codex-proxy-state.json")); !os.IsNotExist(err) {
		t.Errorf("sidecar state survived the purge, stat err = %v", err)
	}
}

func TestPurgeLegacyBlock_RemovesFileItCreated(t *testing.T) {
	codexHomeDir, stateDirPath := isolateCodexEnv(t)
	cfg := writeLegacyConfig(t, codexHomeDir, "")
	writeLegacyState(t, stateDirPath, codexProxyState{ConfigPath: cfg, HadConfig: false})

	purgeLegacyCodexProxyBlock()

	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("expected config.toml removed, stat err = %v", err)
	}
}

// THE behaviour change. The old reconciler only stripped the block when it
// judged the owning session stale — and it judged that from ~/.promptster, so a
// live session kept the block and a wiped state dir was indistinguishable from a
// healthy machine. Nothing writes the block any more, so its presence is a
// leftover regardless of what any session is doing.
func TestPurgeLegacyBlock_IsUnconditional(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	cfg := writeLegacyConfig(t, codexHomeDir, "")
	if err := saveSession(Session{
		SessionID:    "sess-live",
		SessionToken: "PST-LIVE-TOKEN",
		TaskRoot:     t.TempDir(),
		Tools:        []string{toolCodex},
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	purgeLegacyCodexProxyBlock()

	if legacyCodexProxyBlockPresent() {
		t.Error("legacy block survived a purge because a session was live")
	}
	if after := mustRead(t, cfg); strings.Contains(after, "promptster") {
		t.Errorf("managed traces remain:\n%s", after)
	}
}

func TestPurgeLegacyBlock_NoConfigIsNoOp(t *testing.T) {
	isolateCodexEnv(t)
	purgeLegacyCodexProxyBlock()
	if _, err := os.Stat(codexConfigPath()); !os.IsNotExist(err) {
		t.Errorf("purge created a config file where none existed: %v", err)
	}
}

func TestPurgeLegacyBlock_LeavesUnrelatedConfigAlone(t *testing.T) {
	codexHomeDir, _ := isolateCodexEnv(t)
	cfg := filepath.Join(codexHomeDir, "config.toml")
	original := "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\n"
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	purgeLegacyCodexProxyBlock()

	if after := mustRead(t, cfg); after != original {
		t.Errorf("purge touched a config it never wrote:\nwant %q\ngot  %q", original, after)
	}
}

func TestStripCodexProxyBlock(t *testing.T) {
	content := "before\n" + legacyCodexProxyBlock(testCodexBaseURL) + "\nafter\n"
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
