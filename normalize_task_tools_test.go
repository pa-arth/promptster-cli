package main

import "testing"

// Claude Code renamed the todo tool to TaskCreate/TaskUpdate/TaskList, which
// silently killed the "planning" event kind: the normalizer only matched
// TodoWrite, so the backend recorded zero planning rows while the agent kept
// planning as much as ever.
//
// Every payload below is copied from a REAL Claude Code transcript under
// ~/.claude/projects (a TaskCreate/TaskUpdate pair from one session, plus that
// session's verbatim tool_response strings). They are not guesses at the
// schema — the guessed shapes (`todos`/`tasks` arrays) all come back from the
// real tool as InputValidationError, which is precisely the trap these tests
// exist to prevent someone re-introducing.

func TestNormalizeClaudeCode_TaskCreateIsPlanning(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input": map[string]interface{}{
			"subject":     "Scaffold @promptster/config-cost package",
			"description": "Create packages/config-cost (package.json deps zod+js-tiktoken, tsconfig extends base, vitest config) following packages/env conventions.",
			"activeForm":  "Scaffolding @promptster/config-cost package",
		},
		"tool_response": "Task #1 created successfully: Scaffold @promptster/config-cost package",
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["subject"] != "Scaffold @promptster/config-cost package" {
		t.Fatalf("subject = %#v", data["subject"])
	}
	if data["itemCount"] != 1 {
		t.Fatalf("itemCount = %#v, want 1", data["itemCount"])
	}
}

// The running plan size lives only in TaskCreate's response ("Task #N created
// successfully"), since the tool creates one task per call and never sends a
// list. If this stops resolving, decision capture goes quiet again.
func TestNormalizeClaudeCode_TaskCreateItemCountFromResponse(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input": map[string]interface{}{
			"subject": "Public endpoint + seed playground org + cleanup",
		},
		"tool_response": "Task #6 created successfully: Public endpoint + seed playground org + cleanup",
	}

	e, _ := normalizeClaudeCode(payload, "sess-1")
	data, _ := e.Data.(map[string]interface{})
	if data["itemCount"] != 6 {
		t.Fatalf("itemCount = %#v, want 6", data["itemCount"])
	}
}

// An unparseable response must omit itemCount rather than invent one — a wrong
// plan size is worse downstream than a missing one.
func TestNormalizeClaudeCode_TaskCreateOmitsItemCountWhenUnparseable(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input":      map[string]interface{}{"subject": "Do the thing"},
		"tool_response":   "something unexpected",
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if _, present := data["itemCount"]; present {
		t.Fatalf("itemCount should be absent, got %#v", data["itemCount"])
	}
}

func TestNormalizeClaudeCode_TaskUpdateIsPlanning(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskUpdate",
		"tool_input": map[string]interface{}{
			"taskId": "1",
			"status": "in_progress",
		},
		"tool_response": "Updated task #1 status",
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if data["taskId"] != "1" || data["status"] != "in_progress" {
		t.Fatalf("data = %#v", data)
	}
	// A status flip executes a plan, it does not define one; carrying an
	// itemCount here would let one update masquerade as an N-step plan.
	if _, present := data["itemCount"]; present {
		t.Fatalf("TaskUpdate must not carry itemCount, got %#v", data["itemCount"])
	}
}

// TaskList is a READ. Routing it to "planning" would inflate planning volume
// with pure observation.
func TestNormalizeClaudeCode_TaskListIsPlanningRead(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskList",
		"tool_input":      map[string]interface{}{},
		"tool_response":   "No tasks found",
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning_read" {
		t.Fatalf("kind = %q, want planning_read", e.Kind)
	}
}

// Back-compat: older Claude Code clients still send TodoWrite, and dropping it
// would blind us to every session that has not upgraded.
func TestNormalizeClaudeCode_TodoWriteStillPlanning(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TodoWrite",
		"tool_input": map[string]interface{}{
			"todos": []interface{}{
				map[string]interface{}{"content": "Fix bug", "status": "in_progress"},
				map[string]interface{}{"content": "Add test", "status": "pending"},
			},
		},
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	todos, _ := data["todos"].([]interface{})
	if len(todos) != 2 {
		t.Fatalf("todos = %#v", data["todos"])
	}
	if data["itemCount"] != 2 {
		t.Fatalf("itemCount = %#v, want 2", data["itemCount"])
	}
}

func TestNormalizeClaudeCode_TodoReadStillPlanningRead(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TodoRead",
		"tool_input":      map[string]interface{}{},
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning_read" {
		t.Fatalf("kind = %q, want planning_read", e.Kind)
	}
}

// The decision-capture end of the wire. The normalizer hands this map over
// in-process, so itemCount arrives as a Go int, not a decoded float64 —
// resolving only float64 here would silently gate every candidate out.
func TestDetectDecisionCandidateForTaskCreatePlan(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName":  "TaskCreate",
		"subject":   "Tests: rubric-aware skip (worker) + drift sweep selection (api)",
		"itemCount": 5,
	}

	candidate := detectDecisionCandidate(event)
	if candidate == nil {
		t.Fatal("expected a candidate for a 5-item plan")
	}
	if candidate.CategoryHint != "planning" {
		t.Fatalf("category = %q", candidate.CategoryHint)
	}
	if candidate.ChosenOption != "Execute a 5-step implementation plan" {
		t.Fatalf("chosenOption = %q", candidate.ChosenOption)
	}
	// The copy must not name a tool that no longer exists.
	if want := "The agent tracked a 5-item plan before making code changes."; candidate.Context != want {
		t.Fatalf("context = %q, want %q", candidate.Context, want)
	}
}

// Same map after a JSON round-trip: itemCount comes back as float64 and must
// still resolve.
func TestDetectDecisionCandidateForPlanItemCountAsFloat(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName":  "TaskCreate",
		"itemCount": float64(4),
	}

	if candidate := detectDecisionCandidate(event); candidate == nil {
		t.Fatal("expected a candidate when itemCount decodes as float64")
	}
}

// A lone TaskUpdate carries no plan size and must not produce a decision.
func TestDetectDecisionCandidateSkipsTaskUpdate(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName": "TaskUpdate",
		"taskId":   "1",
		"status":   "completed",
	}

	if candidate := detectDecisionCandidate(event); candidate != nil {
		t.Fatalf("expected no candidate for a status flip, got %#v", candidate)
	}
}
