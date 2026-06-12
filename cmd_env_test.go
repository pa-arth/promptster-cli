package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellQuoteHandlesSingleQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"PST-ABCD-1234", `'PST-ABCD-1234'`},
		{"", `''`},
		{"has space", `'has space'`},
		// The single-quote case is what justifies the helper's existence —
		// a shell-injection regression in the token format must not be able
		// to escape the eval and execute arbitrary commands.
		{"a'b", `'a'\''b'`},
		{"';rm -rf /;'", `''\'';rm -rf /;'\'''`},
	}
	for _, c := range cases {
		got := shellQuote(c.in)
		if got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// captureEnv runs cmdEnv with an isolated state dir and returns the stdout
// bytes plus a cleanup func. It does NOT validate the exit code (cmdEnv
// always exits 0; the contract is that empty output == "no exports").
func captureEnv(t *testing.T, session *Session) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	if session != nil {
		path := filepath.Join(dir, "session.json")
		data, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			t.Fatalf("marshal session: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	cmdEnv(nil)
	w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// captureAuthToken runs cmdAuthToken with an isolated state dir and returns the
// stdout bytes. Mirror of captureEnv for the apiKeyHelper path.
func captureAuthToken(t *testing.T, session *Session) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("PROMPTSTER_STATE_DIR", dir)

	if session != nil {
		path := filepath.Join(dir, "session.json")
		data, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			t.Fatalf("marshal session: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write session: %v", err)
		}
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	cmdAuthToken(nil)
	w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestCmdEnvClearPrintsUnset(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdEnv([]string{"--clear"})
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "unset ") || !strings.Contains(out, "ANTHROPIC_API_KEY") {
		t.Errorf("missing ANTHROPIC_API_KEY in clear output %q", out)
	}
	if !strings.Contains(out, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("missing ANTHROPIC_AUTH_TOKEN in clear output %q", out)
	}
	if !strings.Contains(out, "ANTHROPIC_BASE_URL") {
		t.Errorf("missing ANTHROPIC_BASE_URL in clear output %q", out)
	}
}

func TestCmdEnvNoSessionPrintsNothing(t *testing.T) {
	out := captureEnv(t, nil)
	if out != "" {
		t.Errorf("expected empty output for missing session, got %q", out)
	}
}

func TestCmdEnvPartialSessionPrintsNothing(t *testing.T) {
	// Redeem ran but start never finished — TaskRoot empty, no exports.
	out := captureEnv(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-TEST-1234",
		// TaskRoot deliberately empty
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	if out != "" {
		t.Errorf("expected empty output for partial session, got %q", out)
	}
}

func TestCmdEnvFreshSessionPrintsNothing(t *testing.T) {
	// As of 1.2.0 `promptster env` no longer exports proxy creds — the proxy
	// is wired via apiKeyHelper (see cmdAuthToken). A fresh session is a no-op.
	out := captureEnv(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-FRESH-TOKEN",
		TaskRoot:     "/tmp/ws",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	})
	if out != "" {
		t.Errorf("expected empty output (no shell exports in 1.2.0), got %q", out)
	}
}

func TestCmdAuthTokenFreshSessionPrintsBareToken(t *testing.T) {
	// The apiKeyHelper contract: bare token, no `export`, no trailing newline.
	out := captureAuthToken(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-FRESH-TOKEN",
		TaskRoot:     "/tmp/ws",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	})
	if out != "PST-FRESH-TOKEN" {
		t.Errorf("expected bare token with no decoration, got %q", out)
	}
}

func TestCmdAuthTokenStaleSessionPrintsNothing(t *testing.T) {
	// Stale: ExpiresAt in the past → self-evict (fire-and-forget) and emit
	// nothing, so Claude Code treats it as "no helper credential".
	out := captureAuthToken(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-STALE-TOKEN",
		TaskRoot:     "/tmp/ws",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
	})
	if out != "" {
		t.Errorf("expected empty output for stale session, got %q", out)
	}
}

func TestCmdAuthTokenPartialSessionPrintsNothing(t *testing.T) {
	out := captureAuthToken(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-TEST-1234",
		// TaskRoot deliberately empty — start never finished
		ExpiresAt: time.Now().Add(1 * time.Hour),
	})
	if out != "" {
		t.Errorf("expected empty output for partial session, got %q", out)
	}
}

func TestCmdAuthTokenMissingExpiresAtStillEmitsToken(t *testing.T) {
	// session.json written by a pre-feature CLI won't have ExpiresAt. We must
	// NOT treat zero-time as "stale" — that would brick every legacy session.
	// Emit the token and let the server-side proxy reject if it's truly stale.
	out := captureAuthToken(t, &Session{
		SessionID:    "abc",
		SessionToken: "PST-LEGACY-TOKEN",
		TaskRoot:     "/tmp/ws",
		// ExpiresAt: zero value
	})
	if out != "PST-LEGACY-TOKEN" {
		t.Errorf("expected token for legacy session w/o ExpiresAt, got %q", out)
	}
}
