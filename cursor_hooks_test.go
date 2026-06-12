package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureCursorHooks_WritesAndIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	if err := configureCursorHooks(ws); err != nil {
		t.Fatalf("configureCursorHooks: %v", err)
	}

	path := cursorHooksPath(ws)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v", err)
	}
	// Cursor requires a numeric version.
	if config["version"] == nil {
		t.Fatal("hooks.json missing version")
	}
	hooks, _ := config["hooks"].(map[string]interface{})
	if hooks == nil {
		t.Fatal("hooks.json missing hooks section")
	}
	for _, event := range cursorHookPointNames {
		arr, _ := hooks[event].([]interface{})
		if len(arr) != 1 {
			t.Fatalf("event %q: got %d entries, want 1", event, len(arr))
		}
	}
	if !strings.Contains(string(data), "promptster hook cursor") {
		t.Fatal("hooks.json missing the promptster hook cursor command")
	}

	// Idempotent: a second call must not duplicate entries.
	if err := configureCursorHooks(ws); err != nil {
		t.Fatalf("second configureCursorHooks: %v", err)
	}
	data2, _ := os.ReadFile(path)
	var config2 map[string]interface{}
	_ = json.Unmarshal(data2, &config2)
	hooks2, _ := config2["hooks"].(map[string]interface{})
	for _, event := range cursorHookPointNames {
		arr, _ := hooks2[event].([]interface{})
		if len(arr) != 1 {
			t.Fatalf("after re-run, event %q has %d entries, want 1", event, len(arr))
		}
	}
}

func TestRemoveCursorHooks_RemovesFileWeCreated(t *testing.T) {
	ws := t.TempDir()
	if err := configureCursorHooks(ws); err != nil {
		t.Fatalf("configureCursorHooks: %v", err)
	}
	removeCursorHooks(ws)
	if _, err := os.Stat(cursorHooksPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("hooks.json should be removed, stat err = %v", err)
	}
}

func TestCursorHooks_PreservesAndRestoresExisting(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".cursor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{
  "version": 1,
  "hooks": {
    "afterFileEdit": [
      { "command": "my-own-formatter.sh" }
    ]
  }
}`
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := configureCursorHooks(ws); err != nil {
		t.Fatalf("configureCursorHooks: %v", err)
	}
	merged, _ := os.ReadFile(path)
	if !strings.Contains(string(merged), "my-own-formatter.sh") {
		t.Fatal("user's existing hook was clobbered")
	}
	if !strings.Contains(string(merged), "promptster hook cursor") {
		t.Fatal("promptster hook was not merged in")
	}

	// Removal restores the candidate's original config verbatim.
	removeCursorHooks(ws)
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hooks.json should still exist after restore: %v", err)
	}
	if strings.Contains(string(restored), "promptster hook") {
		t.Fatal("promptster hook survived removal")
	}
	if !strings.Contains(string(restored), "my-own-formatter.sh") {
		t.Fatal("user's hook was not restored")
	}
	if _, err := os.Stat(path + ".promptster-backup"); !os.IsNotExist(err) {
		t.Fatal("backup file should be cleaned up after restore")
	}
}
