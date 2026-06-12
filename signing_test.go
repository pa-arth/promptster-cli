package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// fixtureEvent and fixtureSeed define a stable test vector used to prove the
// Go signing output matches the TS `buildSigningMessage` byte-for-byte.
// The expected message and sig below MUST match the TS fixture test in
// apps/api/src/__tests__/signing-fixture.test.ts. Do not mutate either
// side without updating the other — that would silently break cross-language
// verification.
var fixtureSeed = mustHex("0102030405060708091011121314151617181920212223242526272829303132")

var fixtureEvent = Event{
	ID:        "01234567-89ab-cdef-0123-456789abcdef",
	SessionID: "sess-abc",
	Ts:        "2026-01-01T00:00:00.000Z",
	Kind:      "command",
	Source:    "terminal",
	V:         1,
	Data: map[string]interface{}{
		"command":    "ls -la",
		"exitCode":   float64(0),
		"durationMs": float64(42),
	},
}

const fixturePrevSig = ""

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// fixtureExpectedMessage and fixtureExpectedSigHex pin the exact signing
// bytes for the fixture event + fixtureSeed. The TS test at
// apps/api/src/__tests__/signing-fixture.test.ts asserts the same values,
// so any divergence in canonical-JSON encoding between Go and TS breaks CI.
const fixtureExpectedMessage = "PST-EVT-V1\n" +
	"01234567-89ab-cdef-0123-456789abcdef\n" +
	"sess-abc\n" +
	"2026-01-01T00:00:00.000Z\n" +
	"command\n" +
	"terminal\n" +
	"1\n" +
	"754338c13ebc454648b60cc340edc9221e95c6e30e5feba10aa5a7b8506d24a5\n" +
	"\n"

const fixtureExpectedSigHex = "39375331ee604f6dd11cd975b0a00ccb2894feebb8b82dcc98316e1b88997d60934c33322c4481d3b593a8c4667460cd93db0660514424288e3b3caa257fe90f"

func TestBuildSigningMessage_Stable(t *testing.T) {
	msg, err := buildSigningMessage(fixtureEvent, fixturePrevSig)
	if err != nil {
		t.Fatalf("buildSigningMessage: %v", err)
	}
	if string(msg) != fixtureExpectedMessage {
		t.Fatalf("signing message mismatch:\n--- got ---\n%q\n--- want ---\n%q",
			string(msg), fixtureExpectedMessage)
	}
}

func TestSign_MatchesPinnedFixture(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(fixtureSeed)
	sigHex, _, err := signEvent(fixtureEvent, fixturePrevSig, priv)
	if err != nil {
		t.Fatalf("signEvent: %v", err)
	}
	if sigHex != fixtureExpectedSigHex {
		t.Fatalf("sig drift from fixture:\n got  %s\n want %s", sigHex, fixtureExpectedSigHex)
	}
}

func TestSignAndVerify_RoundTrip(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(fixtureSeed)
	pub := priv.Public().(ed25519.PublicKey)

	sigHex, hashHex, err := signEvent(fixtureEvent, fixturePrevSig, priv)
	if err != nil {
		t.Fatalf("signEvent: %v", err)
	}
	if len(sigHex) != 128 {
		t.Fatalf("sig length = %d, want 128", len(sigHex))
	}
	if len(hashHex) != 64 {
		t.Fatalf("hash length = %d, want 64", len(hashHex))
	}

	msg, _ := buildSigningMessage(fixtureEvent, fixturePrevSig)
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatalf("ed25519.Verify failed on valid sig")
	}

	tampered := fixtureEvent
	tampered.Data = map[string]interface{}{
		"command":    "ls -la",
		"exitCode":   float64(0),
		"durationMs": float64(43),
	}
	msg2, _ := buildSigningMessage(tampered, fixturePrevSig)
	if ed25519.Verify(pub, msg2, sig) {
		t.Fatalf("ed25519.Verify unexpectedly accepted a tampered event")
	}
}

func TestGenerateSessionKeypair_PersistsWith0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	pubB64, err := generateSessionKeypair()
	if err != nil {
		t.Fatalf("generateSessionKeypair: %v", err)
	}
	if pubB64 == "" {
		t.Fatal("empty pubkey")
	}

	info, err := os.Stat(filepath.Join(dir, "session.key"))
	if err != nil {
		t.Fatalf("stat session.key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session.key mode = %o, want 600", info.Mode().Perm())
	}

	priv, err := loadSessionKeypair()
	if err != nil {
		t.Fatalf("loadSessionKeypair: %v", err)
	}
	if priv == nil {
		t.Fatal("loadSessionKeypair returned nil after generate")
	}
}

func TestBufferLock_ChainsSigs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(dir, "buffer.jsonl"))

	if _, err := generateSessionKeypair(); err != nil {
		t.Fatalf("generate keypair: %v", err)
	}

	e1 := newEvent("prompt", "sess-1")
	e1.Data = map[string]interface{}{"text": "first"}
	if err := appendEventToLocalBuffer(&e1); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if e1.Sig == "" {
		t.Fatal("e1 missing sig")
	}
	if e1.PrevSig != "" {
		t.Fatalf("e1 prevSig = %q, want empty (first event)", e1.PrevSig)
	}

	e2 := newEvent("command", "sess-1")
	e2.Data = map[string]interface{}{"command": "ls"}
	if err := appendEventToLocalBuffer(&e2); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	if e2.PrevSig != e1.Sig {
		t.Fatalf("e2 prevSig mismatch: got %q, want %q", e2.PrevSig, e1.Sig)
	}
	if e2.Sig == "" || e2.Sig == e1.Sig {
		t.Fatalf("e2 sig unexpected: %q", e2.Sig)
	}
}
