package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Syncing the local append-only log to the backend assignment log
// (openspec practice-effect-experiment, the half of task 0.1 deferred until
// 0.3's routes existed — they do now, backend PR #699).
//
// WHY THIS IS NOT OPTIONAL: until a row reaches the server, the ONLY copy of
// batch 1's assignment log is one JSONL file on one laptop. A pre-registered
// trial whose assignment record can be lost with a disk is not a trial. Sync is
// what makes "assignment preceded the work" survive this machine.
//
// TWO RULES SHAPE EVERY DECISION BELOW.
//
//  1. The local log is append-only and IMMUTABLE, so sync state cannot be a flag
//     flipped on a row. It lives in its own append-only receipt log, and "synced"
//     is derived by reading it. Nothing here ever rewrites an assignment.
//  2. One authority per row (design.md). The server stores a `cli-offline` draw
//     VERBATIM and never recomputes it; if the two sides disagree about what the
//     engineer was shown, that is surfaced loudly and left for a human. This code
//     has no reconcile path, on purpose — two allocators arbitrating one row is
//     how an experiment gets silently corrupted.

const defaultAPIBase = "https://api.promptster.ai"

func apiBase() string {
	if u := strings.TrimRight(os.Getenv("PROMPTSTER_API_URL"), "/"); u != "" {
		return u
	}
	return defaultAPIBase
}

// engineerKey resolves the PSE- engineer key. Flag beats env beats config, so a
// one-off run can target another identity without rewriting stored state.
//
// It refuses a PSO- org key rather than letting the route 401: an org capture
// token authenticates a MACHINE and carries no userId, and an assignment belongs
// to a person. Naming that here costs one string compare and saves reading a
// generic auth failure.
func engineerKey(flagKey string, cfg Config) (string, error) {
	key := strings.TrimSpace(flagKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("PROMPTSTER_ENGINEER_KEY"))
	}
	if key == "" {
		key = strings.TrimSpace(cfg.EngineerKey)
	}
	if key == "" {
		return "", fmt.Errorf("no engineer key: pass --key, set PROMPTSTER_ENGINEER_KEY, or store one with `init --key PSE-...`")
	}
	if strings.HasPrefix(key, "PSO-") {
		return "", fmt.Errorf("that is an ORG capture key (PSO-). It authenticates a machine and carries no engineer identity, so it cannot own an assignment. Use the engineer key (PSE-)")
	}
	if !strings.HasPrefix(key, "PSE-") {
		return "", fmt.Errorf("engineer key should start with PSE-")
	}
	return key, nil
}

var syncClient = &http.Client{Timeout: 20 * time.Second}

func postJSON(base, key, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	req.Header.Set("X-Promptster-CLI-Version", toolVersion)
	resp, err := syncClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes(), nil
}

