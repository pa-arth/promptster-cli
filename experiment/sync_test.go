package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func syncTestCfg() Config {
	return Config{
		OrgID:         "org_test",
		EngineerID:    "eng@example.com",
		ExperimentKey: "batch1-context-mechanics",
		Enabled:       true,
	}
}

func row(taskKey, arm string, eligible bool) Assignment {
	cfg := syncTestCfg()
	return Assignment{
		SchemaVersion: schemaVersion, ExperimentKey: cfg.ExperimentKey, TaskKey: taskKey,
		OrgID: cfg.OrgID, EngineerID: cfg.EngineerID, Arm: arm, Eligible: eligible,
		AssignedAt: "2026-08-12T17:00:00Z", AssignmentSource: "cli-offline",
	}
}

func TestEngineerKeyResolution(t *testing.T) {
	cfg := syncTestCfg()
	cfg.EngineerKey = "PSE-from-config"

	t.Run("flag beats env beats config", func(t *testing.T) {
		t.Setenv("PROMPTSTER_ENGINEER_KEY", "PSE-from-env")
		if got, _ := engineerKey("PSE-from-flag", cfg); got != "PSE-from-flag" {
			t.Fatalf("flag should win, got %q", got)
		}
		if got, _ := engineerKey("", cfg); got != "PSE-from-env" {
			t.Fatalf("env should beat config, got %q", got)
		}
	})

	t.Run("config is the fallback", func(t *testing.T) {
		t.Setenv("PROMPTSTER_ENGINEER_KEY", "")
		if got, _ := engineerKey("", cfg); got != "PSE-from-config" {
			t.Fatalf("config fallback, got %q", got)
		}
		if _, err := engineerKey("", syncTestCfg()); err == nil {
			t.Fatal("no key anywhere must be an error, not an unauthenticated POST")
		}
	})

	// An org capture key authenticates a machine and carries no userId, so it can
	// never own an assignment. Named here rather than left to a generic 401.
	t.Run("an org key is refused by name", func(t *testing.T) {
		t.Setenv("PROMPTSTER_ENGINEER_KEY", "")
		_, err := engineerKey("PSO-machine", syncTestCfg())
		if err == nil || !strings.Contains(err.Error(), "PSE-") {
			t.Fatalf("a PSO- key must be refused and told what to use instead, got %v", err)
		}
	})
}

// TestDeriveAdherenceCountsGatesNotKeystrokes is the test that matters most in
// this file. Adherence is what the batch-2 gate is read off, so a derivation
// that miscounts does not fail loudly — it produces a plausible wrong number.
func TestDeriveAdherenceCountsGatesNotKeystrokes(t *testing.T) {
	t.Setenv("PROMPTSTER_EXPERIMENT_DIR", t.TempDir())
	cfg := syncTestCfg()
	rows := []Assignment{
		row("repo/anchored", "c1off_c2on", true),
		row("repo/bypassed", "c1off_c2on", true),
		row("repo/abandoned", "c1off_c2on", true),
		row("repo/still-open", "c1off_c2on", true),
		row("repo/excluded", "c1off_c2on", false),
	}

	ev := func(kind, task, session, at string, extra func(*Event)) {
		e := Event{
			SchemaVersion: schemaVersion, ComplianceEvent: kind, Source: "cli-hook",
			ExperimentKey: cfg.ExperimentKey, TaskKey: task, OrgID: cfg.OrgID,
			EngineerID: cfg.EngineerID, SessionID: session, ObservedAt: at,
		}
		if extra != nil {
			extra(&e)
		}
		if err := appendJSONL(eventsPath(), e); err != nil {
			t.Fatal(err)
		}
	}

	// The real shape of batch 1's first task: the engineer's first two drafts were
	// too short, the third landed. That is ONE compliant gate, not three violations.
	ev("gate_armed", "repo/anchored", "s1", "2026-08-12T10:00:00Z", nil)
	ev("reanchor_rejected", "repo/anchored", "s1", "2026-08-12T10:01:00Z", nil)
	ev("reanchor_rejected", "repo/anchored", "s1", "2026-08-12T10:02:00Z", nil)
	ev("reanchor_accepted", "repo/anchored", "s1", "2026-08-12T10:03:00Z", func(e *Event) {
		e.CharLen = 240
		e.Attempt = 3
	})
	// A second compaction in the same session arms a second, independent gate.
	ev("gate_armed", "repo/anchored", "s1", "2026-08-12T11:00:00Z", nil)
	ev("reanchor_accepted", "repo/anchored", "s1", "2026-08-12T11:01:00Z", func(e *Event) { e.CharLen = 300 })

	ev("gate_armed", "repo/bypassed", "s2", "2026-08-12T10:00:00Z", nil)
	ev("gate_bypassed", "repo/bypassed", "s2", "2026-08-12T10:05:00Z", nil)

	// Armed, never answered, task closed: an unanswered gate is `unknown`, never
	// silence — dropping it would quietly inflate adherence.
	ev("gate_armed", "repo/abandoned", "s3", "2026-08-12T10:00:00Z", nil)
	ev("task_close", "repo/abandoned", "s3", "2026-08-12T12:00:00Z", nil)

	// Armed, unanswered, task still open: it can still be answered, so nothing is
	// claimed about it yet.
	ev("gate_armed", "repo/still-open", "s4", "2026-08-12T10:00:00Z", nil)

	// An excluded task is not in the experiment at all.
	ev("gate_armed", "repo/excluded", "s5", "2026-08-12T10:00:00Z", nil)
	ev("gate_bypassed", "repo/excluded", "s5", "2026-08-12T10:01:00Z", nil)

	obs, err := deriveAdherence(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, o := range obs {
		if o.ComplianceEvent != "anchor_after_compact" {
			t.Fatalf("only the declared c2 event may be posted, got %q", o.ComplianceEvent)
		}
		got[o.DedupeKey] = o.Observed
	}
	k := func(task, armedAt string) string {
		return adherenceDedupeKey(cfg, task, "anchor_after_compact", armedAt)
	}
	want := map[string]string{
		k("repo/anchored", "2026-08-12T10:00:00Z"):  "followed",
		k("repo/anchored", "2026-08-12T11:00:00Z"):  "followed",
		k("repo/bypassed", "2026-08-12T10:00:00Z"):  "violated",
		k("repo/abandoned", "2026-08-12T10:00:00Z"): "unknown",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d observations, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q, want %q", k, got[k], v)
		}
	}

	// C1 adherence is hand-read by pre-registration; the CLI must never assert it.
	for _, o := range obs {
		if o.ComplianceEvent == "zero_topic_pivots" {
			t.Fatal("c1 adherence must come from the hand audit, not from the CLI")
		}
	}
}

