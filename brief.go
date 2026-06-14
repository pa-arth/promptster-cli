package main

// Brief is the structured assessment brief shown to candidates.
//
// The shape mirrors how Promptster assessments run: the candidate inherits an
// existing codebase (orient, debug, review), then designs and builds a new
// feature on top of it (architecture decisions, prompting, implementation,
// testing). Logistics (time limit, workspace, submission) live on Session,
// not here — the brief is pure task content.
//
// Information-leak guardrail: there is intentionally no field for describing
// the planted defect or the expected solution. Phases carry Tasks (what to
// do) and Guidance (what we pay attention to). Anything that would tell the
// candidate *where* the bug is or *how* to architect the feature does not
// belong in the brief — same reasoning as the no-auto-title rule (see
// project_audit_may2026_results.md).
type Brief struct {
	// Scenario is the narrative framing: what situation the candidate is
	// stepping into and why the work matters. One or two short paragraphs.
	Scenario string `json:"scenario,omitempty"`
	// Codebase orients the candidate in the inherited repo — stack, how to
	// run and test it, where to start reading. Orientation only.
	Codebase BriefCodebase `json:"codebase,omitzero"`
	// Phases are the ordered parts of the assessment. Typically two:
	// "orient & stabilize" (debugging / code review on the existing code)
	// then "build" (a new feature with real architectural choices).
	Phases []BriefPhase `json:"phases,omitempty"`
	// Evaluation lists the dimensions being assessed (e.g. how the candidate
	// directs AI tools, debugging process, tradeoff reasoning, testing
	// rigor). Dimensions only — never rubric details or weights.
	Evaluation []string `json:"evaluation,omitempty"`
	// GroundRules are the rules of engagement: what's allowed, what isn't,
	// and in-session obligations (e.g. record rationale via `promptster
	// explain`).
	GroundRules []string `json:"groundRules,omitempty"`
	// Deliverables is the definition of done — what must exist when the
	// candidate runs `promptster done`.
	Deliverables []string `json:"deliverables,omitempty"`
}

// BriefCodebase introduces the inherited repository — only what a real
// onboarding would give you: what the product is and how to run it. There is
// deliberately no "where to look" field (entry points, key directories):
// orienting in unfamiliar code is part of what's being assessed, and in a
// debugging task a reading pointer is half the answer.
type BriefCodebase struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Stack lists languages / frameworks / notable dependencies.
	Stack []string `json:"stack,omitempty"`
	// RunCommand and TestCommand are the canonical "make it go" commands —
	// README-level facts. Environment fights measure nothing in a timed
	// assessment.
	RunCommand  string `json:"runCommand,omitempty"`
	TestCommand string `json:"testCommand,omitempty"`
}

func (c BriefCodebase) isEmpty() bool {
	return c.Name == "" && c.Description == "" && len(c.Stack) == 0 &&
		c.RunCommand == "" && c.TestCommand == ""
}

// BriefPhase is one ordered part of the assessment.
type BriefPhase struct {
	// Name is short and action-oriented, e.g. "Orient & stabilize".
	Name string `json:"name"`
	// Goal is a one-sentence statement of what done looks like for this phase.
	Goal string `json:"goal,omitempty"`
	// Tasks are the concrete steps, in order.
	Tasks []string `json:"tasks,omitempty"`
	// Guidance tells the candidate what we pay attention to during this
	// phase (process signals, not answers).
	Guidance []string `json:"guidance,omitempty"`
}

func (b Brief) isEmpty() bool {
	return b.Scenario == "" && b.Codebase.isEmpty() && len(b.Phases) == 0 &&
		len(b.Evaluation) == 0 && len(b.GroundRules) == 0 && len(b.Deliverables) == 0
}

// resolveBrief returns the structured brief for a session, falling back to a
// minimal single-scenario brief built from the legacy flat TaskBrief string
// when the backend didn't send a structured one.
func resolveBrief(s Session) Brief {
	if s.Brief != nil && !s.Brief.isEmpty() {
		return *s.Brief
	}
	return Brief{Scenario: s.TaskBrief}
}
