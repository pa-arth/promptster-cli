package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{
		OrgID: "org_test", EngineerID: "eng@example.com",
		ExperimentKey: "batch1-context-mechanics", Enabled: true,
	}
}

// --- allocator: must mirror the backend spec exactly ------------------------

// TestBlockSeedMatchesTheSpecLiterally recomputes the seed from the written
// spec independently of blockSeed's implementation, so a refactor that changes
// the byte layout fails here instead of silently diverging from the server.
//
//	sha256(experimentKey || 0x00 || orgId || 0x00 || engineerUserId || 0x00 ||
//	       stratum || 0x00 || decimal(block))
func TestBlockSeedMatchesTheSpecLiterally(t *testing.T) {
	ek, org, eng, stratum, block := "batch1-context-mechanics", "org_test", "eng@example.com", "pa-arth/promptster-cli|feature", 3

	var want []byte
	want = append(want, []byte(ek)...)
	want = append(want, 0)
	want = append(want, []byte(org)...)
	want = append(want, 0)
	want = append(want, []byte(eng)...)
	want = append(want, 0)
	want = append(want, []byte(stratum)...)
	want = append(want, 0)
	want = append(want, []byte(strconv.Itoa(block))...)
	sum := sha256.Sum256(want)

	got := blockSeed(ek, org, eng, stratum, block)
	if string(got) != string(sum[:]) {
		t.Fatalf("seed mismatch\n got %x\nwant %x", got, sum)
	}
}

// TestHashStreamIsCounterExtendedSha256 pins the random source: the
// concatenation of sha256(seed || uint32be(i)), consumed 4 bytes at a time.
func TestHashStreamIsCounterExtendedSha256(t *testing.T) {
	seed := []byte("seed-bytes")
	st := &hashStream{seed: seed}

	var expect []byte
	for i := uint32(0); i < 2; i++ {
		var ctr [4]byte
		binary.BigEndian.PutUint32(ctr[:], i)
		sum := sha256.Sum256(append(append([]byte(nil), seed...), ctr[:]...))
		expect = append(expect, sum[:]...)
	}
	for n := 0; n < 16; n++ {
		want := binary.BigEndian.Uint32(expect[n*4 : n*4+4])
		if got := st.next4(); got != want {
			t.Fatalf("draw %d: got %d want %d", n, got, want)
		}
	}
}

// TestPermutationFollowsDeclarationOrderFisherYates recomputes the permutation
// straight from the spec's loop and compares.
func TestPermutationFollowsDeclarationOrderFisherYates(t *testing.T) {
	seed := blockSeed("e", "o", "n", "s", 0)

	want := []string{CellControl, CellC1, CellC2, CellC1C2} // registry declaration order
	st := &hashStream{seed: seed}
	for j := len(want) - 1; j >= 1; j-- {
		k := int(st.next4() % uint32(j+1))
		want[j], want[k] = want[k], want[j]
	}

	got := permuteCells(seed)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permutation mismatch at %d: got %v want %v", i, got, want)
		}
	}
}

// selfVectors is a self-generated regression fixture. It proves this
// implementation is STABLE, not that it agrees with the backend's — the
// backend half is publishing shared vectors and agreement is UNVERIFIED until
// this is re-run against that file.
func TestAllocatorSelfVectorsAreStable(t *testing.T) {
	cfg := testCfg()
	stratum := "pa-arth/promptster-cli|feature"
	var got []string
	for pos := 0; pos < 8; pos++ {
		arm, _, _ := assignArm(cfg, stratum, pos)
		got = append(got, arm)
	}
	want := []string{
		"c1off_c2off", "c1off_c2on", "c1on_c2on", "c1on_c2off",
		"c1on_c2on", "c1on_c2off", "c1off_c2on", "c1off_c2off",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allocator drifted at position %d:\n got %v\nwant %v", i, got, want)
		}
	}
}

// --- balance properties -----------------------------------------------------

func TestEveryCompleteBlockHoldsAllFourCells(t *testing.T) {
	cfg := testCfg()
	stratum := "pa-arth/promptster-cli|feature"

	for block := 0; block < 25; block++ {
		seen := map[string]int{}
		for slot := 0; slot < 4; slot++ {
			arm, gotBlock, _ := assignArm(cfg, stratum, block*4+slot)
			if gotBlock != block {
				t.Fatalf("position %d: block %d, want %d", block*4+slot, gotBlock, block)
			}
			seen[arm]++
		}
		if len(seen) != 4 {
			t.Fatalf("block %d is not a permutation of the 4 cells: %v", block, seen)
		}
	}
}

