package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withTempRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_EXPERIMENT_DIR", dir)
	return dir
}

func TestCheckReanchorRequiresLengthAndAllThreeFields(t *testing.T) {
	long := strings.Repeat("x", 250)

	cases := []struct {
		name    string
		prompt  string
		ok      bool
		missing []string
	}{
		{
			name:   "complete brief",
			prompt: "What: ship the assignment hook so batch 1 can start. " + long + "\nWhy: batch 1 is blocked on it.\nDone when: an assignment row exists and the banner prints.",
			ok:     true,
		},
		{
			name:    "long enough but no fields",
			prompt:  long,
			ok:      false,
			missing: []string{"what", "why", "done-when"},
		},
		{
			name:    "all fields but too short",
			prompt:  "What: x\nWhy: y\nDone when: z",
			ok:      false,
			missing: nil,
		},
		{
			name:   "done-when spelled with a hyphen",
			prompt: "What: a thing. Why: a reason. Done-when: a condition. " + long,
			ok:     true,
		},
		{
			name:    "missing why only",
			prompt:  "What: a thing. Done when: a condition. " + long,
			ok:      false,
			missing: []string{"why"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := checkReanchor(tc.prompt)
			if res.OK != tc.ok {
				t.Fatalf("OK = %v, want %v (chars=%d missing=%v)", res.OK, tc.ok, res.Chars, res.Missing)
			}
			if tc.missing != nil && strings.Join(res.Missing, ",") != strings.Join(tc.missing, ",") {
				t.Fatalf("missing = %v, want %v", res.Missing, tc.missing)
			}
		})
	}
}

func TestReanchorLengthCountsRunesNotBytes(t *testing.T) {
	// 200 multi-byte runes: >200 characters, well over 200 bytes either way —
	// the check must accept it on the rune count, and reject 199 runes.
	body := "What: — Why: — Done when: — " + strings.Repeat("é", 199)
	if res := checkReanchor(body); !res.OK {
		t.Fatalf("should pass on rune count: chars=%d missing=%v", res.Chars, res.Missing)
	}
	short := "What: a Why: b Done when: c " + strings.Repeat("é", 100)
	if res := checkReanchor(short); res.OK {
		t.Fatalf("should fail: chars=%d", res.Chars)
	}
}

func TestBypassIsRecognized(t *testing.T) {
	if !isBypass("  !noanchor keep going") {
		t.Fatal("prefix bypass not recognized")
	}
	if isBypass("this is not a !noanchor bypass") {
		t.Fatal("mid-string token should not bypass")
	}
}

