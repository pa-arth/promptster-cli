package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Actor + attribution coverage for the fluency-grading capture layer: every
// emitted event must say who acted (ai/human/system), and git-watcher diffs
// must distinguish fresh human edits from human revisions of AI output.

func TestNormalizeClaudeCode_PromptActorHuman(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"prompt":          "fix the bug in trace.py",
	}
	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Fatalf("actor = %#v, want human", e.Actor)
	}
	if e.Provenance == nil || e.Provenance.Attribution != "likely_human" {
		t.Fatalf("provenance = %#v, want likely_human", e.Provenance)
	}
}

func TestNormalizeClaudeCode_StopActorAiWithMessage(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name":        "Stop",
		"last_assistant_message": "I fixed the dedup branch and the tests pass.",
	}
	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "ai_response" {
		t.Fatalf("kind = %q, want ai_response", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "ai" {
		t.Fatalf("actor = %#v, want ai", e.Actor)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["lastAssistantMessage"] != "I fixed the dedup branch and the tests pass." {
		t.Fatalf("lastAssistantMessage = %#v", data["lastAssistantMessage"])
	}
}

func TestNormalizeClaudeCode_PostToolUseEditActorAi(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "Edit",
		"tool_input": map[string]interface{}{
			"file_path":  "src/app.ts",
			"old_string": "a",
			"new_string": "b",
		},
	}
	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "file_diff" {
		t.Fatalf("kind = %q, want file_diff", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "ai" {
		t.Fatalf("actor = %#v, want ai", e.Actor)
	}
}

func TestNormalizeClaudeCode_PostToolUseDefaultActorBackstop(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "SomeNewTool",
		"tool_input":      map[string]interface{}{"x": 1},
	}
	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "tool_use" {
		t.Fatalf("kind = %q, want tool_use", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "ai" {
		t.Fatalf("actor = %#v, want ai backstop", e.Actor)
	}
}

func TestNormalizeClaudeCode_ExitPlanModePlanDecision(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "ExitPlanMode",
		"tool_input": map[string]interface{}{
			"plan": "1. Read trace.py\n2. Fix dedup branch\n3. Run tests",
		},
		"tool_response": map[string]interface{}{"behavior": "allow"},
	}
	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "plan_decision" {
		t.Fatalf("kind = %q, want plan_decision", e.Kind)
	}
	if e.Actor == nil || e.Actor.Type != "human" {
		t.Fatalf("actor = %#v, want human (plan approval is the candidate's act)", e.Actor)
	}
	data, _ := e.Data.(map[string]interface{})
	plan, _ := data["planPreview"].(string)
	if plan == "" {
		t.Fatalf("planPreview missing: %#v", data)
	}
}

func TestNormalizeCursor_ActorAssignments(t *testing.T) {
	prompt, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "beforeSubmitPrompt",
		"cursor_version":  "2.6.14",
		"prompt":          "hello",
	}, "sess-1")
	if !ok || prompt.Actor == nil || prompt.Actor.Type != "human" {
		t.Fatalf("cursor prompt actor = %#v, want human", prompt.Actor)
	}

	cmd, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "afterShellExecution",
		"cursor_version":  "2.6.14",
		"command":         "pnpm test",
		"output":          "ok",
	}, "sess-1")
	if !ok || cmd.Actor == nil || cmd.Actor.Type != "ai" {
		t.Fatalf("cursor command actor = %#v, want ai", cmd.Actor)
	}

	end, ok := normalizeCursor(map[string]interface{}{
		"hook_event_name": "stop",
		"cursor_version":  "2.6.14",
		"status":          "done",
	}, "sess-1")
	if !ok || end.Actor == nil || end.Actor.Type != "system" {
		t.Fatalf("cursor stop actor = %#v, want system", end.Actor)
	}
}

func TestGitPollProvenance(t *testing.T) {
	fresh := gitPollProvenance(false)
	if fresh.Attribution != "likely_human" {
		t.Fatalf("fresh attribution = %q, want likely_human", fresh.Attribution)
	}
	revised := gitPollProvenance(true)
	if revised.Attribution != "ai_revised_by_human" {
		t.Fatalf("revised attribution = %q, want ai_revised_by_human", revised.Attribution)
	}
}

func TestAiPathsLedger_RecordAndQuery(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	if wasAiTouchedPath("sess-1", "src/app.ts") {
		t.Fatal("empty ledger should report untouched")
	}
	recordAiTouchedPath("sess-1", "src/app.ts")
	if !wasAiTouchedPath("sess-1", "src/app.ts") {
		t.Fatal("recorded path should report touched")
	}
	if wasAiTouchedPath("sess-1", "src/other.ts") {
		t.Fatal("unrecorded path should report untouched")
	}
	// A different session must not inherit AI paths from a reused workspace.
	if wasAiTouchedPath("sess-2", "src/app.ts") {
		t.Fatal("different session should not see prior session's AI paths")
	}
	// Recording under the new session resets the ledger to that session.
	recordAiTouchedPath("sess-2", "src/new.ts")
	if !wasAiTouchedPath("sess-2", "src/new.ts") {
		t.Fatal("new session's path should be recorded")
	}
	if wasAiTouchedPath("sess-2", "src/app.ts") {
		t.Fatal("old session's paths should be gone after session change")
	}
}

func TestDedupeFileDiff_RecordsAiTouchedPath(t *testing.T) {
	stateDirPath := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", stateDirPath)
	taskRoot := t.TempDir()

	abs := filepath.Join(taskRoot, "main.go")
	if err := os.WriteFile(abs, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// AI channel emits a diff for main.go — wins the claim and records the path.
	aiEvent := newEvent("file_diff", "sess-1")
	aiEvent.Provenance = aiProvenance()
	aiEvent.Data = map[string]interface{}{"path": "main.go", "diff": "+x"}
	if !dedupeFileDiff(taskRoot, &aiEvent) {
		t.Fatal("first AI claim should win")
	}
	if !wasAiTouchedPath("sess-1", "main.go") {
		t.Fatal("AI-attributed diff should record the path in the AI-paths ledger")
	}

	// The git watcher later sees a NEW human edit to the same file (different
	// content hash) — it should attribute it as ai_revised_by_human.
	if err := os.WriteFile(abs, []byte("package main\n// edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	humanEvent := newEvent("file_diff", "sess-1")
	humanEvent.Provenance = gitPollProvenance(wasAiTouchedPath("sess-1", "main.go"))
	humanEvent.Data = map[string]interface{}{"path": "main.go", "diff": "+y"}
	if !dedupeFileDiff(taskRoot, &humanEvent) {
		t.Fatal("new content hash should win its own claim")
	}
	if humanEvent.Provenance.Attribution != "ai_revised_by_human" {
		t.Fatalf("attribution = %q, want ai_revised_by_human", humanEvent.Provenance.Attribution)
	}
	// Human-attributed claims must NOT pollute the AI-paths ledger.
	if wasAiTouchedPath("sess-1", "other.go") {
		t.Fatal("sanity: unrelated path untouched")
	}
}