func TestAssignmentIsDeterministic(t *testing.T) {
	cfg := testCfg()
	stratum := "pa-arth/promptster-backend|fix"
	for pos := 0; pos < 40; pos++ {
		a1, b1, s1 := assignArm(cfg, stratum, pos)
		a2, b2, s2 := assignArm(cfg, stratum, pos)
		if a1 != a2 || b1 != b2 || s1 != s2 {
			t.Fatalf("position %d not deterministic: %v/%v", pos, a1, a2)
		}
	}
}

func TestDifferentStrataGetDifferentPermutations(t *testing.T) {
	cfg := testCfg()
	same := 0
	for pos := 0; pos < 16; pos++ {
		x, _, _ := assignArm(cfg, "pa-arth/promptster-cli|feature", pos)
		y, _, _ := assignArm(cfg, "pa-arth/promptster-backend|feature", pos)
		if x == y {
			same++
		}
	}
	if same == 16 {
		t.Fatal("two strata produced an identical arm sequence — the stratum is not in the seed")
	}
}

func TestDifferentEngineersGetDifferentPermutations(t *testing.T) {
	a := testCfg()
	b := testCfg()
	b.EngineerID = "other@example.com"
	same := 0
	for pos := 0; pos < 16; pos++ {
		x, _, _ := assignArm(a, "r|feature", pos)
		y, _, _ := assignArm(b, "r|feature", pos)
		if x == y {
			same++
		}
	}
	if same == 16 {
		t.Fatal("allocation key is not within-engineer")
	}
}

// The reason permuted blocks exist at all: across a realistic batch the four
// cells stay exactly equal, which unstratified hashing does not guarantee.
func TestBatchSizedRunStaysBalanced(t *testing.T) {
	cfg := testCfg()
	strata := []string{
		"pa-arth/promptster-cli|feature",
		"pa-arth/promptster-cli|fix",
		"pa-arth/promptster-backend|feature",
		"pa-arth/promptster-backend|fix",
		"pa-arth/promptster-teams|analysis",
	}
	counts := map[string]int{}
	pos := map[string]int{}
	for i := 0; i < 60; i++ {
		s := strata[i%len(strata)]
		arm, _, _ := assignArm(cfg, s, pos[s])
		pos[s]++
		counts[arm]++
	}
	for _, arm := range armCells {
		// 60 tasks over 5 strata = 12 each = 3 complete blocks per stratum, so
		// every cell should land exactly 15 times.
		if counts[arm] != 15 {
			t.Fatalf("arm %s got %d of 60, want 15 (counts: %v)", arm, counts[arm], counts)
		}
	}
}

// --- row construction -------------------------------------------------------

func TestFactorsMatchArm(t *testing.T) {
	cases := map[string]Factors{
		CellControl: {false, false},
		CellC1:      {true, false},
		CellC2:      {false, true},
		CellC1C2:    {true, true},
	}
	for arm, want := range cases {
		if got := factorsFor(arm); got != want {
			t.Fatalf("arm %s: got %+v want %+v", arm, got, want)
		}
	}
}

func TestSizeBandIsNotPartOfTheStratum(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	var rows []Assignment
	small := newAssignment(cfg, rows, "repo/t-1", "o/r", "feature", "S", Envelope{}, "", now)
	rows = append(rows, small)
	large := newAssignment(cfg, rows, "repo/t-2", "o/r", "feature", "L", Envelope{}, "", now)

	if small.Stratum != large.Stratum {
		t.Fatalf("size leaked into the stratum: %q vs %q", small.Stratum, large.Stratum)
	}
	if large.StratumPosition != 1 {
		t.Fatalf("second task in the same stratum got position %d, want 1", large.StratumPosition)
	}
	if small.SizeBand != "S" || large.SizeBand != "L" {
		t.Fatal("size band must still be recorded as a covariate")
	}
}

func TestIneligibleTasksNeverConsumeASlot(t *testing.T) {
	cfg := testCfg()
	now := time.Now()

	var rows []Assignment
	a0 := newAssignment(cfg, rows, "repo/t-1", "o/r", "feature", "M", Envelope{}, "", now)
	rows = append(rows, a0)
	rows = append(rows, newAssignment(cfg, rows, "repo/t-2", "o/r", "ops", "M", Envelope{}, "ops_task", now))
	rows = append(rows, newAssignment(cfg, rows, "repo/t-3", "o/r", "feature", "M", Envelope{}, "under_30m", now))
	a3 := newAssignment(cfg, rows, "repo/t-4", "o/r", "feature", "M", Envelope{}, "", now)

	if a0.StratumPosition != 0 {
		t.Fatalf("first eligible task position = %d, want 0", a0.StratumPosition)
	}
	if a3.StratumPosition != 1 {
		t.Fatalf("second eligible task position = %d, want 1 (exclusions must not consume slots)", a3.StratumPosition)
	}
	if rows[1].Arm != CellIneligible || rows[1].ExclusionCode != "ops_task" {
		t.Fatalf("ops task should be ineligible: %+v", rows[1])
	}
	if rows[1].StratumPosition != -1 {
		t.Fatalf("excluded row claimed slot %d", rows[1].StratumPosition)
	}
}

