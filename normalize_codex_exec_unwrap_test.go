package main

import (
	"strings"
	"testing"
)

// Codex >= 0.149 stopped exposing named tools. Everything now arrives as ONE
// generic custom tool called `exec`, whose input is a small JavaScript program
// that calls the real tool:
//
//	const r = await tools.exec_command({cmd:"go test ./...", workdir:"/repo"})
//
// `isCodexShellTool` never matched "exec", so every shell run, test run and file
// edit fell through to the generic tool_use branch. Measured on the hiring DB
// across 30 days: tool_use=23 (toolName=exec x23), command=0, file_diff=1. The
// fluency judge then reported "no verification, no tool use, no durable
// artifacts" about sessions that had run dozens of commands — an accusation
// about the candidate produced by a parser gap.
//
// These lines mirror a real codex 0.149.1 rollout.

const (
	codexExecShellCall = `{"timestamp":"2026-08-26T13:25:51.9Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_1","input":"const r = await tools.exec_command({cmd:\"go test ./...\",workdir:\"/repo\"});\nconsole.log(r);"}}`

	codexExecShellOutput = `{"timestamp":"2026-08-26T13:25:53.1Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"Wall time: 2.1s\nProcess exited with code 1\nOutput:\nFAIL github.com/x/y"}]}}`

	codexExecPatchCall = `{"timestamp":"2026-08-26T13:26:10.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_2","input":"const patch = \"*** Begin Patch\n*** Update File: internal/auth.go\n@@ func Login\n ctx := r.Context()\n-\tif user == nil {\n+\tif user == nil || user.Disabled {\n \t\treturn ErrDenied\n*** End Patch\";\nawait tools.apply_patch(patch);"}}`

	codexExecPatchOutput = `{"timestamp":"2026-08-26T13:26:10.5Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Done"}}`

	// A detached run: the host answers with a cell ID instead of a result, and
	// the real outcome arrives on a later write_stdin.
	codexExecDetachedCall = `{"timestamp":"2026-08-26T13:27:00.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_3","input":"const r = await tools.exec_command({cmd:\"pnpm build\"});"}}`

	codexExecDetachedHandoff = `{"timestamp":"2026-08-26T13:27:00.2Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_3","output":"Script running with cell ID 7"}}`

	codexExecStdinCall = `{"timestamp":"2026-08-26T13:27:40.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_4","input":"await tools.write_stdin({session_id:7, chars:\"\"});"}}`

	codexExecStdinOutput = `{"timestamp":"2026-08-26T13:27:41.0Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_4","output":"Process exited with code 2\nOutput:\nbuild failed"}}`

	// A program calling something we deliberately do not lift.
	codexExecUnknownCall = `{"timestamp":"2026-08-26T13:28:00.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_5","input":"const secret = \"hunter2\"; await tools.some_future_thing({a:1});"}}`

	codexExecUnknownOutput = `{"timestamp":"2026-08-26T13:28:01.0Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_5","output":"ok"}}`
)

// feed runs a sequence of rollout lines and returns everything emitted.
func feed(p *codexRolloutProcessor, lines ...string) []Event {
	var out []Event
	for _, l := range lines {
		out = append(out, p.process([]byte(l))...)
	}
	return out
}

func TestCodexExecWrapperBecomesACommand(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecShellCall, codexExecShellOutput)

	if len(events) != 1 || events[0].Kind != "command" {
		t.Fatalf("expected one command event, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if got, _ := d["command"].(string); got != "go test ./..." {
		t.Errorf("command not recovered from the JS wrapper: %q", got)
	}
	if got, _ := d["exitCode"].(int); got != 1 {
		t.Errorf("exitCode = %v, want 1 (parsed from the content-array output)", d["exitCode"])
	}
}

func TestCodexExecWrapperNeverLeaksWrapperSource(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecUnknownCall, codexExecUnknownOutput)

	if len(events) != 1 || events[0].Kind != "tool_use" {
		t.Fatalf("an unrecognised program must stay a generic tool_use, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if name, _ := d["toolName"].(string); name != "exec" {
		t.Errorf("toolName = %q, want exec", name)
	}
	// The wrapper is JavaScript the model wrote; it can carry anything. It must
	// never ride along on the event.
	for k, v := range d {
		if s, ok := v.(string); ok && strings.Contains(s, "hunter2") {
			t.Fatalf("wrapper source leaked into data[%q]: %q", k, s)
		}
	}
	if strings.Contains(events[0].RawPayload, "hunter2") {
		t.Fatal("wrapper source leaked into RawPayload")
	}
}