func TestSyncReceiptsPreventDoubleCounting(t *testing.T) {
	cfg := syncTestCfg()
	id := identity(cfg.ExperimentKey, cfg.OrgID, "repo/a")
	st := foldReceipts([]SyncReceipt{
		{Kind: "assignment", ExperimentKey: cfg.ExperimentKey, OrgID: cfg.OrgID, TaskKey: "repo/a",
			DedupeKey: id, Result: "stored", AssignmentID: "uuid-a"},
		{Kind: "adherence", ExperimentKey: cfg.ExperimentKey, OrgID: cfg.OrgID, TaskKey: "repo/a",
			DedupeKey: id + "|anchor_after_compact|T1", Result: "stored"},
		{Kind: "adherence", ExperimentKey: cfg.ExperimentKey, OrgID: cfg.OrgID, TaskKey: "repo/a",
			DedupeKey: id + "|anchor_after_compact|T2", Result: "error"},
	})
	if !st.settled["assignment|"+id] || st.assignments[id] != "uuid-a" {
		t.Fatal("a stored assignment must be settled and carry its id forward")
	}
	if !st.settled["adherence|"+id+"|anchor_after_compact|T1"] {
		t.Fatal("a stored adherence row must never be posted twice — the route has no idempotency key")
	}
	if st.settled["adherence|"+id+"|anchor_after_compact|T2"] {
		t.Fatal("a transient error must be retried, not treated as settled")
	}
}

// TestSyncStateIsScopedToTheAssignmentIdentity: the state directory outlives the
// experiment. Batch 2 runs under a different experimentKey in the same
// ~/.promptster-experiment, and a task key may legitimately repeat. Keyed on the
// task alone, batch 1's receipt marks batch 2's row settled and that row never
// syncs — silently, with `status` reporting success.
func TestSyncStateIsScopedToTheAssignmentIdentity(t *testing.T) {
	old := SyncReceipt{
		Kind: "assignment", ExperimentKey: "batch1-context-mechanics", OrgID: "org_test",
		TaskKey: "repo/shared-name", Result: "stored", AssignmentID: "uuid-batch1",
	}
	old.DedupeKey = identity(old.ExperimentKey, old.OrgID, old.TaskKey)
	st := foldReceipts([]SyncReceipt{old})

	next := row("repo/shared-name", "c1on_c2off", true)
	next.ExperimentKey = "batch2-encouragement"
	if st.settled["assignment|"+assignmentDedupeKey(next)] {
		t.Fatal("a receipt from another experiment must not settle this one's row")
	}
	if st.assignments[identity(next.ExperimentKey, next.OrgID, next.TaskKey)] == "uuid-batch1" {
		t.Fatal("adherence must never hang off another experiment's assignmentId")
	}

	sameOrg := row("repo/shared-name", "c1on_c2off", true)
	sameOrg.OrgID = "org_other"
	if st.settled["assignment|"+assignmentDedupeKey(sameOrg)] {
		t.Fatal("a receipt from another org must not settle this one's row")
	}
	// The row it really does describe is still settled.
	same := row("repo/shared-name", "c1on_c2off", true)
	if !st.settled["assignment|"+assignmentDedupeKey(same)] {
		t.Fatal("the matching row must still be recognised as synced")
	}
}

