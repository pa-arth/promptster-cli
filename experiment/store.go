package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxLineBytes keeps every appended JSONL record under the POSIX atomic-append
// size (PIPE_BUF, 4096 on macOS/Linux). Staying under it is why this store
// needs no lock file: concurrent O_APPEND writes of sub-PIPE_BUF buffers do not
// interleave. A record that would exceed it is a bug, not something to split —
// we truncate the free-text fields at construction and error here if that
// somehow failed.
const maxLineBytes = 4000

func assignmentsPath() string { return filepath.Join(rootDir(), "assignments.jsonl") }
func eventsPath() string      { return filepath.Join(rootDir(), "events.jsonl") }
func tasksDir() string        { return filepath.Join(rootDir(), "tasks") }
func gateDir() string         { return filepath.Join(rootDir(), "gate") }
func rejectedDir() string     { return filepath.Join(rootDir(), "rejected") }

// appendJSONL appends one record. Append-only by construction: this package has
// no code path that opens these files for writing without O_APPEND, and none
// that rewrites or deletes a row. Assignment is immutable ground truth
// (design.md: "detectors measure adherence, never assignment").
func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b)+1 > maxLineBytes {
		return fmt.Errorf("record is %d bytes, over the %d-byte atomic-append limit", len(b)+1, maxLineBytes)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// readAssignments returns every assignment row in write order. A corrupt line
// is skipped rather than fatal — a half-written tail must not make the whole
// log unreadable mid-experiment.
func readAssignments() ([]Assignment, error) {
	f, err := os.Open(assignmentsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Assignment
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var a Assignment
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, sc.Err()
}

// Event is the adherence/telemetry stream. Stored separately from assignments
// so an adherence write can never touch an assignment row.
// Field names follow the backend's POST /v1/teams/experiments/adherence body:
// { assignmentId | (experimentKey, taskKey), complianceEvent, observedAt,
//
//	source, detail }.
type Event struct {
	SchemaVersion   int    `json:"schemaVersion"`
	ObservedAt      string `json:"observedAt"`
	ComplianceEvent string `json:"complianceEvent"`
	Source          string `json:"source"`
	ExperimentKey   string `json:"experimentKey"`
	TaskKey         string `json:"taskKey,omitempty"`
	AssignmentID    string `json:"assignmentId,omitempty"`
	OrgID           string `json:"orgId"`
	EngineerID      string `json:"engineerId"`
	Arm             string `json:"arm,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	Detail          string `json:"detail,omitempty"`
	CharLen         int    `json:"charLen,omitempty"`
	Attempt         int    `json:"attempt,omitempty"`
}

func recordEvent(e Event) error {
	e.SchemaVersion = schemaVersion
	e.ObservedAt = nowUTC()
	if e.Source == "" {
		e.Source = "cli-hook"
	}
	e.Detail = truncate(e.Detail, 400)
	return appendJSONL(eventsPath(), e)
}

// ActiveTask names the ONE task whose envelope is currently open, for every
// checkout at once.
//
// It used to be one pointer per repo root, keyed by sha256(repoRoot), on the
// theory that one worktree means one task. Batch 1 falsified that: all seven
// envelopes were opened from a single control checkout while the work ran in
// worktrees under other repos, so all seven hashed to the SAME key. Two things
// followed, and both corrupted the experiment rather than merely annoying
// anyone. Each `open` silently destroyed the previous pointer — one envelope
// was orphaned 43 minutes in and still reads "open" in the log with no close.
// And the hooks resolve the artifact by hashing the SESSION's cwd, so a session
// working the assigned task from its own worktree found no pointer at all: it
// got no C1 contract and could not arm the C2 gate, which is treatment
// delivery keyed to the wrong thing entirely.
//
// One global pointer matches how the work actually happens — a control checkout
// dispatching into worktrees — and makes silent orphaning impossible by
// construction, because there is exactly one slot and `open` refuses to
// overwrite an occupied one.
//
// RepoRoot is retained as a record of where the envelope was opened FROM. It no
// longer selects anything.
type ActiveTask struct {
	TaskKey  string `json:"taskKey"`
	RepoRoot string `json:"repoRoot"`
	OpenedAt string `json:"openedAt"`
}

func activeTaskPath() string { return filepath.Join(tasksDir(), "active.json") }

func marshalActiveTask(t ActiveTask) ([]byte, error) {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeActiveTask(t ActiveTask) error {
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		return err
	}
	data, err := marshalActiveTask(t)
	if err != nil {
		return err
	}
	return os.WriteFile(activeTaskPath(), data, 0o600)
}

// claimActiveTask takes the single envelope slot ATOMICALLY, returning the
// occupant when one already holds it. Checking occupancy and then writing are
// two syscalls, and this fleet runs 5-10 sessions against the same state
// directory: two `open`s interleaving between the check and the write would both
// believe the slot was free and the loser would be orphaned silently, which is
// the exact defect this file exists to make impossible.
//
// O_CREATE|O_EXCL was the first fix and it was not enough, because a CLAIM is
// not a file — it is a file WITH A BODY IN IT, and the body is a second syscall.
// Between the exclusive create and the write the slot exists at zero bytes, and
// a racer arriving in that window read it, failed to unmarshal, and took the
// "unreadable pointer is not an envelope" recovery branch — a SECOND winner that
// overwrote the first claim. TestClaimActiveTaskIsAtomic caught it about one run
// in five.
//
// The real defect was an ambiguity, not a missing lock: an empty slot file has
// two causes — a writer that CRASHED mid-claim (recover) and a writer that is
// ALIVE and microseconds into its claim (back off) — and the code could not tell
// them apart, so it recovered unconditionally.
//
// So write the body into a temp file FIRST and publish it with link(2), which
// fails with EEXIST exactly like O_EXCL but can only ever make a COMPLETE file
// visible. The zero-byte state is now unreachable, which is what lets the
// recovery branch below be reserved for genuine corruption.
// The recover loop is bounded by TIME, not by a retry count. A count encodes an
// assumption about how long recovery takes, and this fleet runs 5-10 sessions on
// one box where load averages above 50 are normal — eight one-millisecond tries
// would report "unreadable pointer" for a recovery that was merely slow, and the
// caller treats that as a warning and carries on WITHOUT an envelope.
//
// recoverMarkerStale is the other half: a process killed while holding the
// marker must not wedge every later claim, so a marker older than this is
// treated as orphaned and cleared.
const (
	claimDeadline      = 5 * time.Second
	claimBackoffMax    = 50 * time.Millisecond
	recoverMarkerStale = 2 * time.Second
)

func recoverMarkerPath() string { return filepath.Join(tasksDir(), "active.recover") }

// claimViaExclusiveCreate is the fallback for filesystems that reject hard links
// (some network mounts, FAT-family volumes) — reachable through
// PROMPTSTER_EXPERIMENT_DIR, or a home directory on such a mount. It is the
// pre-link implementation, so the narrow create-then-write window returns THERE
// and only there, which is strictly no worse than shipping a claim that cannot
// be published at all: cmdOpen treats a claim error as a warning and continues,
// recording the task as opened with no envelope behind it.
func claimViaExclusiveCreate(t ActiveTask, data []byte) (ActiveTask, bool, error) {
	f, err := os.OpenFile(activeTaskPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			if held, ok := readActiveTask(); ok {
				return held, false, nil
			}
			return ActiveTask{}, true, writeActiveTask(t)
		}
		return ActiveTask{}, false, err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return ActiveTask{}, false, err
	}
	return t, true, nil
}

func claimActiveTask(t ActiveTask) (ActiveTask, bool, error) {
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		return ActiveTask{}, false, err
	}
	data, err := marshalActiveTask(t)
	if err != nil {
		return ActiveTask{}, false, err
	}

	// Same directory, so link(2) below never crosses a filesystem boundary.
	tmp, err := os.CreateTemp(tasksDir(), "active-*.claim")
	if err != nil {
		return ActiveTask{}, false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return ActiveTask{}, false, err
	}
	if err := tmp.Close(); err != nil {
		return ActiveTask{}, false, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return ActiveTask{}, false, err
	}

	// Recovering a corrupt slot must not open a window either, and the obvious
	// version does. "Hold a marker, re-read, remove, then re-link" still has the
	// read and the remove as two syscalls — a racer's successful link can land
	// between them and get DELETED, so both racers end up claiming. That is the
	// original double-winner arriving through the recovery path instead of the
	// write path, and TestClaimActiveTaskRecoversACorruptSlotExactlyOnce catches
	// it; it caught it twice while this function was being written.
	//
	// So the slot is never removed. It is REPLACED, with rename(2), by whichever
	// racer holds the recovery marker — one syscall, no window, and every other
	// racer's link keeps failing with EEXIST until it can read the replacement.
	deadline := time.Now().Add(claimDeadline)
	backoff := time.Millisecond
	for {
		err := os.Link(tmpPath, activeTaskPath())
		if err == nil {
			return t, true, nil
		}
		if !os.IsExist(err) {
			// link(2) is unavailable here, or failed for a reason the fallback
			// will surface as its own error. Never leave the caller with no
			// envelope while telling it the task is open.
			return claimViaExclusiveCreate(t, data)
		}
		if held, ok := readActiveTask(); ok {
			return held, false, nil
		}
		// Corrupt: a truncated file from a crash or an older binary. A claim in
		// flight can no longer look like this, which is the whole point of the
		// link(2) publish above.
		rec, recErr := os.OpenFile(recoverMarkerPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if recErr == nil {
			held, ok := readActiveTask()
			if ok {
				// Someone replaced it while we took the marker.
				rec.Close()
				os.Remove(recoverMarkerPath())
				return held, false, nil
			}
			renameErr := os.Rename(tmpPath, activeTaskPath())
			rec.Close()
			os.Remove(recoverMarkerPath())
			if renameErr != nil {
				return ActiveTask{}, false, renameErr
			}
			return t, true, nil
		}
		// Someone else is recovering — or died holding the marker.
		if info, statErr := os.Stat(recoverMarkerPath()); statErr == nil &&
			time.Since(info.ModTime()) > recoverMarkerStale {
			os.Remove(recoverMarkerPath())
			continue
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(backoff)
		if backoff < claimBackoffMax {
			backoff *= 2
		}
	}
	// Reached only if the slot stayed unreadable for the whole deadline while a
	// live-looking marker kept being refreshed. Failing loudly is correct: the
	// alternative is claiming a slot we could not prove was free, which is the
	// silent orphaning this file exists to prevent.
	return ActiveTask{}, false, fmt.Errorf("envelope slot %s is held by an unreadable pointer", activeTaskPath())
}

func readActiveTask() (ActiveTask, bool) {
	var t ActiveTask
	data, err := os.ReadFile(activeTaskPath())
	if err != nil {
		if os.IsNotExist(err) {
			return adoptLegacyActiveTask()
		}
		return t, false
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return t, false
	}
	return t, t.TaskKey != ""
}

// adoptLegacyActiveTask migrates a pointer written by the pre-PR-#9 binary,
// which keyed the file sha256(repoRoot)[:16]. Without this, upgrading mid-task
// makes the open envelope vanish: hooks stop delivering treatment and the next
// `open` sees a free slot and orphans it — the upgrade would reproduce the bug
// it ships the fix for. The newest legacy pointer wins (they were overwriting
// each other anyway) and the rest are cleared, so this runs at most once.
func adoptLegacyActiveTask() (ActiveTask, bool) {
	entries, err := os.ReadDir(tasksDir())
	if err != nil {
		return ActiveTask{}, false
	}
	var newest ActiveTask
	var found bool
	var stale []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "active.json" {
			continue
		}
		p := filepath.Join(tasksDir(), e.Name())
		stale = append(stale, p)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var t ActiveTask
		if err := json.Unmarshal(data, &t); err != nil || t.TaskKey == "" {
			continue
		}
		if !found || t.OpenedAt > newest.OpenedAt {
			newest, found = t, true
		}
	}
	if !found {
		return ActiveTask{}, false
	}
	if err := writeActiveTask(newest); err != nil {
		return newest, true
	}
	for _, p := range stale {
		_ = os.Remove(p)
	}
	return newest, true
}

func clearActiveTask() { _ = os.Remove(activeTaskPath()) }

// GateState is C2's re-anchor gate for one Claude Code session.
type GateState struct {
	Armed         bool   `json:"armed"`
	TaskKey       string `json:"taskKey"`
	AssignmentID  string `json:"assignmentId"`
	Arm           string `json:"arm"`
	ExperimentKey string `json:"experimentKey"`
	CompactedAt   string `json:"compactedAt"`
	Attempts      int    `json:"attempts"`
}

func gatePath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(gateDir(), hex.EncodeToString(sum[:])[:16]+".json")
}

func readGate(sessionID string) (GateState, bool) {
	var g GateState
	data, err := os.ReadFile(gatePath(sessionID))
	if err != nil {
		return g, false
	}
	if err := json.Unmarshal(data, &g); err != nil {
		return g, false
	}
	return g, true
}

func writeGate(sessionID string, g GateState) error {
	if err := os.MkdirAll(gateDir(), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return os.WriteFile(gatePath(sessionID), data, 0o600)
}

// saveRejectedPrompt preserves a prompt the gate refused. Claude Code erases a
// blocked prompt; losing the engineer's typing to our experiment would tank
// adherence for a reason that has nothing to do with the practice.
func saveRejectedPrompt(sessionID, prompt string) string {
	if err := os.MkdirAll(rejectedDir(), 0o700); err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s.txt", time.Now().UTC().Format("20060102T150405.000"), shortHash(sessionID))
	p := filepath.Join(rejectedDir(), name)
	if err := os.WriteFile(p, []byte(prompt), 0o600); err != nil {
		return ""
	}
	return p
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// repoRootOf returns the git worktree root containing dir (worktree-specific).
func repoRootOf(dir string) string {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return dir
	}
	return out
}

// repoSlugOf returns the owner/name repository identity the backend contract
// expects ("pa-arth/promptster-backend"). It is the SAME for every worktree of
// a repo — the stratification variable is "which repo", not "which worktree" —
// so it comes from the remote URL, falling back to the primary .git directory's
// name (git-common-dir points there even from inside a worktree).
func repoSlugOf(dir string) string {
	if url, err := runGit(dir, "remote", "get-url", "origin"); err == nil {
		if slug := repoSlugFromURL(url); slug != "" {
			return slug
		}
	}
	out, err := runGit(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return filepath.Base(dir)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.Base(filepath.Dir(filepath.Clean(out)))
}

// repoSlugFromURL pulls owner/name out of an ssh or https remote.
func repoSlugFromURL(url string) string {
	url = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(url), ".git"))
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if at := strings.Index(url, "@"); at >= 0 {
			url = url[at+1:]
		}
	} else if at := strings.Index(url, "@"); at >= 0 {
		url = url[at+1:] // git@github.com:owner/name
	}
	url = strings.ReplaceAll(url, ":", "/")
	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readJSONL streams a JSONL file line by line. A record that will not parse is
// the caller's to skip: a corrupt line must never abort a read of the log, or
// one bad row would hide every good one behind it.
func readJSONL(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}