// seedTask writes a config + one assignment + an active-task pointer whose repo
// root is a temp dir, so the hook path can be exercised without a git repo.
func seedTask(t *testing.T, arm string) (Config, Assignment, string) {
	t.Helper()
	cfg := testCfg()
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a := Assignment{
		SchemaVersion: schemaVersion, AssignmentID: "aid-1", OrgID: cfg.OrgID,
		EngineerID: cfg.EngineerID, TaskKey: "repo/t-100", ExperimentKey: cfg.ExperimentKey,
		WeekBlock: weekBlock(time.Now()), Arm: arm, Factors: factorsFor(arm),
		Repo: "o/r", TaskClass: "feature", SizeBand: "M", Stratum: "o/r|feature",
		Eligible: true, SpecVersion: TreatmentSpecVersion, AssignmentSource: "cli-offline",
		AssignedAt: nowUTC(), CliVersion: toolVersion, Envelope: Envelope{RepoRoot: root},
	}
	if err := appendJSONL(assignmentsPath(), a); err != nil {
		t.Fatal(err)
	}
	if err := writeActiveTask(ActiveTask{TaskKey: a.TaskKey, RepoRoot: root, OpenedAt: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	return cfg, a, root
}

func TestGateArmsOnCompactAndBlocksUntilBriefed(t *testing.T) {
	withTempRoot(t)
	cfg, a, _ := seedTask(t, CellC2)
	sid := "sess-1"

	// Nothing armed yet: a prompt passes untouched.
	if code := hookUserPromptSubmit(cfg, hookPayload{SessionID: sid, UserInput: "hi"}); code != 0 {
		t.Fatalf("unarmed gate returned %d", code)
	}
	if g, ok := readGate(sid); ok && g.Armed {
		t.Fatal("gate armed without a compaction")
	}

	armGate(cfg, sid, a, "test")
	g, ok := readGate(sid)
	if !ok || !g.Armed {
		t.Fatal("gate did not arm")
	}

	// A short prompt is refused and preserved.
	out := captureStdout(t, func() {
		hookUserPromptSubmit(cfg, hookPayload{SessionID: sid, UserInput: "carry on"})
	})
	var blocked hookOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &blocked); err != nil {
		t.Fatalf("block output is not JSON: %q", out)
	}
	if blocked.Decision != "block" {
		t.Fatalf("decision = %q, want block", blocked.Decision)
	}
	if !strings.Contains(blocked.Reason, "Done when:") {
		t.Fatal("block reason does not include the template")
	}
	if g, _ := readGate(sid); !g.Armed || g.Attempts != 1 {
		t.Fatalf("gate should stay armed with 1 attempt, got %+v", g)
	}
	files, _ := os.ReadDir(filepath.Join(rootDir(), "rejected"))
	if len(files) != 1 {
		t.Fatalf("rejected prompt not preserved: %d files", len(files))
	}

	// A real brief passes and disarms.
	brief := "What: land the C2 gate for the practice experiment. " + strings.Repeat("y", 200) +
		"\nWhy: batch 1 cannot start without it.\nDone when: the hook blocks and releases correctly."
	out = captureStdout(t, func() {
		hookUserPromptSubmit(cfg, hookPayload{SessionID: sid, UserInput: brief})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("accepted brief should emit nothing, got %q", out)
	}
	if g, _ := readGate(sid); g.Armed {
		t.Fatal("gate did not disarm after an accepted brief")
	}

	// Subsequent prompts flow freely.
	out = captureStdout(t, func() {
		hookUserPromptSubmit(cfg, hookPayload{SessionID: sid, UserInput: "ok next"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("disarmed gate still blocking: %q", out)
	}

	assertEventKinds(t, "gate_armed", "reanchor_rejected", "reanchor_accepted")
}

func TestBypassReleasesGateAndIsRecordedAsNonAdherence(t *testing.T) {
	withTempRoot(t)
	cfg, a, _ := seedTask(t, CellC2)
	armGate(cfg, "sess-2", a, "test")

	out := captureStdout(t, func() {
		hookUserPromptSubmit(cfg, hookPayload{SessionID: "sess-2", UserInput: "!noanchor just keep going"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("bypass should not block, got %q", out)
	}
	if g, _ := readGate("sess-2"); g.Armed {
		t.Fatal("bypass did not disarm the gate")
	}
	assertEventKinds(t, "gate_armed", "gate_bypassed")
}

func TestControlArmIsNeverGatedAndSeesNothing(t *testing.T) {
	withTempRoot(t)
	cfg, _, root := seedTask(t, CellControl)

	out := captureStdout(t, func() {
		hookSessionStart(cfg, hookPayload{SessionID: "sess-3", CWD: root, Source: "startup"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("control must see no artifact, got %q", out)
	}

	out = captureStdout(t, func() {
		hookSessionStart(cfg, hookPayload{SessionID: "sess-3", CWD: root, Source: "compact"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("control must see nothing post-compact, got %q", out)
	}
	if g, ok := readGate("sess-3"); ok && g.Armed {
		t.Fatal("control arm was gated")
	}
	out = captureStdout(t, func() {
		hookUserPromptSubmit(cfg, hookPayload{SessionID: "sess-3", UserInput: "x"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("control prompt was blocked: %q", out)
	}
}

func TestC1ContractIsShownAtSessionStart(t *testing.T) {
	withTempRoot(t)
	cfg, _, root := seedTask(t, CellC1)

	out := captureStdout(t, func() {
		hookSessionStart(cfg, hookPayload{SessionID: "sess-4", CWD: root, Source: "startup"})
	})
	var o hookOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &o); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	if !strings.Contains(o.AdditionalContext, "ships ONE artifact") {
		t.Fatalf("C1 contract missing from context: %q", o.AdditionalContext)
	}
	if o.HookSpecificOutput == nil || o.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatal("hookSpecificOutput block missing")
	}
	// C1 alone must not arm the C2 gate.
	hookSessionStart(cfg, hookPayload{SessionID: "sess-4", CWD: root, Source: "compact"})
	if g, ok := readGate("sess-4"); ok && g.Armed {
		t.Fatal("C1-only arm was gated by C2's mechanism")
	}
}

func TestPreCompactRecordsButDoesNotBlockCompaction(t *testing.T) {
	withTempRoot(t)
	cfg, _, root := seedTask(t, CellC1C2)

	code := hookPreCompact(cfg, hookPayload{SessionID: "sess-5", CWD: root, Trigger: "auto"})
	if code != 0 {
		t.Fatalf("PreCompact returned %d — exit 2 would block compaction, which C2 must not do", code)
	}
	if g, ok := readGate("sess-5"); !ok || !g.Armed {
		t.Fatal("PreCompact did not arm the C2 gate")
	}
	assertEventKinds(t, "compaction", "gate_armed")
}

func TestDisabledConfigMakesEveryHookSilent(t *testing.T) {
	withTempRoot(t)
	cfg, a, _ := seedTask(t, CellC2)
	armGate(cfg, "sess-6", a, "test")

	cfg.Enabled = false
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(hookPayload{SessionID: "sess-6", UserInput: "short"})
	out := captureStdoutWithStdin(t, string(payload), func() {
		runHook([]string{"user-prompt-submit"})
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("kill switch did not silence the gate: %q", out)
	}
}

func TestHookFailsOpenOnGarbageInput(t *testing.T) {
	withTempRoot(t)
	seedTask(t, CellC2)
	out := captureStdoutWithStdin(t, "not json at all", func() {
		if code := runHook([]string{"user-prompt-submit"}); code != 0 {
			t.Fatalf("garbage input returned %d, must fail open", code)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("garbage input produced output: %q", out)
	}
}

func TestAppendJSONLRejectsOversizeRecords(t *testing.T) {
	withTempRoot(t)
	big := Event{ComplianceEvent: "x", Detail: strings.Repeat("z", maxLineBytes)}
	if err := appendJSONL(eventsPath(), big); err == nil {
		t.Fatal("oversize record should be refused, not silently torn across appends")
	}
}

// --- helpers ---------------------------------------------------------------

func assertEventKinds(t *testing.T, want ...string) {
	t.Helper()
	data, err := os.ReadFile(eventsPath())
	if err != nil {
		t.Fatalf("no event log: %v", err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil {
			got = append(got, e.ComplianceEvent)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureStdoutWithStdin(t, "", fn)
}

func captureStdoutWithStdin(t *testing.T, stdin string, fn func()) string {
	t.Helper()
	origOut, origIn := os.Stdout, os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	if stdin != "" {
		ir, iw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { iw.WriteString(stdin); iw.Close() }()
		os.Stdin = ir
	}

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()
	w.Close()
	os.Stdout, os.Stdin = origOut, origIn
	return <-done
}
