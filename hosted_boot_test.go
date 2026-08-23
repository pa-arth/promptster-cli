package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// codespaces-hosted-assessment §3.9.
//
// The property under test throughout: an unobserved fact is reported as null,
// never as a default. `prebuilt: false` and `prebuilt: null` are opposite
// claims — the first says the prebuild topology did not apply, the second says
// we do not know whether it did — and defaulting is how the second silently
// becomes the first.

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestResolveOnCreateCompleted(t *testing.T) {
	cases := []struct {
		name             string
		markerPresent    bool
		contractDeclared bool
		want             *bool
	}{
		{"marker present is a completed setup", true, true, boolPtr(true)},
		{"marker present without the env is still completion", true, false, boolPtr(true)},
		// The image declares it writes a marker and there is none: setup failed.
		{"no marker on a contract image is a failure", false, true, boolPtr(false)},
		// No marker and no claim there would be one. Nobody was recording — which
		// is NOT the same as a box whose setup died, and the route turns the
		// second into a verdict that says the environment blocked the work.
		{"no marker on a pre-contract image is unknown", false, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveOnCreateCompleted(c.markerPresent, c.contractDeclared)
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("got %v, want unknown (nil)", *got)
			case c.want != nil && got == nil:
				t.Fatalf("got unknown (nil), want %v", *c.want)
			case c.want != nil && *got != *c.want:
				t.Fatalf("got %v, want %v", *got, *c.want)
			}
		})
	}
}

func TestResolveTreeMatchesNeverGuessesFalse(t *testing.T) {
	if got := resolveTreeMatches(hostedTreeVerified); got == nil || !*got {
		t.Fatalf("verified tree did not report true: %v", got)
	}
	if got := resolveTreeMatches(hostedTreeMismatch); got == nil || *got {
		t.Fatalf("mismatched tree did not report false: %v", got)
	}
	// The two that matter. `unverified` means the catalog carried no
	// expectedTreeSha, so the check never ran; `unreadable` means we could not
	// look. Reporting either as false produces `tree-mismatch` server-side — one
	// of only two verdicts that say the environment blocked the candidate's
	// work — which would both blame our provisioning for a box that is probably
	// fine AND hand a genuinely empty submission an excuse.
	for _, state := range []string{hostedTreeUnverified, hostedTreeUnreadable, "", "something-new"} {
		if got := resolveTreeMatches(state); got != nil {
			t.Fatalf("state %q reported %v, want unknown (nil)", state, *got)
		}
	}
}

func TestResolveBootSecondsRefusesToClamp(t *testing.T) {
	now := mustParse(t, "2026-08-23T10:01:00Z")

	if got := resolveBootSeconds("2026-08-23T10:00:00Z", now); got == nil || *got != 60 {
		t.Fatalf("got %v, want 60", got)
	}
	// A resumed codespace: `now - created_at` is a day and is no longer a boot
	// measurement. Clamping to the 3600 ceiling would file "this box took an
	// hour to come up" as an observation and poison any fleet aggregate with a
	// fabricated worst case.
	if got := resolveBootSeconds("2026-08-22T10:00:00Z", now); got != nil {
		t.Fatalf("a day-old codespace reported %v, want unknown (nil)", *got)
	}
	// Clock disagreement, not a boot time.
	if got := resolveBootSeconds("2026-08-23T10:02:00Z", now); got != nil {
		t.Fatalf("a future creation time reported %v, want unknown (nil)", *got)
	}
	for _, bad := range []string{"", "   ", "not-a-timestamp"} {
		if got := resolveBootSeconds(bad, now); got != nil {
			t.Fatalf("input %q reported %v, want unknown (nil)", bad, *got)
		}
	}
}

// GitHub documents `codespace.prebuild` as nullable. The tri-state is theirs;
// flattening their null at our edge would defeat the whole report.
func TestBuildHostedBootReportPreservesGitHubsNull(t *testing.T) {
	now := mustParse(t, "2026-08-23T10:01:00Z")
	facts := &codespaceFacts{Prebuild: nil, CreatedAt: "2026-08-23T10:00:00Z"}

	r := buildHostedBootReport(true, true, hostedTreeVerified, facts, now)
	if r.Prebuilt != nil {
		t.Fatalf("prebuilt = %v, want unknown (nil)", *r.Prebuilt)
	}
	if r.BootSeconds == nil || *r.BootSeconds != 60 {
		t.Fatalf("bootSeconds = %v, want 60", r.BootSeconds)
	}
}

