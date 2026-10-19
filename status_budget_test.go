package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAPIGetBudget(t *testing.T) {
	var gotPath, gotKey string
	body, status := `{"spentUsd":3.21,"budgetUsd":25}`, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-API-Key")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	b, err := apiGetBudget("sess-1", "PST-KEY")
	if err != nil || gotPath != "/v1/sessions/sess-1/budget" || gotKey != "PST-KEY" {
		t.Fatalf("err=%v path=%q key=%q", err, gotPath, gotKey)
	}
	if got := budgetText(b); got != "$3.21 / $25.00" {
		t.Fatalf("budgetText = %q", got)
	}

	// Failures must surface as errors so the row is omitted, never shown as $0.
	for _, c := range []struct {
		body   string
		status int
	}{{`{"error":"nope"}`, 500}, {`{"spentUsd":0}`, 200}, {`not json`, 200}} {
		body, status = c.body, c.status
		if _, err := apiGetBudget("sess-1", "PST-KEY"); err == nil {
			t.Errorf("%d %s: want error", c.status, c.body)
		}
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	prev := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = prev
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestPrintHelp_CandidateListOnlyInsideSession(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())

	if out := captureStdout(t, func() { printHelp(nil) }); !strings.Contains(out, "redeem <key>") {
		t.Fatalf("no session: want full help, got:\n%s", out)
	}

	if err := saveSession(Session{SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { printHelp(nil) })
	if strings.Contains(out, "redeem") || !strings.Contains(out, "promptster done") || !strings.Contains(out, "help --all") {
		t.Fatalf("in session: want candidate list, got:\n%s", out)
	}
	if out := captureStdout(t, func() { printHelp([]string{"--all"}) }); !strings.Contains(out, "redeem <key>") {
		t.Fatalf("--all: want full help, got:\n%s", out)
	}
}
