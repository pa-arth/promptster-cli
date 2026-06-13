package main

import "time"

// demoBriefSession returns a fake session + fully populated sample brief for
// `promptster brief --demo`. Placeholder content only — it exists so we can
// iterate on the brief structure and TUI design without a live session, and
// so assessment authors can see what a fully populated brief looks like.
func demoBriefSession() (Session, Brief) {
	s := Session{
		OrgName:          "Acme Robotics",
		TaskRoot:         "~/promptster/acme-fleet-dashboard",
		TimeLimitMinutes: 90,
		StartedAt:        time.Now().Add(-23 * time.Minute),
	}
	return s, sampleBrief()
}

func sampleBrief() Brief {
	return Brief{
		Scenario: "You're joining the team behind Fleet Dashboard, an internal tool that tracks delivery-robot health across three cities. The service works, but the team has flagged a regression in the alerting pipeline, and product needs a new capability shipped on top of it. You own both.",
		Codebase: BriefCodebase{
			Name:        "fleet-dashboard",
			Description: "A Go HTTP service with a small web UI. Ingests robot telemetry, stores it in SQLite, and raises alerts when readings cross thresholds.",
			Stack:       []string{"Go 1.25", "SQLite", "chi router", "htmx UI"},
			RunCommand:  "make dev",
			TestCommand: "make test",
		},
		Phases: []BriefPhase{
			{
				Name: "Orient & stabilize",
				Goal: "Get the service running, understand how telemetry flows through it, and fix the alerting regression.",
				Tasks: []string{
					"Get the project running locally and explore the codebase",
					"Investigate why duplicate alerts are firing for healthy robots",
					"Fix the root cause and add a regression test that would have caught it",
				},
				Guidance: []string{
					"Finding your way around unfamiliar code is part of the job — there's no map",
				"We're watching how you narrow down a problem, not just whether you fix it",
					"Small, well-described commits beat one big one",
				},
			},
			{
				Name: "Build the feature",
				Goal: "Design and ship alert escalation: unacknowledged critical alerts notify an on-call engineer after a configurable delay.",
				Tasks: []string{
					"Sketch your approach before writing code — where does escalation state live?",
					"Implement the escalation flow end to end",
					"Test it properly: unit tests for the logic, plus at least one integration-level check",
				},
				Guidance: []string{
					"There are several defensible architectures here — we care about why you chose yours",
					"Use your AI tools the way you actually work; iterating on prompts is part of the job",
				},
			},
		},
		Evaluation: []string{
			"How you direct AI tools — prompt quality, iteration, and judgment about their output",
			"Debugging process — forming hypotheses, narrowing down, verifying",
			"Architectural decisions and the tradeoffs you weigh",
			"Testing rigor — do you prove your changes work?",
			"Code quality and fit with the existing codebase",
		},
		GroundRules: []string{
			"AI coding tools are allowed and expected — that's what we're assessing",
			"Work only inside the assessment workspace",
			"Don't paste in solutions from outside this session",
			"Record key decisions as you go with promptster explain",
		},
		Deliverables: []string{
			"The alerting regression fixed, with a regression test",
			"The escalation feature working end to end, with tests",
			"Decision rationale captured for your major choices",
			"Everything committed — then run promptster done to submit",
		},
	}
}
