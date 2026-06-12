package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSettingsFile is a tiny helper for the proxy-config assertions.
func readSettingsFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return m
}

// TestConfigureClaudeProxyFreshSession verifies a clean workspace gets the
// apiKeyHelper + base URL and NO baked-in token.
func TestConfigureClaudeProxyFreshSession(t *testing.T) {
	ws := t.TempDir()
	if err := configureClaudeProxy(ws, "https://api.example/v1/proxy/anthropic", "PST-FRESH-9999"); err != nil {
		t.Fatalf("configureClaudeProxy: %v", err)
	}

	settings := readSettingsFile(t, claudeProjectSettingsPath(ws))

	helper, _ := settings["apiKeyHelper"].(string)
	if !strings.Contains(helper, "auth-token") {
		t.Errorf("apiKeyHelper not set to a `... auth-token` command, got %q", helper)
	}
	// The token must NOT appear anywhere in the settings file — that's the
	// whole point of sourcing it from session.json via the helper.
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), "PST-FRESH-9999") {
		t.Errorf("session token leaked into settings.local.json: %s", raw)
	}

	env, _ := settings["env"].(map[string]interface{})
	if env == nil || env["ANTHROPIC_BASE_URL"] != "https://api.example/v1/proxy/anthropic" {
		t.Errorf("ANTHROPIC_BASE_URL not set correctly, env=%v", env)
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Errorf("ANTHROPIC_AUTH_TOKEN must not be written in 1.2.0")
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY must not be written in 1.2.0")
	}
	wantHelper := shellQuote(promptsterBin()) + " auth-token"
	if helper != wantHelper {
		t.Errorf("apiKeyHelper = %q, want %q", helper, wantHelper)
	}
}

// TestConfigureClaudeProxyUpgradesStaleSession is the resumed-session matrix
// case: a workspace whose settings still carry a pre-1.2.0 baked-in token (and
// an unrelated env var) must have the token stripped, the helper added, and the
// unrelated keys preserved.
func TestConfigureClaudeProxyUpgradesStaleSession(t *testing.T) {
	ws := t.TempDir()
	settingsPath := claudeProjectSettingsPath(ws)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a 1.1.x settings file: baked-in token + a user env var + hooks.
	stale := `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "PST-OLD-0000",
    "ANTHROPIC_API_KEY": "PST-OLD-0000",
    "ANTHROPIC_BASE_URL": "https://old/proxy",
    "MY_OWN_VAR": "keepme"
  },
  "hooks": {"UserPromptSubmit": [{"x": 1}]}
}`
	if err := os.WriteFile(settingsPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configureClaudeProxy(ws, "https://new/proxy", "PST-NEW-1111"); err != nil {
		t.Fatalf("configureClaudeProxy: %v", err)
	}

	settings := readSettingsFile(t, settingsPath)
	env, _ := settings["env"].(map[string]interface{})

	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Errorf("stale ANTHROPIC_AUTH_TOKEN was not stripped on resume")
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("stale ANTHROPIC_API_KEY was not stripped on resume")
	}
	if env["ANTHROPIC_BASE_URL"] != "https://new/proxy" {
		t.Errorf("base URL not updated, got %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["MY_OWN_VAR"] != "keepme" {
		t.Errorf("unrelated env var was clobbered, env=%v", env)
	}
	if helper, _ := settings["apiKeyHelper"].(string); !strings.Contains(helper, "auth-token") {
		t.Errorf("apiKeyHelper not added on resume, got %q", helper)
	}
	if settings["hooks"] == nil {
		t.Errorf("existing hooks block was dropped on resume")
	}
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), "PST-OLD-0000") || strings.Contains(string(raw), "PST-NEW-1111") {
		t.Errorf("a token leaked into settings.local.json: %s", raw)
	}
}
