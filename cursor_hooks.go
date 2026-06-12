package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cursorHookPointNames lists the Cursor hook events Promptster registers.
//
// We deliberately register OBSERVER hooks (the after*/post* family) plus the
// single gating hook needed to capture the human prompt (beforeSubmitPrompt).
// Observer hooks have no response that can block or slow the agent, so even a
// crashed/slow handler never interferes with the candidate. We do NOT register
// the gating twins (preToolUse, beforeShellExecution, beforeReadFile,
// beforeMCPExecution, subagentStart) — their after*/post* counterparts carry the
// same data plus the tool output/result.
//
// Source: cursor.com/docs/agent/hooks.
var cursorHookPointNames = []string{
	"beforeSubmitPrompt",  // human prompt (gating — handler emits {"continue":true})
	"afterFileEdit",       // file edits: file_path + edits[]{old_string,new_string}
	"afterShellExecution", // shell commands: command, output, duration
	"postToolUse",         // remaining tools: reads, searches, plan updates, etc.
	"postToolUseFailure",  // tool errors
	"afterMCPExecution",   // MCP tool calls: tool_name, tool_input, result_json
	"subagentStop",        // subagent completion
	"sessionStart",        // session lifecycle
	"sessionEnd",
}

// cursorHooksPath returns the project-local Cursor hooks config path. Promptster
// uses the project-scoped file so capture is workspace-isolated by construction
// (mirrors how Claude hooks live in <workspace>/.claude/settings.local.json).
func cursorHooksPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".cursor", "hooks.json")
}

// cursorHookCommand is the command Cursor invokes for every registered event.
// One command serves all events; the handler switches on hook_event_name read
// from stdin (see normalizeCursor + cmdHook's "cursor" route).
func cursorHookCommand() string {
	return promptsterBin() + " hook cursor"
}

// configureCursorHooks installs Promptster's hooks into the project-local
// Cursor hooks config. It preserves any pre-existing user hooks (backs the file
// up and merges our entries in) and is idempotent.
func configureCursorHooks(workspacePath string) error {
	path := cursorHooksPath(workspacePath)
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

	if isCursorHookConfigured(existing) {
		return nil
	}

	if origData != nil {
		_ = os.WriteFile(path+".promptster-backup", origData, 0o644)
	}

	// Cursor's hooks.json requires a numeric "version" (default 1). Preserve any
	// existing value; otherwise set 1.
	if _, ok := existing["version"]; !ok {
		existing["version"] = 1
	}

	hooksSection, _ := existing["hooks"].(map[string]interface{})
	if hooksSection == nil {
		hooksSection = make(map[string]interface{})
	}

	command := cursorHookCommand()
	for _, event := range cursorHookPointNames {
		arr, _ := hooksSection[event].([]interface{})
		hooksSection[event] = append(arr, cursorHookEntry(command))
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
	return os.Rename(tmp, path)
}

// cursorHookEntry builds a single Cursor hook definition.
func cursorHookEntry(command string) map[string]interface{} {
	return map[string]interface{}{"command": command}
}

// isCursorHookConfigured reports whether Promptster's hook command is already
// registered for every event we install (so re-running start is a no-op).
func isCursorHookConfigured(config map[string]interface{}) bool {
	hooksSection, _ := config["hooks"].(map[string]interface{})
	if hooksSection == nil {
		return false
	}
	for _, event := range cursorHookPointNames {
		arr, _ := hooksSection[event].([]interface{})
		found := false
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

// removeCursorHooks tears down Promptster's Cursor hook config. If we backed up
// a pre-existing user config it is restored verbatim; otherwise (we created the
// file) it is removed, along with an empty .cursor directory.
func removeCursorHooks(workspacePath string) {
	if workspacePath == "" {
		return
	}
	path := cursorHooksPath(workspacePath)
	backup := path + ".promptster-backup"

	if data, err := os.ReadFile(backup); err == nil {
		// Restore the candidate's original config, then drop the backup.
		_ = os.WriteFile(path, data, 0o644)
		_ = os.Remove(backup)
		return
	}

	// No backup → the file is entirely ours, OR it contains a mix. Strip our
	// entries; remove the file if nothing of ours-or-theirs remains.
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	config := make(map[string]interface{})
	if err := json.Unmarshal(data, &config); err != nil {
		// Unparseable and no backup — safest is to remove a file we likely wrote.
		_ = os.Remove(path)
		_ = os.Remove(filepath.Dir(path))
		return
	}
	if stripped, hadOnlyOurs := stripPromptsterCursorHooks(config); hadOnlyOurs {
		_ = os.Remove(path)
		_ = os.Remove(filepath.Dir(path))
	} else {
		if out, err := json.MarshalIndent(stripped, "", "  "); err == nil {
			_ = os.WriteFile(path, out, 0o644)
		}
	}
}

// stripPromptsterCursorHooks removes Promptster hook entries from a parsed Cursor
// config. Returns the cleaned config and whether the file held nothing but our
// entries (i.e. it is safe to delete entirely).
func stripPromptsterCursorHooks(config map[string]interface{}) (map[string]interface{}, bool) {
	hooksSection, _ := config["hooks"].(map[string]interface{})
	if hooksSection == nil {
		// No hooks at all — if the only key is our injected "version", it's ours.
		_, hasVersion := config["version"]
		return config, len(config) == 0 || (len(config) == 1 && hasVersion)
	}
	for event, raw := range hooksSection {
		arr, _ := raw.([]interface{})
		kept := make([]interface{}, 0, len(arr))
		for _, entry := range arr {
			b, _ := json.Marshal(entry)
			if strings.Contains(string(b), "promptster hook") {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooksSection, event)
		} else {
			hooksSection[event] = kept
		}
	}
	if len(hooksSection) == 0 {
		delete(config, "hooks")
		_, hasVersion := config["version"]
		// Only our content remained (possibly plus the version we injected).
		return config, len(config) == 0 || (len(config) == 1 && hasVersion)
	}
	config["hooks"] = hooksSection
	return config, false
}