func TestCodexWrappedApplyPatchBecomesFileDiffWithContent(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecPatchCall, codexExecPatchOutput)

	if len(events) != 1 || events[0].Kind != "file_diff" {
		t.Fatalf("expected one file_diff, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if got, _ := d["path"].(string); got != "internal/auth.go" {
		t.Errorf("path = %q", got)
	}
	if got, _ := d["changeType"].(string); got != "update" {
		t.Errorf("changeType = %q, want update", got)
	}
	// Hiring keeps the body — the replay renders it. This is the one place the
	// teams normalizer deliberately differs (it is source-excluded).
	diff, _ := d["diff"].(string)
	if !strings.Contains(diff, "+\tif user == nil || user.Disabled {") {
		t.Errorf("diff body not carried through: %q", diff)
	}
	if strings.Contains(diff, "*** Begin Patch") || strings.Contains(diff, "*** Update File") {
		t.Errorf("envelope framing leaked into the diff: %q", diff)
	}
	if got, _ := d["linesAdded"].(int); got != 1 {
		t.Errorf("linesAdded = %v, want 1", d["linesAdded"])
	}
	if got, _ := d["linesRemoved"].(int); got != 1 {
		t.Errorf("linesRemoved = %v, want 1", d["linesRemoved"])
	}
}

func TestCodexDetachedExecReportsTheRealExitCode(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)

	held := feed(p, codexExecDetachedCall, codexExecDetachedHandoff)
	if len(held) != 0 {
		t.Fatalf("the cell-ID handoff is not a result and must emit nothing, got %v", kindsOf(held))
	}

	done := feed(p, codexExecStdinCall, codexExecStdinOutput)
	if len(done) != 1 || done[0].Kind != "command" {
		t.Fatalf("expected the held command to complete, got %v", kindsOf(done))
	}
	d := eventData(t, done[0])
	if got, _ := d["command"].(string); got != "pnpm build" {
		t.Errorf("the ORIGINAL command must be reported, not the write_stdin: %q", got)
	}
	// Without the running-call handoff, "Script running with cell ID 7" parses as
	// exitCode 0 and a failed build is reported green.
	if got, _ := d["exitCode"].(int); got != 2 {
		t.Errorf("exitCode = %v, want 2", d["exitCode"])
	}
}

func TestCodexDirectToolCallsAreUnchanged(t *testing.T) {
	// Pre-0.149 codex-cli calls the tools by name. That path must not regress.
	call := `{"timestamp":"2026-08-26T13:29:00.0Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c9","arguments":"{\"command\":[\"bash\",\"-lc\",\"ls -la\"]}"}}`
	out := `{"timestamp":"2026-08-26T13:29:01.0Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c9","output":"Process exited with code 0\nOutput:\ntotal 0"}}`

	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, call, out)

	if len(events) != 1 || events[0].Kind != "command" {
		t.Fatalf("expected one command event, got %v", kindsOf(events))
	}
	if got, _ := eventData(t, events[0])["command"].(string); got != "bash -lc ls -la" {
		t.Errorf("command = %q", got)
	}
}

// --- 0.149 status vocabulary ------------------------------------------------
//
// Found by running a real 430-line rollout through the processor: 43 outputs
// said "Script completed" and 2 said "Script failed", and the exit-code regex
// matched NEITHER, so all 45 reported exitCode 0. A fake green is worse than a
// blank — cleanFirstPass and the red/green arc count are built on it.

func TestCodexScriptFailedIsNotExitZero(t *testing.T) {
	code, _ := parseCodexExecOutput("Script failed\nWall time 1.5 seconds\nOutput:\n\nScript error:\nboom")
	if code == 0 {
		t.Fatal("a failed script must not report exitCode 0")
	}
}

