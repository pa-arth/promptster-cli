package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeCursor_BeforeSubmitPrompt_NewSchema(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "beforeSubmitPrompt",
		"cursor_version":  "2.6.14",
		"prompt":          "hello from cursor",
		"conversation_id": "conv-1",
		"generation_id":   "gen-1",
		"model":           "default",
		"transcript_path": "/tmp/transcript.jsonl",
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "prompt" {
		t.Fatalf("kind = %q, want prompt", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["text"] != "hello from cursor" {
		t.Fatalf("text = %#v", data["text"])
	}
	if data["conversationId"] != "conv-1" {
		t.Fatalf("conversationId = %#v", data["conversationId"])
	}
	if data["generationId"] != "gen-1" {
		t.Fatalf("generationId = %#v", data["generationId"])
	}
	if data["model"] != "default" {
		t.Fatalf("model = %#v", data["model"])
	}
	if data["transcriptPath"] != "/tmp/transcript.jsonl" {
		t.Fatalf("transcriptPath = %#v", data["transcriptPath"])
	}
}

func TestDetectSource_CursorNewSchema(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "beforeSubmitPrompt",
		"cursor_version":  "2.6.14",
		"conversation_id": "conv-1",
	}
	if got := detectSource(payload); got != "cursor" {
		t.Fatalf("detectSource = %q, want cursor", got)
	}
}

func TestNormalizeCursor_PostToolUse_Read(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "Read",
		"tool_input":      map[string]interface{}{"file_path": "/home/user/project/main.go"},
		"tool_output":     `{"file_path":"/home/user/project/main.go","content_length":1234}`,
		"duration":        29.48,
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "file_read" {
		t.Fatalf("kind = %q, want file_read", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["path"] != "/home/user/project/main.go" {
		t.Fatalf("path = %#v, want /home/user/project/main.go", data["path"])
	}
}

// postToolUse(Shell) is intentionally skipped — commands are captured by the
// dedicated afterShellExecution hook, so emitting here too would double-count.
func TestNormalizeCursor_PostToolUse_ShellSkipped(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "Shell",
		"tool_input":      map[string]interface{}{"command": "ls -la"},
		"tool_output":     `{"exit_code":0,"stdout":"total 42\n","stderr":""}`,
	}

	if _, ok := normalizeCursor(payload, "sess-1"); ok {
		t.Fatal("expected postToolUse(Shell) to be skipped (owned by afterShellExecution)")
	}
}

// afterShellExecution is the channel we register for commands — it carries the
// command output and duration.
func TestNormalizeCursor_AfterShellExecution(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "afterShellExecution",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"command":         "ls -la",
		"output":          "total 42\n",
		"duration":        float64(12),
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "command" {
		t.Fatalf("kind = %q, want command", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["command"] != "ls -la" {
		t.Fatalf("command = %#v", data["command"])
	}
	if data["stdout"] != "total 42\n" {
		t.Fatalf("stdout = %#v", data["stdout"])
	}
	// No exit code is fabricated — Cursor doesn't provide one.
	if _, present := data["exitCode"]; present {
		t.Fatalf("exitCode should be absent, got %#v", data["exitCode"])
	}
	// durationMs must be an integer — CommandEventData types it as int, so a
	// float would 400 at ingest.
	if dms, ok := data["durationMs"].(int64); !ok || dms != 12 {
		t.Fatalf("durationMs = %#v (%T), want int64 12", data["durationMs"], data["durationMs"])
	}
}

// postToolUse(StrReplace) is intentionally skipped — file edits are captured by
// the dedicated afterFileEdit hook (richer edits[] payload), so emitting here
// too would double-count.
func TestNormalizeCursor_PostToolUse_EditSkipped(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "StrReplace",
		"tool_input": map[string]interface{}{
			"path":       "/home/user/project/main.go",
			"old_string": "foo",
			"new_string": "bar",
		},
		"tool_output": `{}`,
	}

	if _, ok := normalizeCursor(payload, "sess-1"); ok {
		t.Fatal("expected postToolUse(StrReplace) to be skipped (owned by afterFileEdit)")
	}
}

// afterFileEdit is the channel we register for edits — file_path + edits[].
func TestNormalizeCursor_AfterFileEdit(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "afterFileEdit",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"file_path":       "/home/user/project/main.go",
		"edits": []interface{}{
			map[string]interface{}{"old_string": "foo\nbar", "new_string": "baz"},
			map[string]interface{}{"old_string": "", "new_string": "added line"},
		},
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "file_diff" {
		t.Fatalf("kind = %q, want file_diff", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["path"] != "/home/user/project/main.go" {
		t.Fatalf("path = %#v", data["path"])
	}
	diff, _ := data["diff"].(string)
	if diff == "" {
		t.Fatal("diff is empty")
	}
	// First edit: 2 removed (foo, bar), 1 added (baz). Second edit: 1 added.
	if data["linesRemoved"] != 2 {
		t.Fatalf("linesRemoved = %#v, want 2", data["linesRemoved"])
	}
	if data["linesAdded"] != 2 {
		t.Fatalf("linesAdded = %#v, want 2", data["linesAdded"])
	}
	if data["attribution"] != "likely_ai" {
		t.Fatalf("attribution = %#v", data["attribution"])
	}
}

func TestNormalizeCursor_SessionStartEnd(t *testing.T) {
	start, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "sessionStart",
		"cursor_version":  "2026.02.27",
		"conversation_id": "conv-1",
		"session_id":      "cur-sess-9",
		"composer_mode":   "agent",
	}, "sess-1")
	if !ok || start.Kind != "session_start" {
		t.Fatalf("sessionStart → kind=%q ok=%v, want session_start", start.Kind, ok)
	}

	end, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "sessionEnd",
		"cursor_version":  "2026.02.27",
		"conversation_id": "conv-1",
		"session_id":      "cur-sess-9",
		"reason":          "user_closed",
		"final_status":    "completed",
	}, "sess-1")
	if !ok || end.Kind != "session_end" {
		t.Fatalf("sessionEnd → kind=%q ok=%v, want session_end", end.Kind, ok)
	}
	data, _ := end.Data.(map[string]interface{})
	if data["finalStatus"] != "completed" {
		t.Fatalf("finalStatus = %#v", data["finalStatus"])
	}
}

func TestNormalizeCursor_AfterMCPExecution(t *testing.T) {
	e, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "afterMCPExecution",
		"cursor_version":  "2026.02.27",
		"conversation_id": "conv-1",
		"tool_name":       "search_docs",
		"tool_input":      map[string]interface{}{"query": "hooks"},
		"result_json":     `{"hits":3}`,
		"duration":        float64(40),
	}, "sess-1")
	if !ok || e.Kind != "mcp_call" {
		t.Fatalf("afterMCPExecution → kind=%q ok=%v, want mcp_call", e.Kind, ok)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["tool"] != "search_docs" {
		t.Fatalf("tool = %#v", data["tool"])
	}
}

