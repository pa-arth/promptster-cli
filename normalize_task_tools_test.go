package main

import "testing"

// Claude Code renamed the todo tool to TaskCreate/TaskUpdate/TaskList, which
// silently killed the "planning" event kind: the normalizer only matched
// TodoWrite, so the backend recorded zero planning rows while the agent kept
// planning as much as ever.
//
// Every payload below is copied from a REAL Claude Code transcript under
// ~/.claude/projects — including the tool_response shape, which is where the
// first cut of this got it wrong. The hook delivers tool_response as a
// STRUCTURED map ({"task":{"id":"1","subject":"..."}}); the prose "Task #1
// created successfully: ..." lives only in the MODEL-facing tool_result block
// and never reaches the normalizer. A fixture built from that prose passes
// while the real thing silently returns nothing.
//
// The guessed INPUT shapes (`todos`/`tasks` arrays) likewise come back from the
// real tool as InputValidationError. Both traps are the same mistake: asserting
// a schema you assumed instead of one you read.

func TestNormalizeClaudeCode_TaskCreateIsPlanning(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input": map[string]interface{}{
			"subject":     "Scaffold @promptster/config-cost package",
			"description": "Create packages/config-cost (package.json deps zod+js-tiktoken, tsconfig extends base, vitest config) following packages/env conventions.",
			"activeForm":  "Scaffolding @promptster/config-cost package",
		},
		"tool_response": map[string]interface{}{
			"task": map[string]interface{}{"id": "1", "subject": "Scaffold @promptster/config-cost package"},
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
	if data["subject"] != "Scaffold @promptster/config-cost package" {
		t.Fatalf("subject = %#v", data["subject"])
	}
	if data["sessionTaskOrdinal"] != 1 {
		t.Fatalf("sessionTaskOrdinal = %#v, want 1", data["sessionTaskOrdinal"])
	}
}

// The only task number available lives in TaskCreate's STRUCTURED response,
// {"task": {"id": "6", ...}}, since the tool creates one task per call and never
// sends a list. Note `id` is a STRING — a float64-only decode silently yields 0.
// If this stops resolving, decision capture goes quiet again.
// NOTE: 6 here is the session's cumulative ordinal, NOT "a 6-item plan".
func TestNormalizeClaudeCode_TaskCreateOrdinalFromResponse(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input": map[string]interface{}{
			"subject": "Public endpoint + seed playground org + cleanup",
		},
		"tool_response": map[string]interface{}{
			"task": map[string]interface{}{"id": "6", "subject": "Public endpoint + seed playground org + cleanup"},
		},
	}

	e, _ := normalizeClaudeCode(payload, "sess-1")
	data, _ := e.Data.(map[string]interface{})
	if data["sessionTaskOrdinal"] != 6 {
		t.Fatalf("sessionTaskOrdinal = %#v, want 6", data["sessionTaskOrdinal"])
	}
}

// An unparseable response must omit the ordinal rather than invent one — a
// wrong count is worse downstream than a missing one.
func TestNormalizeClaudeCode_TaskCreateOmitsOrdinalWhenUnparseable(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input":      map[string]interface{}{"subject": "Do the thing"},
		"tool_response":   map[string]interface{}{"error": "something unexpected"},
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	if e.Kind != "planning" {
		t.Fatalf("kind = %q, want planning", e.Kind)
	}
	data, _ := e.Data.(map[string]interface{})
	if _, present := data["sessionTaskOrdinal"]; present {
		t.Fatalf("sessionTaskOrdinal should be absent, got %#v", data["sessionTaskOrdinal"])
	}
}

// A FAILED TaskCreate carries no `task` object, so no ordinal — even when its
// error text quotes an earlier success line. This guards the original regex
// approach from coming back: an unanchored "Task #7 created" match anywhere in
// the response would record a stale ordinal for a task that never existed.
func TestNormalizeClaudeCode_TaskCreateIgnoresQuotedNumberInErrorText(t *testing.T) {
	payload := map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"tool_name":       "TaskCreate",
		"tool_input":      map[string]interface{}{"subject": "Do the thing"},
		"tool_response":   map[string]interface{}{"error": "InputValidationError: subject conflicts with Task #7 created successfully earlier in this session"},
	}

	e, ok := normalizeClaudeCode(payload, "sess-1")
	if !ok {
		t.Fatal("normalizeClaudeCode returned false")
	}
	data, _ := e.Data.(map[string]interface{})
	if v, present := data["sessionTaskOrdinal"]; present {
		t.Fatalf("a failed TaskCreate must not harvest a quoted ordinal, got %#v", v)
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
	// A status flip executes a plan, it does not define one. An ordinal here
	// would let one update masquerade as an N-step plan.
	if _, present := data["sessionTaskOrdinal"]; present {
		t.Fatalf("TaskUpdate must not carry sessionTaskOrdinal, got %#v", data["sessionTaskOrdinal"])
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
// in-process, so the ordinal arrives as a Go int, not a decoded float64 —
// resolving only float64 here would silently gate every candidate out.
//
// The copy must NOT claim a plan size off a session ordinal: 5 here means "the
// 5th task created this session", which is not "a 5-item plan".
func TestDetectDecisionCandidateForTaskCreatePlan(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName":           "TaskCreate",
		"subject":            "Tests: rubric-aware skip (worker) + drift sweep selection (api)",
		"sessionTaskOrdinal": 5,
	}

	candidate := detectDecisionCandidate(event)
	if candidate == nil {
		t.Fatal("expected a candidate once the session has tracked 5 tasks")
	}
	if candidate.CategoryHint != "planning" {
		t.Fatalf("category = %q", candidate.CategoryHint)
	}
	if candidate.ChosenOption != "Execute a multi-step implementation plan" {
		t.Fatalf("chosenOption = %q", candidate.ChosenOption)
	}
	if want := "The agent had tracked 5 tasks in this session before making code changes."; candidate.Context != want {
		t.Fatalf("context = %q, want %q", candidate.Context, want)
	}
}

// A TodoWrite list IS a real plan size, so that copy may name it.
func TestDetectDecisionCandidateForTodoWriteNamesRealPlanSize(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName":  "TodoWrite",
		"todos":     []interface{}{1, 2, 3, 4, 5},
		"itemCount": 5,
	}

	candidate := detectDecisionCandidate(event)
	if candidate == nil {
		t.Fatal("expected a candidate for a 5-item TodoWrite plan")
	}
	if candidate.ChosenOption != "Execute a 5-step implementation plan" {
		t.Fatalf("chosenOption = %q", candidate.ChosenOption)
	}
	if want := "The agent tracked a 5-item plan before making code changes."; candidate.Context != want {
		t.Fatalf("context = %q, want %q", candidate.Context, want)
	}
}

// Same map after a JSON round-trip: the ordinal comes back as float64 and must
// still resolve.
func TestDetectDecisionCandidateForPlanOrdinalAsFloat(t *testing.T) {
	event := newEvent("planning", "sess-1")
	event.Data = map[string]interface{}{
		"toolName":           "TaskCreate",
		"sessionTaskOrdinal": float64(4),
	}

	if candidate := detectDecisionCandidate(event); candidate == nil {
		t.Fatal("expected a candidate when sessionTaskOrdinal decodes as float64")
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