func TestCodexScriptCompletedIsExitZero(t *testing.T) {
	code, stdout := parseCodexExecOutput("Script completed\nWall time 0.6 seconds\nOutput:\nall good")
	if code != 0 {
		t.Errorf("exitCode = %d, want 0", code)
	}
	if !strings.Contains(stdout, "all good") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestCodexNumericExitCodeStillWins(t *testing.T) {
	// Pre-0.149 hosts still emit the numeric form; it must keep precedence.
	code, _ := parseCodexExecOutput("Wall time: 1s\nProcess exited with code 3\nOutput:\nx")
	if code != 3 {
		t.Errorf("exitCode = %d, want 3", code)
	}
}

func TestCodexScriptFailedInStdoutIsNotTheVerdict(t *testing.T) {
	// The command's own output may print the phrase. Only the leading status
	// line is this command's verdict.
	code, _ := parseCodexExecOutput("Script completed\nWall time 0.1 seconds\nOutput:\nlog: Script failed earlier today")
	if code != 0 {
		t.Errorf("exitCode = %d, want 0 — the phrase was in stdout, not the status line", code)
	}
}

// --- rejected patches -------------------------------------------------------

func TestCodexRejectedPatchEmitsNoFileDiff(t *testing.T) {
	// Both failures in the sampled rollout were rejected apply_patch calls, each
	// followed by a retry. Emitting from the envelope alone invents file_diffs
	// for edits that never touched the tree and double-counts the retry.
	failed := `{"timestamp":"2026-08-26T13:26:10.5Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Script failed\nWall time 0.0 seconds\nOutput:\n\nScript error:\napply_patch verification failed: invalid patch"}}`

	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecPatchCall, failed)

	for _, e := range events {
		if e.Kind == "file_diff" {
			t.Fatalf("a rejected patch must not produce a file_diff: %+v", e.Data)
		}
	}
	if len(events) != 1 || events[0].Kind != "tool_use" {
		t.Fatalf("expected one tool_use recording the failed attempt, got %v", kindsOf(events))
	}
	if ok, _ := eventData(t, events[0])["ok"].(bool); ok {
		t.Error("the failed apply_patch must be recorded as ok=false")
	}
}

// --- FileChange is the authoritative record --------------------------------
//
// The apply_patch ENVELOPE is the model's request; the FileChange item is what
// landed. Only the latter can describe a deletion: the envelope spells one
// "*** Delete File: path" with no body, so the file_diff carried diff="" and
// linesRemoved=0 and the replay rendered "No diff content available" for a file
// the candidate deliberately removed.

const codexFileChangeDelete = `{"timestamp":"2026-08-26T13:30:00.0Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"exec-1","changes":{"src/old.py":{"type":"delete","content":"import os\nimport sys\nprint(1)\n"}}}}}`

const codexFileChangeUpdate = `{"timestamp":"2026-08-26T13:30:05.0Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"exec-2","changes":{"src/keep.py":{"type":"update","unified_diff":"@@ -1,2 +1,2 @@\n-old\n+new\n","move_path":null}}}}}`

func TestCodexDeletedFileReportsItsLines(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexFileChangeDelete)

	if len(events) != 1 || events[0].Kind != "file_diff" {
		t.Fatalf("expected one file_diff, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if got, _ := d["changeType"].(string); got != "delete" {
		t.Errorf("changeType = %q, want delete", got)
	}
	if got, _ := d["linesRemoved"].(int); got != 3 {
		t.Errorf("linesRemoved = %v, want 3 — a deletion is not zero work", d["linesRemoved"])
	}
	// The replay renders "No diff content available." on an empty body, which is
	// exactly the wrong thing to show for a deliberate deletion.
	diff, _ := d["diff"].(string)
	if diff == "" {
		t.Fatal("a deleted file must still carry a renderable diff body")
	}
	if !strings.Contains(diff, "-import os") {
		t.Errorf("deleted content must render as removals: %q", diff)
	}
}

func TestCodexUpdateUsesTheHostsUnifiedDiff(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexFileChangeUpdate)

	if len(events) != 1 {
		t.Fatalf("expected one file_diff, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	diff, _ := d["diff"].(string)
	if !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
		t.Errorf("the host's unified_diff must be used verbatim: %q", diff)
	}
	if got, _ := d["linesAdded"].(int); got != 1 {
		t.Errorf("linesAdded = %v, want 1", d["linesAdded"])
	}
}

func TestCodexFileChangeSuppressesTheEnvelopeFallback(t *testing.T) {
	// CALL -> FILECHANGE -> OUTPUT is the real ordering. When the authoritative
	// record already fired, re-deriving from the envelope would double every
	// file edit AND report the worse version of it.
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecPatchCall, codexFileChangeUpdate, codexExecPatchOutput)

	diffs := 0
	for _, e := range events {
		if e.Kind == "file_diff" {
			diffs++
		}
	}
	if diffs != 1 {
		t.Fatalf("expected exactly 1 file_diff (FileChange only), got %d: %v", diffs, kindsOf(events))
	}
}