func TestNormalizeCursor_PostToolUse_Grep(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "Grep",
		"tool_input": map[string]interface{}{
			"pattern": "func main",
			"path":    "/home/user/project",
		},
		"tool_output": `{"results":[{"file":"main.go"},{"file":"cmd.go"}]}`,
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "file_search" {
		t.Fatalf("kind = %q, want file_search", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["pattern"] != "func main" {
		t.Fatalf("pattern = %#v", data["pattern"])
	}
	if data["cwd"] != "/home/user/project" {
		t.Fatalf("cwd = %#v", data["cwd"])
	}
}

func TestNormalizeCursor_PostToolUse_Glob(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "Glob",
		"tool_input": map[string]interface{}{
			"glob_pattern":     "**/*.go",
			"target_directory": "/home/user/project",
		},
		"tool_output": `{"filenames":["main.go","cmd.go","util.go"]}`,
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "file_search" {
		t.Fatalf("kind = %q, want file_search", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["pattern"] != "**/*.go" {
		t.Fatalf("pattern = %#v", data["pattern"])
	}
	if data["cwd"] != "/home/user/project" {
		t.Fatalf("cwd = %#v", data["cwd"])
	}
}

func TestNormalizeCursor_PostToolUse_TodoWrite(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "TodoWrite",
		"tool_input": map[string]interface{}{
			"todos": []interface{}{
				map[string]interface{}{"id": "1", "content": "Fix bug", "status": "in_progress"},
			},
		},
		"tool_output": `{}`,
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
}

func TestNormalizeCursor_PostToolUse_GenericTool(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "SomeCustomTool",
		"tool_input":      map[string]interface{}{"key": "value"},
		"tool_output":     `{"result":"ok"}`,
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "tool_use" {
		t.Fatalf("kind = %q, want tool_use", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["toolName"] != "SomeCustomTool" {
		t.Fatalf("toolName = %#v", data["toolName"])
	}
}

func TestNormalizeCursor_PostToolUse_ToolOutputAsMap(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"tool_name":       "Read",
		"tool_input":      map[string]interface{}{"file_path": "/tmp/test.txt"},
		"tool_output":     map[string]interface{}{"content": "hello world"},
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "file_read" {
		t.Fatalf("kind = %q, want file_read", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["path"] != "/tmp/test.txt" {
		t.Fatalf("path = %#v", data["path"])
	}
	if data["contentLength"] != 11 {
		t.Fatalf("contentLength = %#v, want 11", data["contentLength"])
	}
}

func TestNormalizePostToolUseByTool_ClaudeCodeAndCursorParity(t *testing.T) {
	toolInput := map[string]interface{}{"file_path": "/tmp/test.go"}
	toolResponse := map[string]interface{}{"content": "package main"}

	ccPayload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "Read",
		"tool_input":      toolInput,
		"tool_response":   toolResponse,
	}
	ccEvent, ccOK := normalizeClaudeCode(ccPayload, "sess-cc")

	cursorPayload := map[string]interface{}{
		"hook_event_name": "postToolUse",
		"cursor_version":  "2026.02.27",
		"conversation_id": "conv-1",
		"tool_name":       "Read",
		"tool_input":      toolInput,
		"tool_output":     mustJSON(toolResponse),
	}
	cursorEvent, cursorOK := normalizeCursor(cursorPayload, "sess-cur")

	if !ccOK || !cursorOK {
		t.Fatalf("ccOK=%v cursorOK=%v", ccOK, cursorOK)
	}
	if ccEvent.Kind != cursorEvent.Kind {
		t.Fatalf("kind mismatch: cc=%q cursor=%q", ccEvent.Kind, cursorEvent.Kind)
	}

	ccData, _ := ccEvent.Data.(map[string]interface{})
	curData, _ := cursorEvent.Data.(map[string]interface{})
	if ccData["path"] != curData["path"] {
		t.Fatalf("path mismatch: cc=%q cursor=%q", ccData["path"], curData["path"])
	}
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestNormalizeCursor_Stop_ExtractsStatus(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "stop",
		"cursor_version":  "2026.02.27-e7d2ef6",
		"conversation_id": "conv-1",
		"status":          "completed",
		"loop_count":      float64(5),
	}

	e, ok := normalizeCursor(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeCursor returned false")
	}
	if e.Kind != "session_end" {
		t.Fatalf("kind = %q, want session_end", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["status"] != "completed" {
		t.Fatalf("status = %#v, want completed", data["status"])
	}
	if data["loopCount"] != 5 {
		t.Fatalf("loopCount = %#v, want 5", data["loopCount"])
	}
}
