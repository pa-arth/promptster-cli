package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// hookPayload is the union of the Claude Code hook inputs we register for.
// Field names follow the documented schema; `prompt` is accepted alongside
// `user_input` because older Claude Code builds send the former and a gate that
// silently stops seeing prompts would look like perfect adherence.
type hookPayload struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	EventName string `json:"hook_event_name"`
	Source    string `json:"source"`  // SessionStart: startup|resume|clear|compact|fork
	Trigger   string `json:"trigger"` // PreCompact: manual|auto
	UserInput string `json:"user_input"`
	Prompt    string `json:"prompt"`
}

func (p hookPayload) promptText() string {
	if p.UserInput != "" {
		return p.UserInput
	}
	return p.Prompt
}

// hookOutput is written to stdout. Both the top-level fields and the nested
// hookSpecificOutput form are emitted: Claude Code has carried both shapes
// across versions and ignores fields it does not know, so emitting both is
// cheaper than pinning the engineer's CLI version for two weeks.
type hookOutput struct {
	Decision           string             `json:"decision,omitempty"`
	Reason             string             `json:"reason,omitempty"`
	SystemMessage      string             `json:"systemMessage,omitempty"`
	AdditionalContext  string             `json:"additionalContext,omitempty"`
	HookSpecificOutput *hookSpecificBlock `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificBlock struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
	Decision          string `json:"decision,omitempty"`
	Reason            string `json:"reason,omitempty"`
}

func emit(o hookOutput) {
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(b))
}

// taskContext resolves the open task envelope for a hook's working directory,
// plus its assignment. A session started somewhere with no open envelope is not
// in the experiment at all — every hook returns silently. That is deliberate:
// an unopened session must never be nudged, or the control condition rots.
func taskContext(cfg Config, cwd string) (Assignment, bool) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	root := repoRootOf(cwd)
	active, ok := readActiveTask(root)
	if !ok {
		return Assignment{}, false
	}
	rows, err := readAssignments()
	if err != nil {
		return Assignment{}, false
	}
	return findAssignment(rows, cfg, active.TaskKey)
}

// runHook is the single entry point for every registered Claude Code hook.
// It fails OPEN on every error path: a broken experiment harness must never be
// able to wedge an engineer's session.
func runHook(args []string) int {
	if len(args) < 1 {
		return 0
	}
	which := args[0]

	raw, err := io.ReadAll(os.Stdin)
	if err != nil || len(raw) == 0 {
		return 0
	}
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}

	cfg, err := loadConfig()
	if err != nil || !cfg.Enabled {
		return 0
	}

	switch which {
	case "session-start":
		return hookSessionStart(cfg, p)
	case "pre-compact":
		return hookPreCompact(cfg, p)
	case "user-prompt-submit":
		return hookUserPromptSubmit(cfg, p)
	}
	return 0
}

// hookSessionStart shows the assigned treatment artifact to both the engineer
// (systemMessage) and the model (additionalContext), and arms C2's gate when
// the session is starting up FROM a compaction.
func hookSessionStart(cfg Config, p hookPayload) int {
	a, ok := taskContext(cfg, p.CWD)
	if !ok || !a.Eligible {
		return 0
	}

	if p.Source == "compact" && a.Factors.C2 {
		armGate(cfg, p.SessionID, a, "session_start_compact")
	}

	var ctx []string
	if a.Factors.C1 {
		ctx = append(ctx, c1Contract(a.TaskKey, a.Envelope.Title))
	}
	if a.Factors.C2 && p.Source == "compact" {
		ctx = append(ctx, "This session just compacted. Arm C2 requires a re-anchor brief before\nwork continues; the next prompt is gated until one is written:\n\n"+reanchorTemplate(a.TaskKey))
	} else if a.Factors.C2 {
		ctx = append(ctx, c2Notice(a.TaskKey))
	}
	if len(ctx) == 0 {
		// Control (or C2 pre-compaction on a non-compact start) gets nothing.
		// Silence IS the control condition.
		return 0
	}

	body := strings.Join(ctx, "\n\n")
	_ = recordEvent(Event{
		ComplianceEvent: "artifact_shown", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: a.TaskKey, AssignmentID: a.AssignmentID, Arm: a.Arm, ExperimentKey: cfg.ExperimentKey,
		SessionID: p.SessionID, Detail: "session_start:" + p.Source,
	})
	emit(hookOutput{
		SystemMessage:     body,
		AdditionalContext: body,
		HookSpecificOutput: &hookSpecificBlock{
			HookEventName: "SessionStart", AdditionalContext: body,
		},
	})
	return 0
}

// hookPreCompact records the compaction and arms C2's gate. It deliberately
// does NOT block compaction (exit 2 would): the treatment is the re-anchor
// after compaction, not the prevention of compaction.
func hookPreCompact(cfg Config, p hookPayload) int {
	a, ok := taskContext(cfg, p.CWD)
	if !ok || !a.Eligible {
		return 0
	}
	trigger := p.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	_ = recordEvent(Event{
		ComplianceEvent: "compaction", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: a.TaskKey, AssignmentID: a.AssignmentID, Arm: a.Arm, ExperimentKey: cfg.ExperimentKey,
		SessionID: p.SessionID, Detail: "trigger:" + trigger,
	})
	if a.Factors.C2 {
		armGate(cfg, p.SessionID, a, "pre_compact:"+trigger)
	}
	return 0
}

func armGate(cfg Config, sessionID string, a Assignment, why string) {
	if sessionID == "" {
		return
	}
	if g, ok := readGate(sessionID); ok && g.Armed {
		return // already armed by the other hook point; don't reset attempts
	}
	_ = writeGate(sessionID, GateState{
		Armed: true, TaskKey: a.TaskKey, AssignmentID: a.AssignmentID,
		Arm: a.Arm, ExperimentKey: cfg.ExperimentKey, CompactedAt: nowUTC(),
	})
	_ = recordEvent(Event{
		ComplianceEvent: "gate_armed", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: a.TaskKey, AssignmentID: a.AssignmentID, Arm: a.Arm, ExperimentKey: cfg.ExperimentKey,
		SessionID: sessionID, Detail: why,
	})
}

// hookUserPromptSubmit is C2's actual enforcement point. SessionStart cannot
// block (exit 2 is ignored there) and PreCompact can only block compaction, so
// the prompt gate is the only place a "block until re-anchored" requirement can
// be honored at all.
func hookUserPromptSubmit(cfg Config, p hookPayload) int {
	if p.SessionID == "" {
		return 0
	}
	g, ok := readGate(p.SessionID)
	if !ok || !g.Armed {
		return 0
	}

	prompt := p.promptText()

	if isBypass(prompt) || os.Getenv("PROMPTSTER_EXPERIMENT_BYPASS") == "1" {
		g.Armed = false
		_ = writeGate(p.SessionID, g)
		_ = recordEvent(Event{
			ComplianceEvent: "gate_bypassed", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
			TaskKey: g.TaskKey, AssignmentID: g.AssignmentID, Arm: g.Arm, ExperimentKey: cfg.ExperimentKey,
			SessionID: p.SessionID, Attempt: g.Attempts + 1,
		})
		return 0
	}

	res := checkReanchor(prompt)
	if res.OK {
		g.Armed = false
		_ = writeGate(p.SessionID, g)
		_ = recordEvent(Event{
			ComplianceEvent: "reanchor_accepted", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
			TaskKey: g.TaskKey, AssignmentID: g.AssignmentID, Arm: g.Arm, ExperimentKey: cfg.ExperimentKey,
			SessionID: p.SessionID, CharLen: res.Chars, Attempt: g.Attempts + 1,
		})
		return 0
	}

	g.Attempts++
	_ = writeGate(p.SessionID, g)
	saved := saveRejectedPrompt(p.SessionID, prompt)
	detail := "chars=" + itoa(res.Chars)
	if len(res.Missing) > 0 {
		detail += " missing=" + strings.Join(res.Missing, ",")
	}
	_ = recordEvent(Event{
		ComplianceEvent: "reanchor_rejected", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: g.TaskKey, AssignmentID: g.AssignmentID, Arm: g.Arm, ExperimentKey: cfg.ExperimentKey,
		SessionID: p.SessionID, CharLen: res.Chars, Attempt: g.Attempts, Detail: detail,
	})

	reason := blockReason(g.TaskKey, res.Chars, saved)
	if len(res.Missing) > 0 {
		reason += "\nMissing section(s): " + strings.Join(res.Missing, ", ") + "\n"
	}
	// decision:"block" with a reason is used instead of exit 2 on purpose: exit
	// 2 erases the prompt outright, and destroying the engineer's typing is the
	// fastest way to make an assigned arm stop cooperating.
	emit(hookOutput{
		Decision: "block", Reason: reason, SystemMessage: reason,
		HookSpecificOutput: &hookSpecificBlock{
			HookEventName: "UserPromptSubmit", Decision: "block", Reason: reason,
		},
	})
	return 0
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