func TestCodexEnvelopeStillCoversAHostWithoutFileChange(t *testing.T) {
	// If no FileChange arrives, the envelope must still produce the edit —
	// otherwise a host that does not emit FileChange silently captures nothing,
	// which is the original bug.
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecPatchCall, codexExecPatchOutput)

	if len(events) != 1 || events[0].Kind != "file_diff" {
		t.Fatalf("expected the envelope fallback to emit one file_diff, got %v", kindsOf(events))
	}
}

// A rename inside a wrapped envelope names its destination on a `*** Move to:`
// line of its own. Dropping that line left the event keyed to a path the patch
// had just removed: replay pointed at the wrong file, and cross-channel dedup
// could never match the Git event for the destination. movePath is the shape
// the FileChange and patch_apply_end producers already use for a rename.
func TestCodexWrappedApplyPatchCarriesMoveDestination(t *testing.T) {
	const call = `{"timestamp":"2026-08-26T13:30:00.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_9","input":"const patch = \"*** Begin Patch\n*** Update File: internal/auth.go\n*** Move to: internal/session/auth.go\n@@ func Login\n-\tif user == nil {\n+\tif user == nil || user.Disabled {\n*** End Patch\";\nawait tools.apply_patch(patch);"}}`
	const out = `{"timestamp":"2026-08-26T13:30:00.5Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_9","output":"Done"}}`

	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, call, out)

	if len(events) != 1 || events[0].Kind != "file_diff" {
		t.Fatalf("expected one file_diff, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if got, _ := d["path"].(string); got != "internal/auth.go" {
		t.Errorf("path = %q, want the source path", got)
	}
	if got, _ := d["movePath"].(string); got != "internal/session/auth.go" {
		t.Errorf("movePath = %q, want the destination — the rename was dropped", got)
	}
	// The framing line is a destination, not diff content.
	if diff, _ := d["diff"].(string); strings.Contains(diff, "Move to") {
		t.Errorf("the Move to line leaked into the diff body: %q", diff)
	}
}

// A patch with no rename must not grow a movePath key — the sibling producers
// only set it when the host reported one.
func TestCodexWrappedApplyPatchOmitsMovePathWhenThereIsNoRename(t *testing.T) {
	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, codexExecPatchCall, codexExecPatchOutput)

	if len(events) != 1 {
		t.Fatalf("expected one event, got %v", kindsOf(events))
	}
	if _, present := eventData(t, events[0])["movePath"]; present {
		t.Error("movePath set on a patch that renames nothing")
	}
}

// A rejected patch is a REQUEST, not a record of what landed. The script
// envelope alone cannot decide that: codexScriptFailed is anchored to the head
// of the blob, so a wrapper that catches the refusal and reports "Script
// completed" would have its envelope emitted as real file_diffs — inventing AI
// edits and marking unchanged paths as AI-touched.
func TestCodexWrappedApplyPatchNeverInventsDiffsFromASwallowedRejection(t *testing.T) {
	const call = `{"timestamp":"2026-08-26T13:31:00.0Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_10","input":"const patch = \"*** Begin Patch\n*** Update File: internal/auth.go\n@@ func Login\n-\told\n+\tnew\n*** End Patch\";\ntry { await tools.apply_patch(patch); } catch (e) { console.log(e.message); }"}}`
	// The script itself SUCCEEDS — the wrapper caught the refusal and printed it.
	const out = `{"timestamp":"2026-08-26T13:31:00.5Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_10","output":"Script completed\nWall time 0.1 seconds\nOutput:\napply_patch verification failed: Failed to find expected lines in internal/auth.go"}}`

	p := newCodexRolloutProcessor("sess-1", false)
	events := feed(p, call, out)

	for _, e := range events {
		if e.Kind == "file_diff" {
			t.Fatalf("emitted a file_diff for a patch the host refused: %v", eventData(t, e))
		}
	}
	if len(events) != 1 || events[0].Kind != "tool_use" {
		t.Fatalf("a refused patch must still be recorded as a failed tool_use, got %v", kindsOf(events))
	}
	d := eventData(t, events[0])
	if ok, _ := d["ok"].(bool); ok {
		t.Error("the refused patch was recorded as ok")
	}
}