func TestPostAssignmentClassifiesServerAnswers(t *testing.T) {
	local := row("repo/a", "c1on_c2on", true)

	cases := []struct {
		name       string
		status     int
		body       string
		wantResult string
		wantCode   string
	}{
		{"stored", 200, `{"assignmentId":"uuid-1","arm":"c1on_c2on","alreadyAssigned":false}`, "stored", ""},
		{"idempotent re-post", 200, `{"assignmentId":"uuid-1","arm":"c1on_c2on","alreadyAssigned":true}`, "already_assigned", ""},
		// The server stores a cli-offline draw verbatim, so a 200 carrying a
		// different arm is a contract break, not a success.
		{"200 with the wrong arm", 200, `{"assignmentId":"uuid-1","arm":"c1off_c2off"}`, "conflict", "arm_mismatch_on_200"},
		{"arm conflict", 409, `{"error":"already assigned","code":"arm_conflict","stored":{"arm":"c1off_c2off","stratumPosition":2}}`, "conflict", "arm_conflict"},
		{"slot taken", 409, `{"error":"slot taken","code":"slot_taken"}`, "refused", "slot_taken"},
		{"offline draw not allowed", 403, `{"error":"server draw required","code":"offline_draw_not_allowed"}`, "refused", "offline_draw_not_allowed"},
		// 5xx is the only retryable class: the row is fine, the server is not.
		{"server error retries", 503, `{"error":"teams not configured","code":"teams_not_configured"}`, "error", "teams_not_configured"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "PSE-test" {
					t.Errorf("engineer key must be sent as X-API-Key, got %q", r.Header.Get("X-API-Key"))
				}
				if r.URL.Path != "/v1/teams/experiments/assignment" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				var sent map[string]any
				raw, _ := io.ReadAll(r.Body)
				if json.Unmarshal(raw, &sent) != nil {
					t.Error("body must be JSON")
				}
				// The offline contract: the server needs all three of these to
				// store the draw the engineer was actually shown.
				for _, f := range []string{"assignmentSource", "arm", "stratumPosition", "assignedAt"} {
					if _, ok := sent[f]; !ok {
						t.Errorf("payload is missing %q", f)
					}
				}
				if _, leaked := sent["envelope"]; leaked {
					t.Error("the local envelope (raw repo paths) must never be sent")
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			rec := postAssignment(srv.URL, "PSE-test", local)
			if rec.Result != c.wantResult {
				t.Fatalf("result %q, want %q (detail: %s)", rec.Result, c.wantResult, rec.Detail)
			}
			if c.wantCode != "" && rec.Code != c.wantCode {
				t.Fatalf("code %q, want %q", rec.Code, c.wantCode)
			}
			if c.wantResult == "error" && rec.settled() {
				t.Fatal("a transient error must stay unsettled so the next run retries it")
			}
			if c.wantResult == "conflict" && !rec.settled() {
				t.Fatal("a conflict must be terminal — retrying cannot resolve a disagreement about what was shown")
			}
		})
	}
}

func TestPostAdherenceSendsTheDeclaredShape(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	obs := adherenceObs{
		TaskKey: "repo/a", ComplianceEvent: "anchor_after_compact", Observed: "followed",
		ObservedAt: "2026-08-12T10:03:00Z", Detail: map[string]any{"anchorChars": 240},
		DedupeKey: "repo/a|anchor_after_compact|2026-08-12T10:00:00Z",
	}
	rec := postAdherence(srv.URL, "PSE-test", syncTestCfg(), "uuid-1", obs)
	if rec.Result != "stored" {
		t.Fatalf("result %q", rec.Result)
	}
	// The route pins the assignment by id and rejects an undeclared compliance
	// event with a 400, so both fields have to be exactly right on the wire.
	if sent["assignmentId"] != "uuid-1" || sent["complianceEvent"] != "anchor_after_compact" {
		t.Fatalf("wrong wire shape: %v", sent)
	}
	if sent["observed"] != "followed" || sent["source"] != "cli-hook" {
		t.Fatalf("wrong wire shape: %v", sent)
	}
	if _, ok := sent["detail"].(map[string]any); !ok {
		t.Fatalf("detail must be an object, the route's schema rejects a string: %v", sent["detail"])
	}
}

