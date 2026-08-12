package main

import (
	"fmt"
	"strings"
)

// TreatmentSpecVersion versions the intervention artifacts below. The registry
// requirement (specs/practice-experiments) is that a practice enters the system
// as a VERSIONED protocol package, not a label — so every assignment row
// carries the version of the artifact text the engineer actually saw. Changing
// any banner or gate text below is a version bump, and a mid-batch bump splits
// the analysis.
const TreatmentSpecVersion = "batch1-v1"

const toolVersion = "0.1.0"

// reanchorMinChars is C2's threshold, verbatim from design.md: "a >=200-char
// re-anchor brief (what/why/done-when)".
const reanchorMinChars = 200

const bypassToken = "!noanchor"

// ---------------------------------------------------------------------------
// C1 — one-artifact-per-session
// ---------------------------------------------------------------------------

// c1Contract is the treatment artifact: "task-open hook prints the contract
// ('this session ships ONE artifact; new topic -> new session/subagent')".
func c1Contract(taskKey, title string) string {
	ships := title
	if ships == "" {
		ships = taskKey
	}
	return "" +
		"C1 — ONE-ARTIFACT-PER-SESSION CONTRACT (assigned, required)\n" +
		"\n" +
		"This session ships ONE artifact: " + ships + "\n" +
		"\n" +
		"  • New topic → new session or subagent. Not this one.\n" +
		"  • A tangent you want to keep → open it as its own task\n" +
		"    (`promptster-experiment open --task …`), do not carry it here.\n" +
		"  • Finishing this artifact ends the session. Do not start the next\n" +
		"    thing in the same window.\n" +
		"\n" +
		"Task: " + taskKey
}

// c1Banner is the terminal-visible version printed at task open.
func c1Banner(taskKey, title string) string {
	return box("EXPERIMENT · C1 one-artifact-per-session", c1Contract(taskKey, title))
}

// ---------------------------------------------------------------------------
// C2 — post-compact re-anchor
// ---------------------------------------------------------------------------

func c2Notice(taskKey string) string {
	return "" +
		"C2 — POST-COMPACT RE-ANCHOR (assigned, required)\n" +
		"\n" +
		"If this session compacts, the next prompt must be a re-anchor brief of\n" +
		fmt.Sprintf("at least %d characters covering what / why / done-when.\n", reanchorMinChars) +
		"Prompts are blocked until it is written.\n" +
		"\n" +
		"Task: " + taskKey
}

func c2Banner(taskKey string) string {
	return box("EXPERIMENT · C2 post-compact re-anchor", c2Notice(taskKey))
}

// reanchorTemplate is shown on every block. The artifact under test is the
// template as much as the requirement: an engineer who has to invent the shape
// each time complies less, and adherence below 50% means "the artifact failed",
// not "the practice failed" (design.md's decision gate).
func reanchorTemplate(taskKey string) string {
	return "" +
		"What: <the one artifact this session ships — " + taskKey + ">\n" +
		"Why: <why it matters / what breaks without it>\n" +
		"Done when: <the checkable condition that ends this task>\n" +
		"State: <where you actually are right now, post-compaction>"
}

// blockReason is what the engineer sees when the gate refuses a prompt.
func blockReason(taskKey string, got int, savedPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Post-compact re-anchor required (experiment arm C2, task %s).\n\n", taskKey)
	fmt.Fprintf(&b, "This session compacted. Before continuing, write a re-anchor brief of at\nleast %d characters covering what / why / done-when. Yours was %d.\n\n", reanchorMinChars, got)
	b.WriteString("Template:\n\n")
	b.WriteString(reanchorTemplate(taskKey))
	b.WriteString("\n\n")
	if savedPath != "" {
		fmt.Fprintf(&b, "Your prompt was not lost — it is saved at:\n  %s\n\n", savedPath)
	}
	fmt.Fprintf(&b, "To proceed without the brief (recorded as non-adherence, not an error),\nprefix your prompt with %s.\n", bypassToken)
	return b.String()
}

// ---------------------------------------------------------------------------
// Re-anchor validation — this IS C2's compliance event
// ---------------------------------------------------------------------------

// anchorFields are the three required sections. Each entry is a set of accepted
// spellings; one match per field is enough. Requiring literal labels is the
// point: the compliance event must be machine-decidable and hand-auditable, and
// "does this paragraph contain a why" is neither.
var anchorFields = [][]string{
	{"what:", "what -", "what —"},
	{"why:", "why -", "why —"},
	{"done when", "done-when", "done_when", "definition of done", "acceptance:"},
}

type anchorCheck struct {
	OK      bool
	Chars   int
	Missing []string
}

var anchorFieldNames = []string{"what", "why", "done-when"}

// checkReanchor decides whether a prompt satisfies C2.
func checkReanchor(prompt string) anchorCheck {
	trimmed := strings.TrimSpace(prompt)
	lower := strings.ToLower(trimmed)
	// Count runes, not bytes: a 200-byte threshold would silently ask for fewer
	// characters from anyone whose brief contains non-ASCII.
	res := anchorCheck{Chars: len([]rune(trimmed))}
	for i, spellings := range anchorFields {
		found := false
		for _, s := range spellings {
			if strings.Contains(lower, s) {
				found = true
				break
			}
		}
		if !found {
			res.Missing = append(res.Missing, anchorFieldNames[i])
		}
	}
	res.OK = res.Chars >= reanchorMinChars && len(res.Missing) == 0
	return res
}

func isBypass(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(prompt)), bypassToken)
}

// ---------------------------------------------------------------------------

// controlBanner confirms the envelope opened without showing any contract text.
// The control condition is "work as usual" — leaking C1's contract into control
// would erase the contrast the batch exists to measure.
func controlBanner(taskKey, arm string) string {
	return box("EXPERIMENT · task envelope open",
		"Task: "+taskKey+"\nArm: "+arm+"\nNo protocol assigned — work as usual.")
}

func box(title, body string) string {
	const w = 72
	line := strings.Repeat("─", w)
	var b strings.Builder
	b.WriteString("\n┌" + line + "┐\n")
	b.WriteString("│ " + pad(title, w-1) + "│\n")
	b.WriteString("├" + line + "┤\n")
	for _, l := range strings.Split(body, "\n") {
		for _, chunk := range wrap(l, w-2) {
			b.WriteString("│ " + pad(chunk, w-1) + "│\n")
		}
	}
	b.WriteString("└" + line + "┘\n")
	return b.String()
}

func pad(s string, w int) string {
	n := len([]rune(s))
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func wrap(s string, w int) []string {
	if len([]rune(s)) <= w {
		return []string{s}
	}
	var out []string
	words := strings.Fields(s)
	cur := ""
	indent := s[:len(s)-len(strings.TrimLeft(s, " "))]
	for _, word := range words {
		cand := cur
		if cand == "" {
			cand = indent + word
		} else {
			cand = cand + " " + word
		}
		if len([]rune(cand)) > w && cur != "" {
			out = append(out, cur)
			cur = indent + "  " + word
			continue
		}
		cur = cand
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