// The §3.9 headline. When GitHub cannot be reached at all, the two facts it
// owns go out as null — not as the false/zero §2 refused to send.
func TestBuildHostedBootReportWithNoGitHubAnswer(t *testing.T) {
	now := mustParse(t, "2026-08-23T10:01:00Z")
	r := buildHostedBootReport(true, true, hostedTreeVerified, nil, now)

	if r.Prebuilt != nil {
		t.Fatalf("prebuilt = %v, want unknown (nil) — false would report a prebuilt box as healthy-cold", *r.Prebuilt)
	}
	if r.BootSeconds != nil {
		t.Fatalf("bootSeconds = %v, want unknown (nil) — 0 would report an unknown boot as perfect", *r.BootSeconds)
	}
	if r.OnCreateCompleted == nil || !*r.OnCreateCompleted {
		t.Fatalf("onCreateCompleted = %v, want true", r.OnCreateCompleted)
	}
}

// An unknown must serialise as an explicit `null`, not vanish. The route's
// schema is nullable-but-NOT-optional precisely so a truncated payload cannot
// pass as a considered answer, so an `omitempty` creeping onto these fields
// would turn every unknown into a 400 — or, worse, into a field the server
// defaults for us.
func TestHostedBootReportMarshalsUnknownsAsExplicitNull(t *testing.T) {
	data, err := json.Marshal(hostedBootReport{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"prebuilt", "onCreateCompleted", "bootSeconds", "treeMatches"} {
		v, present := got[key]
		if !present {
			t.Fatalf("%q was omitted; the route requires the key and rejects the payload without it", key)
		}
		if v != nil {
			t.Fatalf("%q = %v, want null", key, v)
		}
	}
}

func TestParseHostedSetupMarker(t *testing.T) {
	m := parseHostedSetupMarker([]byte(
		"schema=1\nstartedAt=2026-08-23T10:00:00Z\ncompletedAt=2026-08-23T10:00:42Z\n" +
			"setupSeconds=42\nprebuildEnvRaw=true\nprebuildEnvSet=yes\n" +
			"codespaceName=octocat-space-1\nissueId=demo\n"))
	if m.Schema != 1 || m.SetupSeconds == nil || *m.SetupSeconds != 42 {
		t.Fatalf("bad parse: %+v", m)
	}
	if m.PrebuildEnvRaw != "true" || !m.PrebuildEnvSet {
		t.Fatalf("prebuild evidence not parsed: %+v", m)
	}

	// Forward compatibility: a newer generator adding a field must not make an
	// older CLI treat the marker as unreadable, because "unreadable" would walk
	// straight into "setup did not complete".
	m2 := parseHostedSetupMarker([]byte("schema=2\nsetupSeconds=7\nsomethingNew=hello\nnot a pair\n"))
	if m2.Schema != 2 || m2.SetupSeconds == nil || *m2.SetupSeconds != 7 {
		t.Fatalf("unknown keys broke the parse: %+v", m2)
	}

	// An empty marker is still a completed setup: §1.3 writes it last and only
	// on success, so presence is the signal and the body is detail.
	if m3 := parseHostedSetupMarker(nil); m3.Schema != 0 {
		t.Fatalf("empty marker parsed to %+v", m3)
	}
}

func TestFetchCodespaceFactsFailsClosed(t *testing.T) {
	// Every one of these is a `nil, err` that becomes null in the report. The
	// un-scoped-token path in particular is the EXPECTED path until task 0.2 is
	// run, so it has to land somewhere honest rather than somewhere convenient.
	if _, err := fetchCodespaceFacts("", "tok"); err == nil {
		t.Fatal("no codespace name did not error")
	}
	if _, err := fetchCodespaceFacts("name", ""); err == nil {
		t.Fatal("no token did not error")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)
	if _, err := fetchCodespaceFacts("name", "tok"); err == nil {
		t.Fatal("a 403 (token without `codespace` scope) did not error")
	}
}

func TestFetchCodespaceFactsReadsGitHubsShape(t *testing.T) {
	var gotPath, gotAuth, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotVersion = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-GitHub-Api-Version")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"octocat-space-1","prebuild":true,"created_at":"2026-08-23T10:00:00Z"}`) //nolint:errcheck
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)

	facts, err := fetchCodespaceFacts("octocat-space-1", "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/user/codespaces/octocat-space-1" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" || gotVersion != "2022-11-28" {
		t.Fatalf("auth = %q, version = %q", gotAuth, gotVersion)
	}
	if facts.Prebuild == nil || !*facts.Prebuild {
		t.Fatalf("prebuild = %v, want true", facts.Prebuild)
	}
	if facts.CreatedAt != "2026-08-23T10:00:00Z" {
		t.Fatalf("created_at = %q", facts.CreatedAt)
	}

	// The nullable arm, on the wire rather than in a struct literal.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"prebuild":null,"created_at":"2026-08-23T10:00:00Z"}`) //nolint:errcheck
	}))
	defer srv2.Close()
	t.Setenv("GITHUB_API_URL", srv2.URL)
	facts2, err := fetchCodespaceFacts("n", "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if facts2.Prebuild != nil {
		t.Fatalf("a null prebuild decoded to %v", *facts2.Prebuild)
	}
}

func TestHostedBootReportedOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMPTSTER_HOSTED_SETUP_MARKER", filepath.Join(dir, "hosted-setup-complete"))

	if hostedBootAlreadyReported("sess-1") {
		t.Fatal("reported before anything was written")
	}
	markHostedBootReported("sess-1")
	if !hostedBootAlreadyReported("sess-1") {
		t.Fatal("did not remember the report")
	}
	// A genuinely new session in a reused box still reports: the sentinel is
	// keyed by session, not by machine.
	if hostedBootAlreadyReported("sess-2") {
		t.Fatal("a different session was treated as already reported")
	}
	// And the sentinel lives outside the workspace, beside the marker, for the
	// same reason: anything inside the repo lands in the candidate's diff.
	if filepath.Dir(hostedBootReportedPath()) != dir {
		t.Fatalf("sentinel at %s, want beside the marker in %s", hostedBootReportedPath(), dir)
	}
}

// The end-to-end shape, on the wire: a box that can observe nothing but its own
// marker still files a report, and every unobserved field is an explicit null.
func TestReportHostedBootSendsExplicitUnknowns(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hosted-setup-complete")
	t.Setenv("PROMPTSTER_HOSTED_SETUP_MARKER", marker)
	if err := os.WriteFile(marker, []byte("schema=1\nsetupSeconds=42\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("PROMPTSTER_SETUP_MARKER_CONTRACT", "1")
	// No CODESPACE_NAME and no GITHUB_TOKEN: GitHub cannot be asked.
	t.Setenv("CODESPACE_NAME", "")
	t.Setenv("GITHUB_TOKEN", "")

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"verdict":{"state":"boot-time-unknown"}}`) //nolint:errcheck
	}))
	defer srv.Close()
	t.Setenv("PROMPTSTER_API_URL", srv.URL)

	reportHostedBoot(Session{
		SessionID:        "sess-1",
		SessionToken:     "tok",
		TreeVerification: hostedTreeUnverified,
	})

	if body == nil {
		t.Fatal("no report was sent")
	}
	if v, ok := body["onCreateCompleted"].(bool); !ok || !v {
		t.Fatalf("onCreateCompleted = %v, want true", body["onCreateCompleted"])
	}
	for _, key := range []string{"prebuilt", "bootSeconds", "treeMatches"} {
		v, present := body[key]
		if !present {
			t.Fatalf("%q omitted — the route rejects a payload missing the key", key)
		}
		if v != nil {
			t.Fatalf("%q = %v, want null", key, v)
		}
	}

	// And it does not file twice: the second report would overwrite a boot
	// measurement with a later, worse one.
	body = nil
	reportHostedBoot(Session{SessionID: "sess-1", SessionToken: "tok", TreeVerification: hostedTreeUnverified})
	if body != nil {
		t.Fatal("filed a second report for the same session")
	}
}

func TestDescribeHostedBootReportSaysUnknown(t *testing.T) {
	// A --verbose line printing `false` for an unknown would hide the one thing
	// this change is about, in the exact place someone would go looking for it.
	got := describeHostedBootReport(hostedBootReport{})
	want := "prebuilt=unknown onCreateCompleted=unknown bootSeconds=unknown treeMatches=unknown"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The hosted-boot report is launched as a goroutine and joined at the end of
// `start`. Two properties of that join are worth holding: it must not be the
// thing that keeps the candidate's terminal, and it must not be so short that
// every report is abandoned.
func TestAwaitHostedBootReportReturnsWhenTheReportFinishes(t *testing.T) {
	done := make(chan struct{})
	close(done)
	start := time.Now()
	awaitHostedBootReport(done)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for an already-finished report", elapsed)
	}
}

func TestAwaitHostedBootReportIsANoOpOffTheHostedLane(t *testing.T) {
	start := time.Now()
	awaitHostedBootReport(nil) // hostedBootDone is nil when the lane is not hosted
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s on a lane that never started a report", elapsed)
	}
}

func TestHostedBootGraceIsWellUnderTheHTTPTimeout(t *testing.T) {
	// The whole point of the goroutine is that the candidate's clock never pays
	// for two 15s network calls. A grace at or above httpClient's timeout would
	// re-create exactly the block it was moved to avoid.
	if hostedBootGrace >= httpClient.Timeout {
		t.Fatalf("hostedBootGrace %s is not under httpClient.Timeout %s — the report is back on the timed path",
			hostedBootGrace, httpClient.Timeout)
	}
	if hostedBootGrace <= 0 {
		t.Fatal("hostedBootGrace must leave the report some time, or every report is abandoned")
	}
}