func TestExistingTaskIsFoundByTheBackendIdempotencyKey(t *testing.T) {
	cfg := testCfg()
	var rows []Assignment
	for i := 0; i < 6; i++ {
		rows = append(rows, newAssignment(cfg, rows, fmt.Sprintf("repo/t-%d", i), "o/r", "feature", "M", Envelope{}, "", time.Now()))
	}
	first, ok := findAssignment(rows, cfg, "repo/t-2")
	if !ok {
		t.Fatal("repo/t-2 not found")
	}
	// A different experiment must NOT match the same taskKey.
	other := cfg
	other.ExperimentKey = "batch2-ops"
	if _, ok := findAssignment(rows, other, "repo/t-2"); ok {
		t.Fatal("idempotency key ignored experimentKey")
	}
	again, _ := findAssignment(rows, cfg, "repo/t-2")
	if again.Arm != first.Arm || again.AssignmentID != first.AssignmentID {
		t.Fatalf("lookup is not stable: %+v vs %+v", first, again)
	}
}

func TestOfflineRowCarriesEverythingTheServerNeedsToStoreItVerbatim(t *testing.T) {
	cfg := testCfg()
	a := newAssignment(cfg, nil, "repo/t-1", "pa-arth/promptster-cli", "feature", "M",
		Envelope{RepoRoot: "/Users/someone/repos/promptster-cli", Title: "t"}, "", time.Now())

	p := a.syncPayload()
	for _, k := range []string{"arm", "stratumPosition", "assignedAt", "assignmentSource", "experimentKey", "taskKey"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("sync payload is missing %q — the server cannot store an offline draw without it", k)
		}
	}
	if p["assignmentSource"] != "cli-offline" {
		t.Fatalf("assignmentSource = %v, want cli-offline", p["assignmentSource"])
	}
	// repoRoot is a raw absolute path and must never leave the machine.
	for k, v := range p {
		if s, ok := v.(string); ok && s == a.Envelope.RepoRoot {
			t.Fatalf("sync payload leaks the raw repoRoot in field %q", k)
		}
	}
	if _, leaked := p["envelope"]; leaked {
		t.Fatal("sync payload includes the local envelope")
	}
}

func TestWeekBlockISOFormat(t *testing.T) {
	if got := weekBlock(time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)); got != "2026-W33" {
		t.Fatalf("weekBlock = %s, want 2026-W33", got)
	}
}

func TestValidateClassAndSize(t *testing.T) {
	if got, err := validateClass("Feature"); err != nil || got != "feature" {
		t.Fatalf("Feature should normalize: %q %v", got, err)
	}
	// design.md's prose spelling maps onto the wire enum rather than becoming a
	// fourth class.
	if got, err := validateClass("analysis-spec"); err != nil || got != "analysis" {
		t.Fatalf("analysis-spec should map to analysis: %q %v", got, err)
	}
	if _, err := validateClass("chore"); err == nil {
		t.Fatal("chore should be rejected")
	}
	if s, err := validateSize(""); err != nil || s != "M" {
		t.Fatalf("empty size should default to M, got %q %v", s, err)
	}
	if s, err := validateSize("l"); err != nil || s != "L" {
		t.Fatalf("size should uppercase, got %q %v", s, err)
	}
	if _, err := validateSize("xl"); err == nil {
		t.Fatal("xl should be rejected")
	}
}

func TestValidateTaskKeyMatchesTheBackendContract(t *testing.T) {
	ok := []string{"promptster-cli/task-envelope", "PE-101", "repo/a.b_c-1"}
	for _, k := range ok {
		if err := validateTaskKey(k); err != nil {
			t.Fatalf("%q should be valid: %v", k, err)
		}
	}
	bad := []string{
		"",
		"/Users/paarth/repos/x",
		"~/repos/x",
		"repo/../etc/passwd",
		"a/b/c/d",
		"has space",
		"-leading-dash-is-not-alnum",
	}
	for _, k := range bad {
		if err := validateTaskKey(k); err == nil {
			t.Fatalf("%q should be rejected", k)
		}
	}
}

func TestRepoSlugFromURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:pa-arth/promptster-cli.git":     "pa-arth/promptster-cli",
		"https://github.com/pa-arth/promptster-cli.git": "pa-arth/promptster-cli",
		"https://user@github.com/pa-arth/x":             "pa-arth/x",
		"ssh://git@github.com/pa-arth/y.git":            "pa-arth/y",
	}
	for url, want := range cases {
		if got := repoSlugFromURL(url); got != want {
			t.Fatalf("%s -> %q, want %q", url, got, want)
		}
	}
}
