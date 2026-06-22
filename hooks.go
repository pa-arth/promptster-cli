package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// claudeUserSettingsPath returns the path to ~/.claude/settings.json.
func claudeUserSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// claudeProjectSettingsPath returns the project-local Claude settings path.
// Promptster uses the local project settings file so hook config stays
// per-project and does not become the user's global Claude default.
func claudeProjectSettingsPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".claude", "settings.local.json")
}

// promptsterBin returns the canonical path to the promptster binary
// in the user's install directory (~/.promptster/bin/promptster).
func promptsterBin() string {
	home, _ := os.UserHomeDir()
	name := "promptster"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, ".promptster", "bin", name)
}

// runningEditors returns the subset of the given tools whose launch binary is
// currently running, in the order of `tools`. Hooks live in the workspace, so a
// tool window opened before `start` (anywhere) won't capture this session — the
// caller warns about each running tool by name.
func runningEditors(tools []string) []string {
	var running []string
	for _, t := range tools {
		if isProcessRunning(toolBinaryName(t)) {
			running = append(running, t)
		}
	}
	return running
}

// isProcessRunning returns true if a process with the given name (case-insensitive,
// exact match) is currently running. Uses pgrep, which is available on macOS and Linux.
func isProcessRunning(name string) bool {
	return exec.Command("pgrep", "-ix", name).Run() == nil
}

// killEditors sends SIGTERM to each running tool so it can be reopened with hooks.
func killEditors(tools []string) {
	for _, t := range tools {
		_ = exec.Command("pkill", "-ix", toolBinaryName(t)).Run()
	}
}

// configureHooks configures Claude Code hooks in the workspace.
func configureHooks(workspacePath string) error {
	if err := configureClaudeHooks(workspacePath); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	// Best-effort: the /explain slash command is a convenience, not load-bearing.
	// A write failure shouldn't fail hook configuration.
	if err := installExplainCommand(workspacePath); err != nil {
		hookDebugf("install explain command: %v", err)
	}
	return nil
}

// configureClaudeHooks installs Promptster hooks into the project-local Claude
// settings file and removes Promptster's legacy global hooks if present.
func configureClaudeHooks(workspacePath string) error {
	path := claudeProjectSettingsPath(workspacePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	existing := make(map[string]interface{})
	var origData []byte
	if data, err := os.ReadFile(path); err == nil {
		origData = data
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}

	bin := promptsterBin()

	if isHookConfigured(existing) {
		return migrateLegacyClaudeHooks()
	}

	if origData != nil {
		_ = os.WriteFile(path+".promptster-backup", origData, 0o644)
	}

	hooksSection, _ := existing["hooks"].(map[string]interface{})
	if hooksSection == nil {
		hooksSection = make(map[string]interface{})
	}

	// Register all Claude Code hook points that Promptster needs.
	// PreToolUse/PostToolUse capture tool calls (commands, file edits, reads, etc.)
	// UserPromptSubmit captures every prompt the user sends.
	// Stop captures turn-end metadata (token usage, cost, duration).
	// Notification is used for session lifecycle events.
	claudeHookPoints := []string{
		"PreToolUse",
		"PostToolUse",
		"UserPromptSubmit",
		"Stop",
		"Notification",
	}
	for _, hookPoint := range claudeHookPoints {
		subcommand := "post-tool-use"
		if hookPoint == "PreToolUse" || hookPoint == "UserPromptSubmit" {
			subcommand = "pre-tool-use"
		}
		arr, _ := hooksSection[hookPoint].([]interface{})
		hooksSection[hookPoint] = append(arr, hookEntry(bin+" hook "+subcommand))
	}

	existing["hooks"] = hooksSection

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := migrateLegacyClaudeHooks(); err != nil {
		return err
	}
	return nil
}

// migrateLegacyClaudeHooks removes Promptster hook entries from the legacy
// user-level Claude settings file so project hooks do not run twice.
func migrateLegacyClaudeHooks() error {
	path := claudeUserSettingsPath()
	existing := make(map[string]interface{})

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy Claude settings %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("parse legacy Claude settings %s: %w", path, err)
	}
	if !hasAnyPromptsterHook(existing) {
		return nil
	}

	updated, changed := removePromptsterClaudeHooks(existing)
	if !changed {
		return nil
	}

	_ = os.WriteFile(path+".promptster-backup", data, 0o644)

	encoded, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal legacy Claude settings: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return fmt.Errorf("write legacy Claude settings tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

// hookEntry builds a Claude Code hook list entry for settings.json.
func hookEntry(command string) map[string]interface{} {
	return map[string]interface{}{
		"matcher": "*",
		"hooks": []interface{}{
			map[string]interface{}{"type": "command", "command": command},
		},
	}
}

func removePromptsterClaudeHooks(settings map[string]interface{}) (map[string]interface{}, bool) {
	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		return settings, false
	}

	changed := false
	for _, key := range claudeHookPointNames {
		arr, _ := hooksSection[key].([]interface{})
		if arr == nil {
			continue
		}

		kept := make([]interface{}, 0, len(arr))
		for _, entry := range arr {
			b, _ := json.Marshal(entry)
			if strings.Contains(string(b), "promptster hook") {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}

		if len(kept) == 0 {
			delete(hooksSection, key)
			continue
		}
		hooksSection[key] = kept
	}

	if len(hooksSection) == 0 {
		delete(settings, "hooks")
		return settings, changed
	}

	settings["hooks"] = hooksSection
	return settings, changed
}

// isHookConfigured returns true when a "promptster hook" command is already
// present anywhere in the Claude PreToolUse or PostToolUse hook arrays.
// claudeHookPointNames lists all Claude Code hook points Promptster registers.
var claudeHookPointNames = []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "Stop", "Notification"}

// hasAnyPromptsterHook returns true if ANY hook entry contains "promptster hook".
// Used for legacy migration where not all hook points may be present.
func hasAnyPromptsterHook(settings map[string]interface{}) bool {
	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		return false
	}
	for _, key := range claudeHookPointNames {
		arr, _ := hooksSection[key].([]interface{})
		for _, entry := range arr {
			b, _ := json.Marshal(entry)
			if strings.Contains(string(b), "promptster hook") {
				return true
			}
		}
	}
	return false
}

func isHookConfigured(settings map[string]interface{}) bool {
	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		return false
	}
	// Consider configured if ALL expected hook points have a promptster entry.
	for _, key := range claudeHookPointNames {
		found := false
		arr, _ := hooksSection[key].([]interface{})
		for _, entry := range arr {
			b, _ := json.Marshal(entry)
			if strings.Contains(string(b), "promptster hook") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
