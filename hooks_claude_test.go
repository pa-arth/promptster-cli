package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureClaudeHooks_WritesProjectSettingsLocal(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "task")
	t.Setenv("HOME", home)

	if err := configureClaudeHooks(workspace); err != nil {
		t.Fatalf("configureClaudeHooks: %v", err)
	}

	projectPath := claudeProjectSettingsPath(workspace)
	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project Claude config: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("unmarshal project Claude config: %v", err)
	}
	if !isHookConfigured(settings) {
		t.Fatalf("Promptster hooks missing from %s: %s", projectPath, string(data))
	}

	if _, err := os.Stat(claudeUserSettingsPath()); !os.IsNotExist(err) {
		t.Fatalf("expected no global Claude config write, got err=%v", err)
	}
}

func TestConfigureClaudeHooks_MigratesLegacyGlobalPromptsterHooks(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "task")
	t.Setenv("HOME", home)

	globalPath := claudeUserSettingsPath()
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("mkdir global config dir: %v", err)
	}

	bin := promptsterBin()
	legacy := map[string]interface{}{
		"theme": "light",
		"hooks": map[string]interface{}{
			"PreToolUse": []interface{}{
				map[string]interface{}{"matcher": "*", "hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": bin + " hook pre-tool-use"},
				}},
				map[string]interface{}{"matcher": "Bash", "hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": "my-linter"},
				}},
			},
			"PostToolUse": []interface{}{
				map[string]interface{}{"matcher": "*", "hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": bin + " hook post-tool-use"},
				}},
			},
		},
	}
	writeJSON(t, globalPath, legacy)

	if err := configureClaudeHooks(workspace); err != nil {
		t.Fatalf("configureClaudeHooks: %v", err)
	}

	projectData, err := os.ReadFile(claudeProjectSettingsPath(workspace))
	if err != nil {
		t.Fatalf("read project config: %v", err)
	}
	var projectSettings map[string]interface{}
	if err := json.Unmarshal(projectData, &projectSettings); err != nil {
		t.Fatalf("unmarshal project config: %v", err)
	}
	if !isHookConfigured(projectSettings) {
		t.Fatalf("project config missing Promptster hooks: %s", string(projectData))
	}

	globalData, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("read migrated global config: %v", err)
	}
	if strings.Contains(string(globalData), "promptster hook") {
		t.Fatalf("legacy global Promptster hook entries still present: %s", string(globalData))
	}

	var globalSettings map[string]interface{}
	if err := json.Unmarshal(globalData, &globalSettings); err != nil {
		t.Fatalf("unmarshal migrated global config: %v", err)
	}
	if globalSettings["theme"] != "light" {
		t.Fatalf("expected unrelated global setting to remain, got %v", globalSettings["theme"])
	}
	serialized, _ := json.Marshal(globalSettings)
	if !strings.Contains(string(serialized), "my-linter") {
		t.Fatalf("expected non-Promptster global hook to remain: %s", string(globalData))
	}
	if _, err := os.Stat(globalPath + ".promptster-backup"); err != nil {
		t.Fatalf("expected global backup to exist: %v", err)
	}
}

func TestIsHookPointConfigured(t *testing.T) {
	settings := map[string]interface{}{
		"hooks": map[string]interface{}{
			"PreToolUse": []interface{}{
				hookEntry(promptsterBin() + " hook pre-tool-use"),
			},
			"PostToolUse": []interface{}{
				map[string]interface{}{"matcher": "Bash", "hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": "my-linter"},
				}},
			},
		},
	}

	if !isHookPointConfigured(settings, "PreToolUse") {
		t.Errorf("expected PreToolUse to be detected as configured")
	}
	if isHookPointConfigured(settings, "PostToolUse") {
		t.Errorf("PostToolUse has only non-promptster hooks; should not be configured")
	}
	if isHookPointConfigured(settings, "Stop") {
		t.Errorf("Stop has no entries; should not be configured")
	}
	if isHookPointConfigured(map[string]interface{}{}, "PreToolUse") {
		t.Errorf("empty settings should not be configured")
	}
}

func TestConfigureClaudeHooks_ReRunStillCleansLegacyGlobalHooks(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "task")
	t.Setenv("HOME", home)

	if err := configureClaudeHooks(workspace); err != nil {
		t.Fatalf("initial configureClaudeHooks: %v", err)
	}

	globalPath := claudeUserSettingsPath()
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("mkdir global config dir: %v", err)
	}
	writeJSON(t, globalPath, map[string]interface{}{
		"hooks": map[string]interface{}{
			"PreToolUse": []interface{}{
				hookEntry(promptsterBin() + " hook pre-tool-use"),
			},
		},
	})

	if err := configureClaudeHooks(workspace); err != nil {
		t.Fatalf("second configureClaudeHooks: %v", err)
	}

	globalData, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("read migrated global config: %v", err)
	}
	if strings.Contains(string(globalData), "promptster hook") {
		t.Fatalf("stale global Promptster hooks remained after rerun: %s", string(globalData))
	}
}
