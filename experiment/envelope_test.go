package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

	// A worktree of the task's declared repo ("o/r"), at a path the envelope
	// knows nothing about — exactly the dispatch pattern batch 1 ran on.
	elsewhere := filepath.Join(t.TempDir(), "r")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
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

	far := filepath.Join(t.TempDir(), "r") // another worktree of the same repo
	if err := os.MkdirAll(far, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := hookPreCompact(cfg, hookPayload{
		SessionID: "sess-far", CWD: far, Trigger: "auto",
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

// TestUnrelatedSessionIsNotAttributedToTheOpenTask is the mirror of the two
// tests above, and the reason the envelope resolves by MEMBERSHIP rather than
// simply being global. A session in an unrelated repo must get no artifact, no
// gate, and — most importantly — must not have its compaction recorded against
// somebody else's task. Over-attribution corrupts adherence exactly as
// thoroughly as under-delivery, and under C2 it would gate a stranger's prompt.
func TestUnrelatedSessionIsNotAttributedToTheOpenTask(t *testing.T) {
	withTempRoot(t)
	cfg, _, _ := seedTask(t, CellC1C2)

	stranger := filepath.Join(t.TempDir(), "some-other-repo")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := hookSessionStart(cfg, hookPayload{
			SessionID: "sess-stranger", CWD: stranger, Source: "startup",
		}); code != 0 {
			t.Fatalf("hook returned %d", code)
		}
	})
	if out != "" {
		t.Fatalf("unrelated session was served the treatment: %q", out)
	}

	if code := hookPreCompact(cfg, hookPayload{
		SessionID: "sess-stranger", CWD: stranger, Trigger: "auto",
	}); code != 0 {
		t.Fatalf("pre-compact returned %d", code)
	}
	if kinds := readEventKinds(t); hasEvent(kinds, "compaction") || hasEvent(kinds, "artifact_shown") {
		t.Fatalf("unrelated session's activity was attributed to the open task: %v", kinds)
	}
	if g, ok := readGate("sess-stranger"); ok && g.Armed {
		t.Fatal("unrelated session had C2's gate armed against it")
	}
}

// TestLegacyPointerIsAdoptedOnUpgrade covers the upgrade path. A pointer written
// by the pre-#9 binary lives under sha256(repoRoot)[:16].json; if the new binary
// ignored it, upgrading mid-task would make the open envelope vanish — hooks
// stop delivering and the next open sees a free slot and orphans it. The upgrade
// would reproduce the very bug it ships the fix for.
func TestLegacyPointerIsAdoptedOnUpgrade(t *testing.T) {
	withTempRoot(t)
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := ActiveTask{TaskKey: "o/legacy", RepoRoot: "/some/root", OpenedAt: "2026-08-14T00:00:00Z"}
	older := ActiveTask{TaskKey: "o/older", RepoRoot: "/other/root", OpenedAt: "2026-08-10T00:00:00Z"}
	for name, v := range map[string]ActiveTask{"aaaaaaaaaaaaaaaa.json": older, "bbbbbbbbbbbbbbbb.json": legacy} {
		data, err := marshalActiveTask(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tasksDir(), name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := readActiveTask()
	if !ok {
		t.Fatal("legacy pointer was not adopted; the open envelope would vanish on upgrade")
	}
	if got.TaskKey != "o/legacy" {
		t.Fatalf("adopted %q, want the newest legacy pointer o/legacy", got.TaskKey)
	}

	// Adoption is a migration, not a lasting compatibility shim.
	entries, err := os.ReadDir(tasksDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "active.json" {
		t.Fatalf("legacy files survived adoption: %d entries", len(entries))
	}
}

// TestClaimActiveTaskIsAtomic pins the fix for the check-then-write race. The
// occupancy test and the write must be one syscall, or two concurrent opens both
// see a free slot and the loser is orphaned with no task_superseded — which is
// the defect this whole file exists to prevent, reintroduced by its own fix.
func TestClaimActiveTaskIsAtomic(t *testing.T) {
	withTempRoot(t)

	// Sequential first: the contract. A refused claim reports the occupant and
	// leaves the slot alone.
	if _, claimed, err := claimActiveTask(
		ActiveTask{TaskKey: "o/first", RepoRoot: "/a", OpenedAt: nowUTC()}); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	held, claimed, err := claimActiveTask(
		ActiveTask{TaskKey: "o/second", RepoRoot: "/b", OpenedAt: nowUTC()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed || held.TaskKey != "o/first" {
		t.Fatalf("second claim: claimed=%v held=%q", claimed, held.TaskKey)
	}
	if got, _ := readActiveTask(); got.TaskKey != "o/first" {
		t.Fatalf("a refused claim mutated the slot: %+v", got)
	}

	// Then the race itself, which is the whole point and which a sequential test
	// cannot see: a check followed by a write passes this contract and still
	// loses a task when two opens interleave.
	clearActiveTask()
	const racers = 16
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, ok, err := claimActiveTask(ActiveTask{
				TaskKey: fmt.Sprintf("o/racer-%d", i), RepoRoot: "/r", OpenedAt: nowUTC(),
			}); err == nil && ok {
				wins.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d concurrent claims won the single slot; exactly one may, or the losers are orphaned silently", got)
	}
}

// TestClaimActiveTaskRecoversACorruptSlotExactlyOnce pins the branch the link(2)
// publish made reachable-for-the-right-reason. Before it, a slot could be found
// empty for TWO different reasons — a crashed writer, or a live writer between
// its exclusive create and its write — and the recovery branch could not tell
// them apart, so a racer arriving mid-claim "recovered" into a second winner.
//
// Publishing a fully-written temp file with link(2) makes the zero-byte state
// unreachable, which is what lets recovery mean genuine corruption only. This
// asserts both halves: a corrupt pointer is still recovered rather than wedging
// the fleet forever, and concurrent racers against one still produce exactly one
// winner rather than each clearing and claiming in turn.
func TestClaimActiveTaskRecoversACorruptSlotExactlyOnce(t *testing.T) {
	withTempRoot(t)

	// Sequential: a truncated pointer is not an envelope, and must not wedge.
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeTaskPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := claimActiveTask(
		ActiveTask{TaskKey: "o/after-corrupt", RepoRoot: "/a", OpenedAt: nowUTC()}); err != nil || !claimed {
		t.Fatalf("a corrupt slot must be recoverable: claimed=%v err=%v", claimed, err)
	}
	if got, _ := readActiveTask(); got.TaskKey != "o/after-corrupt" {
		t.Fatalf("recovery left the slot at %+v", got)
	}

	// Concurrent, against a corrupt slot: still exactly one winner. Bounding the
	// retry is what holds this — unbounded retries let every racer clear and
	// re-claim, reintroducing the double-winner through the recovery path.
	if err := os.WriteFile(activeTaskPath(), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	const racers = 16
	var wg sync.WaitGroup
	var wins atomic.Int32
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, ok, err := claimActiveTask(ActiveTask{
				TaskKey: fmt.Sprintf("o/corrupt-racer-%d", i), RepoRoot: "/r", OpenedAt: nowUTC(),
			}); err == nil && ok {
				wins.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d claims won a corrupt slot; recovery must not become a second way to double-claim", got)
	}
	if _, ok := readActiveTask(); !ok {
		t.Fatal("the winner's pointer is unreadable; link(2) must only ever publish a complete file")
	}
}

// TestClaimActiveTaskReclaimsAnOrphanedRecoveryMarker covers the process that
// was killed mid-recovery. The marker makes recovery exclusive; without an age
// check it also makes a dead process able to wedge every later claim, and the
// caller treats a claim error as a warning and carries on WITHOUT an envelope —
// so a stuck marker reads downstream as a task nobody ever worked.
func TestClaimActiveTaskReclaimsAnOrphanedRecoveryMarker(t *testing.T) {
	withTempRoot(t)
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeTaskPath(), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recoverMarkerPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	orphaned := time.Now().Add(-10 * recoverMarkerStale)
	if err := os.Chtimes(recoverMarkerPath(), orphaned, orphaned); err != nil {
		t.Fatal(err)
	}

	got, claimed, err := claimActiveTask(
		ActiveTask{TaskKey: "o/after-orphan", RepoRoot: "/a", OpenedAt: nowUTC()})
	if err != nil || !claimed || got.TaskKey != "o/after-orphan" {
		t.Fatalf("an orphaned marker must not wedge the slot: claimed=%v got=%q err=%v", claimed, got.TaskKey, err)
	}
	if _, err := os.Stat(recoverMarkerPath()); !os.IsNotExist(err) {
		t.Fatalf("the recovery marker outlived the recovery: %v", err)
	}
}

// TestClaimViaExclusiveCreateIsTheLinklessFallback pins the contract of the path
// taken where link(2) is unavailable — network mounts and FAT-family volumes,
// reachable through PROMPTSTER_EXPERIMENT_DIR. It carries the original
// create-then-write window, which is why it is a fallback and not the default;
// what it must NOT do is fail to publish, because cmdOpen would then record the
// task as opened with nothing behind it.
func TestClaimViaExclusiveCreateIsTheLinklessFallback(t *testing.T) {
	withTempRoot(t)
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := marshalActiveTask(ActiveTask{TaskKey: "o/fallback", RepoRoot: "/a", OpenedAt: nowUTC()})
	if err != nil {
		t.Fatal(err)
	}
	got, claimed, err := claimViaExclusiveCreate(
		ActiveTask{TaskKey: "o/fallback", RepoRoot: "/a", OpenedAt: nowUTC()}, data)
	if err != nil || !claimed || got.TaskKey != "o/fallback" {
		t.Fatalf("free slot: claimed=%v got=%q err=%v", claimed, got.TaskKey, err)
	}
	if stored, ok := readActiveTask(); !ok || stored.TaskKey != "o/fallback" {
		t.Fatalf("the fallback must publish a READABLE pointer, got %+v ok=%v", stored, ok)
	}

	held, claimed, err := claimViaExclusiveCreate(
		ActiveTask{TaskKey: "o/second", RepoRoot: "/b", OpenedAt: nowUTC()}, data)
	if err != nil {
		t.Fatal(err)
	}
	if claimed || held.TaskKey != "o/fallback" {
		t.Fatalf("occupied slot: claimed=%v held=%q", claimed, held.TaskKey)
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
