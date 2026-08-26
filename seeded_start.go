package main

import (
	"fmt"
	"strings"
)

// `promptster start --seeded` — the mode the provisioning worker uses.
//
// openspec private-problem-sandbox-lane §7.2 step 3, task 2.5c.
//
// ── Why this is a MODE and not a set of flags ───────────────────────────────
//
// The worker could pass `--adopt --accept-tos --workspace <root> --editor-extension`
// to the existing `start` and get most of this behaviour today. The task
// deliberately rejects that: "not an argv incantation the worker passes to the
// existing `start` — the suppression belongs in the CLI where it is testable,
// not in a string in the worker."
//
// The difference is not stylistic. Every rule below is a REFUSAL with a message
// naming what is wrong with the seed. A flag combination has no equivalent — it
// skips a branch, and when the seed is malformed the failure surfaces as a hang
// (a consent TUI nobody answers), a second tree beside the one the candidate is
// looking at, or a 409 in front of a candidate mid-assessment. Those are the
// three ways this has already gone wrong on adjacent lanes.
//
// ── Why the worker cannot just redeem inside the box ────────────────────────
//
// `POST /v1/candidate/redeem` is single-use — it locks FOR UPDATE and 409s any
// key not in status `pending` — and it returns the candidate key AS the session
// token. So redeeming from inside the box puts the strictly more powerful,
// still-unredeemed artifact in a box we do not fully trust, and takes the 409
// away from the worker permanently. A 409 reaching a candidate mid-assessment is
// unrecoverable without an operator. design.md §7.1, §7.2.

// seededArgsRefusal reports why a positional argument disqualifies a seeded
// start, or "" when there is none.
//
// Silently ignoring a key would leave the caller's wrong model of who redeems
// intact until it produces a 409 in front of a candidate.
func seededArgsRefusal(args []string) string {
	if len(args) == 0 {
		return ""
	}
	first := strings.TrimSpace(args[0])
	var b strings.Builder
	fmt.Fprintf(&b, "error: --seeded takes no arguments, and %q was given.\n", first)
	if strings.HasPrefix(strings.ToUpper(first), "PST-") {
		b.WriteString("  That looks like a candidate key. The WORKER redeems, once, server-side —\n")
		b.WriteString("  the key is already inside the session it wrote to disk. Redeeming again\n")
		b.WriteString("  returns 409 and there is no way back from that without an operator.\n")
	} else {
		b.WriteString("  A seeded start reads everything it needs from the session on disk.\n")
	}
	b.WriteString("  Nothing was redeemed and nothing was changed.\n")
	return b.String()
}

// seededSessionRefusal reports why the session on disk cannot support a seeded
// start, or "" when it can.
//
// `loadErr` is whatever `loadSession()` returned; `saved` is its result, which
// is only meaningful when loadErr is nil.
func seededSessionRefusal(loadErr error, saved Session, path string) string {
	if loadErr != nil {
		var b strings.Builder
		b.WriteString("error: --seeded found no session on disk.\n")
		fmt.Fprintf(&b, "  Expected the provisioning worker to have written %s\n", path)
		b.WriteString("  (or <taskRoot>/.promptster/session.json, with the workspace pointer beside it).\n")
		fmt.Fprintf(&b, "  loadSession: %v\n", loadErr)
		b.WriteString("  Nothing was redeemed and nothing was changed.\n")
		return b.String()
	}

	// A session with no token is what `redeem` leaves behind when `start` never
	// finished. On this lane it means the worker wrote a partial seed, and every
	// later step — the apiKeyHelper, the shell hook, the proxy — would come up
	// credential-less and quiet.
	if strings.TrimSpace(saved.SessionToken) == "" {
		return "error: --seeded found a session with no sessionToken.\n" +
			"  The seed is partial. Claude Code's apiKeyHelper would return nothing and the\n" +
			"  agent would run on whatever credential it finds next — working, and captured\n" +
			"  by nobody.\n"
	}

	// ⛔ Refuse rather than fall through to the consent block in `start`. That
	// block runs a TUI, and on a seeded box there is nobody at the other end of
	// it — `start` would hang for the whole assessment window while the candidate
	// stares at a terminal that never returns.
	//
	// Consent is guaranteed on file by this point in a correct provision:
	// redemption is gated on `consentConfirmedAt` (403 otherwise) and the worker
	// redeems before it seeds. So reaching here means the seed is wrong, and
	// saying so beats hanging.
	if !saved.ConsentAccepted {
		return "error: --seeded requires a session whose consent is already recorded.\n" +
			"  The seeded session.json has consentAccepted=false, so the worker either\n" +
			"  redeemed before consent was confirmed or dropped the field when it wrote the\n" +
			"  file. Refusing rather than opening a consent prompt no one is here to answer.\n"
	}

	return ""
}
