package main

import (
	"errors"
	"strings"
	"testing"
)

// The refusals are the mode. openspec private-problem-sandbox-lane task 2.5c.
//
// They are pure functions rather than branches inside `cmdStart` for one
// reason: the task asks for a suppression that is TESTABLE in the CLI instead of
// an argv string in the worker, and a branch that ends in `os.Exit(1)` cannot be
// asserted in-process. What is asserted here is not only THAT it refuses but
// WHAT IT SAYS — a worker operator reading the message has to learn which half
// of the seed is wrong, because every one of these failures otherwise surfaces
// minutes later as a hang or an empty capture.

func TestSeededRefusesACandidateKey(t *testing.T) {
	msg := seededArgsRefusal([]string{"PST-EY3F-5DW9"})
	if msg == "" {
		t.Fatal("a key argument must be refused — the worker redeems, not the box")
	}
	// The message has to name the 409, because that is the unrecoverable
	// outcome the refusal exists to prevent, and it is not obvious from
	// "takes no arguments".
	if !strings.Contains(msg, "409") {
		t.Errorf("the refusal must explain the 409, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Nothing was redeemed") {
		t.Errorf("the refusal must say nothing was consumed, got:\n%s", msg)
	}
}

func TestSeededRefusesAnyArgumentNotJustAKey(t *testing.T) {
	// A path, a typo, anything. The mode reads everything from disk, so an
	// argument means the caller believes something this mode does not do.
	msg := seededArgsRefusal([]string{"/home/user/problems/threadline-stylist"})
	if msg == "" {
		t.Fatal("a non-key argument must also be refused")
	}
	if strings.Contains(msg, "409") {
		t.Errorf("only a key argument should raise the 409, got:\n%s", msg)
	}
}

func TestSeededAcceptsNoArguments(t *testing.T) {
	if msg := seededArgsRefusal(nil); msg != "" {
		t.Fatalf("no arguments is the correct call, got refusal:\n%s", msg)
	}
}

func TestSeededRefusesAMissingSession(t *testing.T) {
	msg := seededSessionRefusal(errors.New("open session.json: no such file"), Session{}, "/x/session.json")
	if msg == "" {
		t.Fatal("no session on disk must be refused, not prompted for")
	}
	// It must NOT tell the operator to run a redeem — that is exactly the
	// instruction the generic no-session path gives, and following it here is
	// how the key gets burned from inside the box.
	if strings.Contains(msg, "promptster start PST-") {
		t.Errorf("must not send the operator to a redeem, got:\n%s", msg)
	}
	if !strings.Contains(msg, "/x/session.json") {
		t.Errorf("must name the path it looked at, got:\n%s", msg)
	}
}

func TestSeededRefusesAPartialSeed(t *testing.T) {
	// `redeem` ran but nothing wrote the token: the shape a half-finished
	// provision leaves behind.
	msg := seededSessionRefusal(nil, Session{ConsentAccepted: true}, "/x/session.json")
	if msg == "" {
		t.Fatal("a session with no sessionToken must be refused")
	}
	if !strings.Contains(msg, "sessionToken") {
		t.Errorf("must name the missing field, got:\n%s", msg)
	}
}

func TestSeededRefusesUnrecordedConsent(t *testing.T) {
	// ⛔ The failure this prevents is a HANG, not an error. Falling through opens
	// a consent TUI in a box with nobody at the terminal, and `start` waits for
	// the whole assessment window.
	msg := seededSessionRefusal(nil, Session{SessionToken: "PST-AAAA-BBBB"}, "/x/session.json")
	if msg == "" {
		t.Fatal("consentAccepted=false must be refused, not prompted")
	}
	if !strings.Contains(msg, "consentAccepted") {
		t.Errorf("must name the field that is wrong, got:\n%s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "prompt") {
		t.Errorf("must explain that the alternative is an unanswerable prompt, got:\n%s", msg)
	}
}

func TestSeededAcceptsACompleteSeed(t *testing.T) {
	msg := seededSessionRefusal(nil, Session{
		SessionToken:    "PST-AAAA-BBBB",
		ConsentAccepted: true,
		TaskRoot:        "/home/user/problems/threadline-stylist",
	}, "/x/session.json")
	if msg != "" {
		t.Fatalf("a complete seed must be accepted, got refusal:\n%s", msg)
	}
}
