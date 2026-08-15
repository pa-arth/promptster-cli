package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file all target the same class of defect, found by hand on
// 2026-08-15 after two days of batch 1: the envelope resolved by WHERE it was
// opened rather than by WHAT was assigned. Each one fails against the pre-fix
// code — that is the point of them. A guard that was never falsified against
// the behaviour it forbids is how PR #7 shipped a repo check that a flag could
// walk straight past.

func readEventKinds(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(eventsPath())
	if err != nil {
		return nil
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		kinds = append(kinds, e.ComplianceEvent)
	}
	return kinds
}

func hasEvent(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// TestTreatmentReachesASessionInAnotherCheckout is the defect that mattered
// most: the assignment is drawn in one checkout and the work happens in a
// worktree under a different repo, so keying the lookup on the session's cwd
// meant the assigned session got no C1 contract and could not arm C2's gate.
// Treatment silently stopped being delivered to the tasks it was assigned to,
// which reads afterwards as non-adherence rather than as a broken instrument.
func TestTreatmentReachesASessionInAnotherCheckout(t *testing.T) {
	withTempRoot(t)
	cfg, a, openedIn := seedTask(t, CellC1)

	elsewhere := t.TempDir() // a different repo entirely, as a worktree would be
	if elsewhere == openedIn {
		t.Fatal("test setup: the two roots must differ")
	}

	out := captureStdout(t, func() {
		if code := hookSessionStart(cfg, hookPayload{
			SessionID: "sess-elsewhere", CWD: elsewhere, Source: "startup",
		}); code != 0 {
			t.Fatalf("hook returned %d", code)
		}
	})

	if !strings.Contains(out, a.TaskKey) {
		t.Fatalf("C1 artifact did not reach a session in another checkout; stdout=%q", out)
	}
	if !hasEvent(readEventKinds(t), "artifact_shown") {
		t.Fatal("no artifact_shown event recorded for the out-of-checkout session")
	}
}

// TestGateArmsFromAnotherCheckout is the same defect on C2's enforcement path.
// A compaction in the working session must arm the gate for the open envelope
// no matter which directory that session sits in.
func TestGateArmsFromAnotherCheckout(t *testing.T) {
	withTempRoot(t)
	cfg, a, _ := seedTask(t, CellC2)

	if code := hookPreCompact(cfg, hookPayload{
		SessionID: "sess-far", CWD: t.TempDir(), Trigger: "auto",
	}); code != 0 {
		t.Fatalf("hook returned %d", code)
	}

	g, ok := readGate("sess-far")
	if !ok || !g.Armed {
		t.Fatal("compaction in another checkout did not arm the gate")
	}
	if g.TaskKey != a.TaskKey {
		t.Fatalf("gate armed for %q, want %q", g.TaskKey, a.TaskKey)
	}
	if !hasEvent(readEventKinds(t), "compaction") {
		t.Fatal("compaction not recorded")
	}
}

// TestActiveTaskIsOneGlobalSlot pins the property the fix rests on. If this
// ever becomes per-directory again, every test above can pass while the
// treatment is still routed by cwd.
func TestActiveTaskIsOneGlobalSlot(t *testing.T) {
	withTempRoot(t)
	if got := filepath.Base(activeTaskPath()); got != "active.json" {
		t.Fatalf("active task path is %q; the pointer must not be keyed by anything", got)
	}
	if err := writeActiveTask(ActiveTask{TaskKey: "o/one", RepoRoot: "/a", OpenedAt: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	got, ok := readActiveTask()
	if !ok || got.TaskKey != "o/one" {
		t.Fatalf("readActiveTask = %+v, %v", got, ok)
	}

	entries, err := os.ReadDir(tasksDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tasks dir holds %d files; exactly one envelope may exist", len(entries))
	}
}

// openIn runs `open` with cwd set to dir, returning stdout and the exit code.
func openIn(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	var code int
	out := captureStdout(t, func() {
		t.Chdir(dir)
		code = cmdOpen(args)
	})
	return out, code
}

// TestSecondOpenRefusesRatherThanOrphaning is the silent-orphan defect. Batch 1
// lost an envelope this way: opening the next task overwrote the pointer, and
// the orphaned task still reads "open" in the log with no close event, which at
// analysis time is indistinguishable from a task nobody ever worked.
func TestSecondOpenRefusesRatherThanOrphaning(t *testing.T) {
	withTempRoot(t)
	if err := saveConfig(testCfg()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if _, code := openIn(t, dir, "--task", "alpha/one", "--class", "feature", "--repo", "o/alpha"); code != 0 {
		t.Fatalf("first open returned %d", code)
	}

	_, code := openIn(t, dir, "--task", "beta/two", "--class", "feature", "--repo", "o/beta")
	if code == 0 {
		t.Fatal("second open succeeded; it must refuse rather than orphan the first")
	}
	active, ok := readActiveTask()
	if !ok || active.TaskKey != "alpha/one" {
		t.Fatalf("refused open still moved the pointer: %+v", active)
	}

	// The explicit handover is allowed, and leaves a record.
	if _, code := openIn(t, dir, "--task", "beta/two", "--class", "feature", "--repo", "o/beta", "--supersede"); code != 0 {
		t.Fatalf("--supersede open returned %d", code)
	}
	if !hasEvent(readEventKinds(t), "task_superseded") {
		t.Fatal("supersede did not record task_superseded; an orphan must never be silent")
	}
	if active, _ := readActiveTask(); active.TaskKey != "beta/two" {
		t.Fatalf("supersede did not move the pointer: %+v", active)
	}
}

// TestReopeningTheOpenTaskIsNotASupersede guards the obvious false positive:
// re-running open on the task that is already open is a no-op, not a handover.
func TestReopeningTheOpenTaskIsNotASupersede(t *testing.T) {
	withTempRoot(t)
	if err := saveConfig(testCfg()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if _, code := openIn(t, dir, "--task", "alpha/one", "--class", "feature", "--repo", "o/alpha"); code != 0 {
		t.Fatalf("first open returned %d", code)
	}
	if _, code := openIn(t, dir, "--task", "alpha/one", "--class", "feature", "--repo", "o/alpha"); code != 0 {
		t.Fatal("re-opening the open task must not refuse")
	}
	if hasEvent(readEventKinds(t), "task_superseded") {
		t.Fatal("re-open recorded a supersede")
	}
	kinds := readEventKinds(t)
	if !hasEvent(kinds, "task_reopen") {
		t.Fatalf("expected task_reopen, got %v", kinds)
	}
}

// TestClosingAnOrphanDoesNotEvictTheOpenEnvelope is the same bug from the other
// end: closing task X by --task while Y is open must not retire Y's pointer.
func TestClosingAnOrphanDoesNotEvictTheOpenEnvelope(t *testing.T) {
	withTempRoot(t)
	if err := saveConfig(testCfg()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if _, code := openIn(t, dir, "--task", "alpha/one", "--class", "feature", "--repo", "o/alpha"); code != 0 {
		t.Fatalf("open alpha returned %d", code)
	}
	if _, code := openIn(t, dir, "--task", "beta/two", "--class", "feature", "--repo", "o/beta", "--supersede"); code != 0 {
		t.Fatalf("open beta returned %d", code)
	}

	// alpha/one is now an orphan. Closing it must leave beta/two open.
	captureStdout(t, func() {
		t.Chdir(dir)
		if code := cmdClose([]string{"--task", "alpha/one", "--outcome", "merged"}); code != 0 {
			t.Fatalf("close returned %d", code)
		}
	})

	active, ok := readActiveTask()
	if !ok {
		t.Fatal("closing the orphan retired the open envelope")
	}
	if active.TaskKey != "beta/two" {
		t.Fatalf("active task is %q, want beta/two", active.TaskKey)
	}
}

// TestDeclaredRepoDivergenceIsRecorded covers the third defect. --repo sets the
// stratum, and the stratum decides which permuted block the arm comes from, so
// a flag that can disagree with the checkout without leaving a trace makes the
// draw unauditable. All seven of batch 1's rows carried a repoRoot that
// contradicted their stratum and nothing in the row said so.
func TestDeclaredRepoDivergenceIsRecorded(t *testing.T) {
	withTempRoot(t)
	if err := saveConfig(testCfg()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	detected := repoSlugOf(dir)
	if detected == "" {
		t.Skip("no detectable slug for the temp dir")
	}

	out, code := openIn(t, dir, "--task", "promptster-backend/some-work", "--class", "fix", "--repo", "pa-arth/promptster-backend")
	if code != 0 {
		t.Fatalf("open returned %d: %s", code, out)
	}
	if !strings.Contains(out, "opened from") {
		t.Fatalf("divergence not surfaced to the engineer; stdout=%q", out)
	}

	rows, err := readAssignments()
	if err != nil || len(rows) != 1 {
		t.Fatalf("readAssignments = %d rows, %v", len(rows), err)
	}
	if rows[0].Envelope.OpenedInRepo != detected {
		t.Fatalf("envelope.openedInRepo = %q, want the detected %q",
			rows[0].Envelope.OpenedInRepo, detected)
	}
	if rows[0].Repo != "pa-arth/promptster-backend" {
		t.Fatalf("stratum repo = %q; --repo must still decide the stratum", rows[0].Repo)
	}
}
