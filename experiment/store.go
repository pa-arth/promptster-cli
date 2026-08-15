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

func writeActiveTask(t ActiveTask) error {
	if err := os.MkdirAll(tasksDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(activeTaskPath(), append(data, '\n'), 0o600)
}

func readActiveTask() (ActiveTask, bool) {
	var t ActiveTask
	data, err := os.ReadFile(activeTaskPath())
	if err != nil {
		return t, false
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return t, false
	}
	return t, t.TaskKey != ""
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