// SyncReceipt records one POST and its answer. Its own append-only log, because
// the assignment log it describes may never be rewritten.
type SyncReceipt struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"` // assignment | adherence
	SyncedAt      string `json:"syncedAt"`
	ExperimentKey string `json:"experimentKey"`
	TaskKey       string `json:"taskKey"`
	// DedupeKey identifies the exact fact posted. The adherence route is
	// append-only with no idempotency key of its own, so double-posting would
	// double-count adherence — the thing the batch-2 gate is read off. This is
	// what stops a second `sync` from doing that.
	DedupeKey  string `json:"dedupeKey"`
	HTTPStatus int    `json:"httpStatus"`
	// Result: stored | already_assigned | conflict | refused | error.
	// Only stored/already_assigned are terminal-success; conflict and refused are
	// terminal FAILURES that a retry cannot fix and a human must read; error is
	// transient and retried on the next run.
	Result       string `json:"result"`
	AssignmentID string `json:"assignmentId,omitempty"`
	ServerArm    string `json:"serverArm,omitempty"`
	Code         string `json:"code,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

func syncReceiptsPath() string { return filepath.Join(rootDir(), "sync-receipts.jsonl") }

func readReceipts() ([]SyncReceipt, error) {
	var out []SyncReceipt
	err := readJSONL(syncReceiptsPath(), func(line []byte) error {
		var r SyncReceipt
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func (r SyncReceipt) settled() bool {
	switch r.Result {
	case "stored", "already_assigned", "conflict", "refused":
		return true
	}
	return false
}

// syncState folds the receipt log into the two questions sync asks: is this fact
// already settled, and what assignmentId did the server give this task?
type syncState struct {
	settled     map[string]bool
	assignments map[string]string
	conflicts   []SyncReceipt
}

func foldReceipts(rs []SyncReceipt) syncState {
	st := syncState{settled: map[string]bool{}, assignments: map[string]string{}}
	for _, r := range rs {
		if r.settled() {
			st.settled[r.Kind+"|"+r.DedupeKey] = true
		}
		if r.Kind == "assignment" && r.AssignmentID != "" {
			st.assignments[r.TaskKey] = r.AssignmentID
		}
		if r.Result == "conflict" {
			st.conflicts = append(st.conflicts, r)
		}
	}
	return st
}

// serverAssignment is the 200 body of POST /v1/teams/experiments/assignment.
type serverAssignment struct {
	AssignmentID    string `json:"assignmentId"`
	Arm             string `json:"arm"`
	Stratum         string `json:"stratum"`
	StratumPosition int    `json:"stratumPosition"`
	AssignedAt      string `json:"assignedAt"`
	Source          string `json:"assignmentSource"`
	AlreadyAssigned bool   `json:"alreadyAssigned"`
}

type serverError struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Stored struct {
		Arm             string `json:"arm"`
		AssignedAt      string `json:"assignedAt"`
		Source          string `json:"assignmentSource"`
		StratumPosition int    `json:"stratumPosition"`
	} `json:"stored"`
}

func cmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	key := fs.String("key", "", "engineer key (PSE-...); default: $PROMPTSTER_ENGINEER_KEY or config")
	dry := fs.Bool("dry-run", false, "show what would be posted, post nothing")
	only := fs.String("only", "", "assignments|adherence (default: both)")
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	rows, err := readAssignments()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading assignment log: %v\n", err)
		return 1
	}
	receipts, err := readReceipts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading sync receipts: %v\n", err)
		return 1
	}
	st := foldReceipts(receipts)

	var apiKey string
	if !*dry {
		apiKey, err = engineerKey(*key, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
	}

	doAssign := *only == "" || *only == "assignments"
	doAdhere := *only == "" || *only == "adherence"
	if *only != "" && !doAssign && !doAdhere {
		fmt.Fprintf(os.Stderr, "error: --only must be assignments or adherence\n")
		return 2
	}

	fmt.Printf("sync → %s  (experiment %s, org %s)\n", apiBase(), cfg.ExperimentKey, cfg.OrgID)
	failed := 0

	if doAssign {
		for _, r := range rows {
			if r.OrgID != cfg.OrgID || r.ExperimentKey != cfg.ExperimentKey {
				continue
			}
			// An excluded task never asked for an arm and must not consume a
			// block slot on the server either.
			if !r.Eligible {
				continue
			}
			if st.settled["assignment|"+r.TaskKey] {
				continue
			}
			if *dry {
				fmt.Printf("  would POST assignment  %s  arm=%s pos=%d\n", r.TaskKey, r.Arm, r.StratumPosition)
				continue
			}
			rec := postAssignment(apiBase(), apiKey, r)
			if err := appendJSONL(syncReceiptsPath(), rec); err != nil {
				fmt.Fprintf(os.Stderr, "error: writing sync receipt: %v\n", err)
				return 1
			}
			if rec.AssignmentID != "" {
				st.assignments[r.TaskKey] = rec.AssignmentID
			}
			if rec.Result == "stored" || rec.Result == "already_assigned" {
				st.settled["assignment|"+r.TaskKey] = true
				fmt.Printf("  assignment %-46s %s (%s)\n", r.TaskKey, rec.Result, rec.ServerArm)
			} else {
				failed++
				fmt.Printf("  assignment %-46s %s [%s] %s\n", r.TaskKey, rec.Result, rec.Code, rec.Detail)
			}
			if rec.Result == "conflict" {
				st.conflicts = append(st.conflicts, rec)
			}
		}
	}

	if doAdhere {
		obs, err := deriveAdherence(cfg, rows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: deriving adherence: %v\n", err)
			return 1
		}
		for _, o := range obs {
			if st.settled["adherence|"+o.DedupeKey] {
				continue
			}
			// Adherence hangs off an assignment the server knows. Posting an
			// observation for a task whose assignment never landed would 404, and
			// retrying it every run is noise, so it waits for its assignment.
			id := st.assignments[o.TaskKey]
			if *dry {
				after := ""
				if id == "" {
					after = "  (after its assignment lands)"
				}
				fmt.Printf("  would POST adherence   %s  %s=%s%s\n", o.TaskKey, o.ComplianceEvent, o.Observed, after)
				continue
			}
			if id == "" {
				fmt.Printf("  adherence  %-46s waiting (assignment not synced)\n", o.TaskKey)
				continue
			}
			rec := postAdherence(apiBase(), apiKey, cfg, id, o)
			if err := appendJSONL(syncReceiptsPath(), rec); err != nil {
				fmt.Fprintf(os.Stderr, "error: writing sync receipt: %v\n", err)
				return 1
			}
			if rec.Result == "stored" {
				st.settled["adherence|"+o.DedupeKey] = true
				fmt.Printf("  adherence  %-46s %s=%s\n", o.TaskKey, o.ComplianceEvent, o.Observed)
			} else {
				failed++
				fmt.Printf("  adherence  %-46s %s [%s] %s\n", o.TaskKey, rec.Result, rec.Code, rec.Detail)
			}
		}
	}

	if len(st.conflicts) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s\n", strings.Repeat("=", 72))
		fmt.Fprintf(os.Stderr, "ARM CONFLICT — the server's log and this machine's log disagree about\n"+
			"what the engineer was SHOWN. Nothing was written and nothing will be\n"+
			"retried: one authority per row, and a reconcile here would silently\n"+
			"corrupt the batch. Read both rows and decide by hand.\n")
		for _, c := range st.conflicts {
			fmt.Fprintf(os.Stderr, "  %s: local=? server=%s (%s)\n", c.TaskKey, c.ServerArm, c.Detail)
		}
		fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("=", 72))
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d row(s) did not sync.\n", failed)
		return 1
	}
	if !*dry {
		fmt.Println("all rows synced.")
	}
	return 0
}

func postAssignment(base, key string, r Assignment) SyncReceipt {
	rec := SyncReceipt{
		SchemaVersion: schemaVersion, Kind: "assignment", SyncedAt: nowUTC(),
		ExperimentKey: r.ExperimentKey, TaskKey: r.TaskKey, DedupeKey: r.TaskKey,
	}
	status, body, err := postJSON(base, key, "/v1/teams/experiments/assignment", r.syncPayload())
	rec.HTTPStatus = status
	if err != nil {
		rec.Result, rec.Detail = "error", truncate(err.Error(), 300)
		return rec
	}
	if status == 200 {
		var ok serverAssignment
		if json.Unmarshal(body, &ok) != nil {
			rec.Result, rec.Detail = "error", "unparseable 200 body"
			return rec
		}
		rec.AssignmentID, rec.ServerArm = ok.AssignmentID, ok.Arm
		rec.Result = "stored"
		if ok.AlreadyAssigned {
			rec.Result = "already_assigned"
		}
		// The server stores a cli-offline draw verbatim, so a 200 whose arm
		// differs from the local row is not possible by contract — if it ever
		// happens, it is a contract break and must not pass silently.
		if ok.Arm != r.Arm {
			rec.Result, rec.Code = "conflict", "arm_mismatch_on_200"
			rec.Detail = fmt.Sprintf("local %s, server returned %s", r.Arm, ok.Arm)
		}
		return rec
	}
	var e serverError
	_ = json.Unmarshal(body, &e)
	rec.Code = e.Code
	rec.Detail = truncate(e.Error, 300)
	switch {
	case e.Code == "arm_conflict":
		rec.Result, rec.ServerArm = "conflict", e.Stored.Arm
		rec.Detail = fmt.Sprintf("local %s @ pos %d; server %s @ pos %d (%s)",
			r.Arm, r.StratumPosition, e.Stored.Arm, e.Stored.StratumPosition, e.Stored.Source)
	case status >= 400 && status < 500:
		// 4xx is a decision, not a hiccup: slot_taken, offline_draw_not_allowed,
		// unknown_experiment, experiment_closed, a rejected body. Retrying with the
		// same row cannot change any of them, and mutating the row to get a
		// different answer is exactly what must never happen.
		rec.Result = "refused"
		if rec.Detail == "" {
			rec.Detail = truncate(string(body), 300)
		}
	default:
		rec.Result = "error"
		if rec.Detail == "" {
			rec.Detail = fmt.Sprintf("HTTP %d", status)
		}
	}
	return rec
}

func postAdherence(base, key string, cfg Config, assignmentID string, o adherenceObs) SyncReceipt {
	rec := SyncReceipt{
		SchemaVersion: schemaVersion, Kind: "adherence", SyncedAt: nowUTC(),
		ExperimentKey: cfg.ExperimentKey, TaskKey: o.TaskKey, DedupeKey: o.DedupeKey,
	}
	payload := map[string]any{
		"assignmentId":    assignmentID,
		"complianceEvent": o.ComplianceEvent,
		"observed":        o.Observed,
		"observedAt":      o.ObservedAt,
		"source":          "cli-hook",
		"detail":          o.Detail,
	}
	status, body, err := postJSON(base, key, "/v1/teams/experiments/adherence", payload)
	rec.HTTPStatus = status
	if err != nil {
		rec.Result, rec.Detail = "error", truncate(err.Error(), 300)
		return rec
	}
	if status == 200 || status == 201 {
		rec.Result = "stored"
		return rec
	}
	var e serverError
	_ = json.Unmarshal(body, &e)
	rec.Code, rec.Detail = e.Code, truncate(e.Error, 300)
	if status >= 400 && status < 500 {
		rec.Result = "refused"
		if rec.Detail == "" {
			rec.Detail = truncate(string(body), 300)
		}
		return rec
	}
	rec.Result = "error"
	if rec.Detail == "" {
		rec.Detail = fmt.Sprintf("HTTP %d", status)
	}
	return rec
}

// adherenceObs is one adherence claim, derived from the local hook events.
type adherenceObs struct {
	TaskKey         string
	ComplianceEvent string
	Observed        string // followed | violated | unknown
	ObservedAt      string
	Detail          map[string]any
	DedupeKey       string
}

// deriveAdherence turns raw hook events into the adherence claims the backend
// registry declares. It is deliberately narrow, and the exclusions matter more
// than the inclusions:
//
//   - ONE CLAIM PER ARMED GATE, not per event. A gate that armed and was answered
//     is one observation. Posting the raw stream instead would count the three
//     rejected drafts in batch 1's first task as three violations, when the
//     engineer in fact anchored and continued — adherence would read 40% for
//     behaviour that was 100% compliant. `reanchor_rejected` is a keystroke, not
//     a verdict, so it is never posted.
//   - AN UNANSWERED GATE ON A CLOSED TASK IS `unknown`, NOT SILENCE. Dropping it
//     would quietly inflate adherence, which is the number the batch-2 gate reads.
//     While the task is still open it is posted at all, because it can still be
//     answered.
//   - C1 IS NOT DERIVED HERE. `zero_topic_pivots` is hand-read on every C1-arm
//     task by pre-registration (regexes misfire on long machine notifications), so
//     the CLI has no business asserting it. It reaches the server from the audit
//     pass with source `hand-audit`, not from this code.
func deriveAdherence(cfg Config, rows []Assignment) ([]adherenceObs, error) {
	var events []Event
	err := readJSONL(eventsPath(), func(line []byte) error {
		var e Event
		if json.Unmarshal(line, &e) == nil {
			events = append(events, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	eligible := map[string]bool{}
	for _, r := range rows {
		if r.OrgID == cfg.OrgID && r.ExperimentKey == cfg.ExperimentKey && r.Eligible {
			eligible[r.TaskKey] = true
		}
	}

	closedAt := map[string]string{}
	for _, e := range events {
		if e.ComplianceEvent == "task_close" && closedAt[e.TaskKey] == "" {
			closedAt[e.TaskKey] = e.ObservedAt
		}
	}

	var out []adherenceObs
	for i, g := range events {
		if g.ComplianceEvent != "gate_armed" || !eligible[g.TaskKey] {
			continue
		}
		obs := adherenceObs{
			TaskKey:         g.TaskKey,
			ComplianceEvent: "anchor_after_compact",
			DedupeKey:       g.TaskKey + "|anchor_after_compact|" + g.ObservedAt,
			Detail:          map[string]any{"armedAt": g.ObservedAt, "sessionId": g.SessionID},
		}
		resolved := false
		for _, e := range events[i+1:] {
			if e.TaskKey != g.TaskKey || e.SessionID != g.SessionID {
				continue
			}
			if e.ComplianceEvent == "gate_armed" {
				break // a later compaction re-armed; this gate's window closed
			}
			switch e.ComplianceEvent {
			case "reanchor_accepted":
				obs.Observed, obs.ObservedAt, resolved = "followed", e.ObservedAt, true
				obs.Detail["anchorChars"] = e.CharLen
				obs.Detail["attempts"] = e.Attempt
			case "gate_bypassed":
				obs.Observed, obs.ObservedAt, resolved = "violated", e.ObservedAt, true
				obs.Detail["bypass"] = true
			}
			if resolved {
				break
			}
		}
		if !resolved {
			if closedAt[g.TaskKey] == "" {
				continue // still open, may still be answered
			}
			obs.Observed, obs.ObservedAt = "unknown", closedAt[g.TaskKey]
			obs.Detail["unanswered"] = true
		}
		out = append(out, obs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DedupeKey < out[j].DedupeKey })
	return out, nil
}