// TestDeriveAdherenceIgnoresForeignEvents: the event log outlives the experiment
// too. A task key can repeat across experiments or orgs, and a foreign accept,
// bypass or close matched on task key alone would write another experiment's
// behaviour into this batch's adherence — permanently, since the table is
// append-only and adherence is what the batch-2 gate divides by.
func TestDeriveAdherenceIgnoresForeignEvents(t *testing.T) {
	t.Setenv("PROMPTSTER_EXPERIMENT_DIR", t.TempDir())
	cfg := syncTestCfg()
	rows := []Assignment{row("repo/shared-name", "c1off_c2on", true)}

	ev := func(kind, experimentKey, orgID, at string) {
		if err := appendJSONL(eventsPath(), Event{
			SchemaVersion: schemaVersion, ComplianceEvent: kind, Source: "cli-hook",
			ExperimentKey: experimentKey, OrgID: orgID, TaskKey: "repo/shared-name",
			EngineerID: cfg.EngineerID, SessionID: "s1", ObservedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// This experiment's gate, still unanswered and its task still open.
	ev("gate_armed", cfg.ExperimentKey, cfg.OrgID, "2026-08-12T10:00:00Z")
	// Another experiment's bypass and close on the same task key and session.
	ev("gate_bypassed", "batch2-encouragement", cfg.OrgID, "2026-08-12T10:05:00Z")
	ev("task_close", "batch2-encouragement", cfg.OrgID, "2026-08-12T11:00:00Z")
	// Another org's, likewise.
	ev("gate_bypassed", cfg.ExperimentKey, "org_other", "2026-08-12T10:06:00Z")

	obs, err := deriveAdherence(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Fatalf("a foreign bypass/close must not resolve this gate, got %+v", obs)
	}

	// The same gate, once THIS experiment closes the task, is `unknown` — not the
	// violation the foreign bypass would have made it.
	ev("task_close", cfg.ExperimentKey, cfg.OrgID, "2026-08-12T12:00:00Z")
	obs, err = deriveAdherence(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Observed != "unknown" {
		t.Fatalf("want one unknown, got %+v", obs)
	}
	if obs[0].ObservedAt != "2026-08-12T12:00:00Z" {
		t.Fatalf("must be stamped with this experiment's close, got %s", obs[0].ObservedAt)
	}
}

// TestDeriveAdherenceHandlesCloseThenReopen: a task can be closed and reopened.
// A gate armed after the reopen is answerable no matter how old the first close
// is; resolving it against that stale close posts `unknown` for a live gate and
// stamps it with a time from before the gate existed.
func TestDeriveAdherenceHandlesCloseThenReopen(t *testing.T) {
	t.Setenv("PROMPTSTER_EXPERIMENT_DIR", t.TempDir())
	cfg := syncTestCfg()
	rows := []Assignment{row("repo/reopened", "c1off_c2on", true)}

	ev := func(kind, session, at string) {
		if err := appendJSONL(eventsPath(), Event{
			SchemaVersion: schemaVersion, ComplianceEvent: kind, Source: "cli-hook",
			ExperimentKey: cfg.ExperimentKey, OrgID: cfg.OrgID, TaskKey: "repo/reopened",
			EngineerID: cfg.EngineerID, SessionID: session, ObservedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ev("gate_armed", "s1", "2026-08-12T10:00:00Z")
	ev("reanchor_accepted", "s1", "2026-08-12T10:01:00Z")
	ev("task_close", "s1", "2026-08-12T11:00:00Z")
	// Reopened the next day; a new session compacts and arms a new gate.
	ev("task_open", "s2", "2026-08-13T09:00:00Z")
	ev("gate_armed", "s2", "2026-08-13T09:30:00Z")

	obs, err := deriveAdherence(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Observed != "followed" {
		t.Fatalf("the live post-reopen gate must not be closed out by yesterday's close, got %+v", obs)
	}

	// Once the reopened task closes, the second gate resolves against THAT close.
	ev("task_close", "s2", "2026-08-13T12:00:00Z")
	obs, err = deriveAdherence(cfg, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("want both gates, got %+v", obs)
	}
	var second adherenceObs
	for _, o := range obs {
		if o.Detail["armedAt"] == "2026-08-13T09:30:00Z" {
			second = o
		}
	}
	if second.Observed != "unknown" || second.ObservedAt != "2026-08-13T12:00:00Z" {
		t.Fatalf("the second gate must be unknown at the SECOND close, got %+v", second)
	}
}
