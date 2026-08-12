package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const schemaVersion = 1

// Arm cell names are the backend's, verbatim from the frozen "Assignment row
// contract" in design.md. The CLI does not get its own vocabulary for the same
// value — one name per cell, or the two logs cannot be joined.
const (
	CellControl = "c1off_c2off"
	CellC1      = "c1on_c2off"
	CellC2      = "c1off_c2on"
	CellC1C2    = "c1on_c2on"

	CellIneligible = "ineligible"
)

// armCells is the block. Every complete block of 4 within a stratum contains
// exactly one of these.
var armCells = []string{CellControl, CellC1, CellC2, CellC1C2}

// taskClasses follows the backend contract's enum ("feature | fix | analysis |
// ops"). design.md's prose says "analysis-spec" for the same thing; the wire
// name wins and the prose spelling is accepted as an alias so nobody's muscle
// memory silently creates a fourth class.
var taskClasses = map[string]string{
	"feature":       "feature",
	"fix":           "fix",
	"analysis":      "analysis",
	"analysis-spec": "analysis",
	"ops":           "ops",
}

var sizeBands = map[string]string{"s": "S", "m": "M", "l": "L"}

// taskKeyRe is the backend's canary-safe key contract.
var taskKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,119}$`)

type Factors struct {
	C1 bool `json:"c1"`
	C2 bool `json:"c2"`
}

// Assignment is the immutable ground-truth row.
//
// Field names match the backend's POST /v1/teams/experiments/assignment request
// and response so the sync path is a replay, not a translation. Fields below
// the wire block are LOCAL ONLY and are stripped by syncPayload — `repoRoot` in
// particular is a raw absolute path and falls under the same no-raw-path
// contract as teams ingest.
type Assignment struct {
	SchemaVersion int `json:"schemaVersion"`

	// --- request fields ---
	ExperimentKey string `json:"experimentKey"`
	TaskKey       string `json:"taskKey"`
	WeekBlock     string `json:"weekBlock"`
	Repo          string `json:"repo"`
	TaskClass     string `json:"taskClass"`
	SizeBand      string `json:"sizeBand"`
	CliVersion    string `json:"cliVersion"`

	// --- arm fields ---
	AssignmentID string  `json:"assignmentId"`
	Arm          string  `json:"arm"`
	Factors      Factors `json:"factors"`
	// Stratum is the allocation stratum: "repo|taskClass". Size band is a
	// covariate column, deliberately NOT part of this key.
	Stratum string `json:"stratum"`
	// StratumPosition is the slot this row took in its allocation key. The
	// backend stores it UNIQUE per key, so it is what makes a double-booked slot
	// impossible rather than merely unlikely.
	StratumPosition int `json:"stratumPosition"`
	// AssignedAt is the instant the engineer was SHOWN the arm — not the instant
	// a row reached a database (the backend records that separately).
	AssignedAt string `json:"assignedAt"`
	// AssignmentSource names the single authority for Arm: "cli-offline" rows
	// were drawn here and the server stores them verbatim; "server" rows were
	// drawn there. One authority per row, never a reconcile.
	AssignmentSource string `json:"assignmentSource"`

	// --- local only, never sent ---
	OrgID         string   `json:"orgId"`
	EngineerID    string   `json:"engineerId"`
	Block         int      `json:"block"`
	BlockSeed     string   `json:"blockSeed"`
	TaskHash      string   `json:"taskHash"`
	Eligible      bool     `json:"eligible"`
	ExclusionCode string   `json:"exclusionCode,omitempty"`
	SpecVersion   string   `json:"specVersion"`
	Envelope      Envelope `json:"envelope"`
	// Synced is always false and stays false. It was written before sync existed
	// and cannot become true, because the row is immutable and this package has
	// no path that rewrites one. Sync state lives in its own append-only receipt
	// log (sync-receipts.jsonl) and is derived by reading it; nothing should read
	// this field. Kept only so rows already on disk still round-trip.
	Synced bool `json:"synced"`
}

type Envelope struct {
	OpenedAt string `json:"openedAt"`
	RepoRoot string `json:"repoRoot"`
	Title    string `json:"title,omitempty"`
}

// syncPayload is the exact POST body for
// POST /v1/teams/experiments/assignment. An offline draw carries arm +
// stratumPosition + assignedAt; the server validates the cell is legal and the
// slot is free, then stores them verbatim. It never recomputes, so a task the
// engineer already worked under arm X can never become arm Y — it 409s instead.
//
// repoRoot and title are LOCAL and never appear here: repoRoot is a raw
// absolute path, under the same no-raw-path contract as teams ingest.
func (a Assignment) syncPayload() map[string]any {
	return map[string]any{
		"experimentKey":    a.ExperimentKey,
		"taskKey":          a.TaskKey,
		"weekBlock":        a.WeekBlock,
		"repo":             a.Repo,
		"taskClass":        a.TaskClass,
		"sizeBand":         a.SizeBand,
		"cliVersion":       a.CliVersion,
		"assignmentSource": a.AssignmentSource,
		"arm":              a.Arm,
		"stratumPosition":  a.StratumPosition,
		"assignedAt":       a.AssignedAt,
	}
}

// weekBlock is the ISO week, e.g. "2026-W33".
func weekBlock(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// stratumString is the allocation stratum: repo x task class.
//
// Size band is deliberately absent. design.md's batch-1 section stratifies on
// "repo x task class", and repo x class x size would be 27 cells for ~50 tasks,
// leaving most strata holding a single task — strictly worse for balance than
// not stratifying on size at all. Size rides along as an analysis covariate.
func stratumString(repo, class string) string {
	return repo + "|" + class
}

func taskFingerprint(engineerID, taskKey, week string) string {
	sum := sha256.Sum256([]byte(engineerID + "|" + taskKey + "|" + week))
	return hex.EncodeToString(sum[:])[:16]
}

// blockSeed is the allocator seed, byte-for-byte as specified by the backend
// half (openspec 0.3). NUL separators so no two field values can be confused by
// concatenation; `block` as its ASCII decimal.
//
//	sha256(experimentKey || 0x00 || orgId || 0x00 || engineerUserId || 0x00 ||
//	       stratum || 0x00 || decimal(block))
func blockSeed(experimentKey, orgID, engineerID, stratum string, block int) []byte {
	h := sha256.New()
	for _, part := range []string{experimentKey, orgID, engineerID, stratum} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write([]byte(strconv.Itoa(block)))
	return h.Sum(nil)
}

// hashStream is the allocator's random source: the concatenation of
// sha256(seed || uint32be(i)) for i = 0, 1, 2, ..., consumed 4 bytes at a time.
// A counter-extended hash rather than a stdlib RNG so the Go and TypeScript
// implementations produce identical draws.
type hashStream struct {
	seed []byte
	buf  []byte
	i    uint32
}

func (s *hashStream) next4() uint32 {
	for len(s.buf) < 4 {
		var ctr [4]byte
		binary.BigEndian.PutUint32(ctr[:], s.i)
		s.i++
		sum := sha256.Sum256(append(append([]byte(nil), s.seed...), ctr[:]...))
		s.buf = append(s.buf, sum[:]...)
	}
	v := binary.BigEndian.Uint32(s.buf[:4])
	s.buf = s.buf[4:]
	return v
}

// permuteCells returns the 4 cells in the allocator's order for this seed:
// Fisher-Yates over armCells in REGISTRY DECLARATION ORDER, j from n-1 down to
// 1, k = next4 mod (j+1).
func permuteCells(seed []byte) []string {
	cells := append([]string(nil), armCells...)
	st := &hashStream{seed: seed}
	for j := len(cells) - 1; j >= 1; j-- {
		k := int(st.next4() % uint32(j+1))
		cells[j], cells[k] = cells[k], cells[j]
	}
	return cells
}

// assignArm picks the cell by stratified permuted-block randomization, mirroring
// the backend allocator exactly (openspec 0.3) so an offline draw and a server
// draw of the same slot are the same arm.
//
// This replaced a bare hash(...) -> cell, which is unstratified simple
// randomization: over batch 1's expected 40-60 tasks it routinely lands
// 18/8-style splits across the four cells, and this batch has no power to
// spare. Permuted blocks keep every property the design is after — deterministic,
// assigned before work, replayable, zero experimenter discretion — and add
// balance by construction.
//
// Allocation key is (experimentKey, orgId, engineerUserId, stratum) —
// WITHIN-engineer, because design.md randomizes per task within engineer.
// position is the count of prior ELIGIBLE assignments in that key; exclusions
// never consume a slot, or the blocks develop holes and the balance property
// this design exists for evaporates.
func assignArm(cfg Config, stratum string, position int) (arm string, block int, seedHex string) {
	block = position / len(armCells)
	seed := blockSeed(cfg.ExperimentKey, cfg.OrgID, cfg.EngineerID, stratum, block)
	return permuteCells(seed)[position%len(armCells)], block, hex.EncodeToString(seed)
}

func factorsFor(arm string) Factors {
	switch arm {
	case CellC1:
		return Factors{C1: true}
	case CellC2:
		return Factors{C2: true}
	case CellC1C2:
		return Factors{C1: true, C2: true}
	}
	return Factors{}
}

// armLabel is display only — the banners and `status` read better as "C1" than
// as "c1on_c2off". The wire value is never this string.
func armLabel(arm string) string {
	switch arm {
	case CellControl:
		return "control"
	case CellC1:
		return "C1"
	case CellC2:
		return "C2"
	case CellC1C2:
		return "C1+C2"
	}
	return arm
}

// nextStratumPosition counts prior ELIGIBLE assignments in this allocation key.
// The append-only log IS the position state — there is no counter to drift out
// of sync with the rows. Exclusions are skipped so blocks never develop holes.
func nextStratumPosition(rows []Assignment, cfg Config, stratum string) int {
	n := 0
	for _, r := range rows {
		if r.Eligible && r.OrgID == cfg.OrgID && r.EngineerID == cfg.EngineerID &&
			r.ExperimentKey == cfg.ExperimentKey && r.Stratum == stratum {
			n++
		}
	}
	return n
}

// findAssignment matches the backend's idempotency key:
// (experimentKey, orgId, engineerUserId, taskKey).
func findAssignment(rows []Assignment, cfg Config, taskKey string) (Assignment, bool) {
	for _, r := range rows {
		if r.OrgID == cfg.OrgID && r.EngineerID == cfg.EngineerID &&
			r.ExperimentKey == cfg.ExperimentKey && r.TaskKey == taskKey {
			return r, true
		}
	}
	return Assignment{}, false
}

// newAssignment builds (but does not persist) the row for a task open.
// exclusion is "" for an eligible task.
func newAssignment(cfg Config, rows []Assignment, taskKey, repo, class, size string, env Envelope, exclusion string, now time.Time) Assignment {
	week := weekBlock(now)
	stratum := stratumString(repo, class)
	a := Assignment{
		SchemaVersion: schemaVersion,
		ExperimentKey: cfg.ExperimentKey,
		TaskKey:       taskKey,
		WeekBlock:     week,
		Repo:          repo,
		TaskClass:     class,
		SizeBand:      size,
		CliVersion:    toolVersion,
		Stratum:       stratum,
		AssignedAt:    now.UTC().Format(time.RFC3339Nano),

		AssignmentSource: "cli-offline",
		OrgID:            cfg.OrgID,
		EngineerID:       cfg.EngineerID,
		TaskHash:         taskFingerprint(cfg.EngineerID, taskKey, week),
		Eligible:         exclusion == "",
		ExclusionCode:    exclusion,
		SpecVersion:      TreatmentSpecVersion,
		Envelope:         env,
	}
	// Local id until the server issues a uuid. Deterministic so a replay of the
	// log produces the same ids.
	a.AssignmentID = "local-" + a.TaskHash + "-" + shortHash(cfg.OrgID+"|"+cfg.ExperimentKey)

	if !a.Eligible {
		a.Arm = CellIneligible
		a.StratumPosition = -1
		return a
	}
	a.StratumPosition = nextStratumPosition(rows, cfg, stratum)
	a.Arm, a.Block, a.BlockSeed = assignArm(cfg, stratum, a.StratumPosition)
	a.Factors = factorsFor(a.Arm)
	return a
}

func validateClass(c string) (string, error) {
	got, ok := taskClasses[strings.ToLower(strings.TrimSpace(c))]
	if !ok {
		return "", fmt.Errorf("task class %q is not one of feature, fix, analysis, ops", c)
	}
	return got, nil
}

func validateSize(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		s = "m"
	}
	got, ok := sizeBands[s]
	if !ok {
		return "", fmt.Errorf("size band %q is not one of S, M, L", s)
	}
	return got, nil
}

// validateTaskKey enforces the backend's canary-safe key contract so a key that
// works offline cannot be rejected at sync time — after the engineer has
// already done the work under an arm the server will not accept.
func validateTaskKey(k string) error {
	k = strings.TrimSpace(k)
	if !taskKeyRe.MatchString(k) {
		return fmt.Errorf("task key %q must match [A-Za-z0-9][A-Za-z0-9._/-]{0,119}", k)
	}
	if strings.Contains(k, "..") {
		return fmt.Errorf("task key %q must not contain %q", k, "..")
	}
	if strings.HasPrefix(k, "/") || strings.HasPrefix(k, "~") {
		return fmt.Errorf("task key %q must not be a path", k)
	}
	if strings.Count(k, "/") > 2 {
		return fmt.Errorf("task key %q has too many path segments (expected <repo>/<slug>)", k)
	}
	return nil
}

// checkRepoAttribution refuses a task key whose repo segment contradicts the
// repo the envelope is being opened in.
//
// This is not tidiness. The stratum is repo × class, so a wrong repo draws the
// arm from the wrong permuted-block sequence — and the assignment log is
// append-only, so nothing about it can be fixed afterwards. It happened once
// for real (batch-1 prereg amendment A2): a promptster-backend task opened from
// the promptster-teams checkout landed in the "teams|feature" stratum.
//
// It refuses rather than warns because a warning in a busy terminal is a
// warning nobody reads, and the mistake is unfixable by the time anyone reads
// the log. Both escapes produce a CORRECT row rather than suppressing the
// check: pass --repo to state the repo explicitly, or drop the segment from the
// key (a key with no "/" is not claiming a repo and is not checked).
func checkRepoAttribution(taskKey, repoSlug string) error {
	seg, _, found := strings.Cut(strings.TrimSpace(taskKey), "/")
	if !found || seg == "" || repoSlug == "" {
		return nil
	}
	name := repoSlug
	if _, after, ok := strings.Cut(repoSlug, "/"); ok {
		name = after
	}
	if strings.EqualFold(seg, name) {
		return nil
	}
	return fmt.Errorf(
		"task key %q names repo %q but this checkout is %q.\n"+
			"  The stratum is repo × class, the arm is drawn from it, and the log is append-only —\n"+
			"  a wrong repo here cannot be corrected later. Resolve it before opening:\n"+
			"    --repo %-28s if the work really lands in %s\n"+
			"    --task %-28s if it lands here\n"+
			"  (a task key with no \"/\" claims no repo and is not checked)",
		taskKey, seg, repoSlug, "<owner>/"+seg, seg, name+"/<slug>")
}
