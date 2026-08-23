package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The hosted-lane boot report — openspec changes/codespaces-hosted-assessment
// §3.9, the caller §3.4 was waiting for.
//
// §3.4 built `POST /v1/candidate/hosted-boot` so that a hosted session which
// produced nothing can be told apart from a candidate who did nothing. §2 then
// declined to call it, correctly: the payload wanted `prebuilt` and
// `bootSeconds` and the box can observe neither by itself, so the only report
// the CLI could file was a fabricated one — and `false`/`0` would have rendered
// a prebuilt box as `healthy-cold` and an unmeasured boot as perfect.
//
// This is that gap closed from both ends. What is observable is now observed,
// and what is not is reported as `null` rather than as a default. The backend
// contract gained nullability for exactly this (a separate change): `prebuilt:
// null` and `prebuilt: false` are opposite claims, one level down from the
// missing-vs-healthy collapse §3.4 already refuses.
//
// Where each fact comes from, and why:
//
//	onCreateCompleted  the §1.3 setup marker on disk. Written LAST and only on
//	                   success, so its presence is the fact.
//	treeMatches        the adopt verdict `start` already computed.
//	prebuilt           GitHub. GET /user/codespaces/{name} -> `prebuild`, which
//	                   GitHub documents as "Whether the codespace was created
//	                   from a prebuild" and models as NULLABLE — the tri-state
//	                   is theirs, not ours.
//	bootSeconds        the same call's `created_at`, to now.
//
// Two things deliberately NOT used as sources:
//
//   - `CODESPACE_PREBUILD`. It is a community claim and appears nowhere in
//     GitHub's documented codespace environment. An env var that does not exist
//     is indistinguishable from one set to the empty string, so reading its
//     absence as "not a prebuild" would manufacture the exact false negative
//     this task exists to prevent. The marker records it as raw evidence so the
//     lore can be checked against the API the first time a prebuild runs;
//     nothing derives from it.
//   - The marker's own timestamps, as a boot clock. `onCreateCommand` finishes
//     before there is a terminal to hand over, and on a prebuilt box it ran in a
//     different container generation days earlier. It measures SETUP, which is a
//     different quantity from the budget's creation-to-usable-terminal.

// hostedSetupMarker is the body of the §1.3 onCreateCommand marker: `key=value`
// lines, one per line.
//
// Not JSON, and that is the writer's constraint rather than the reader's: the
// producer is a shell fragment embedded in a JSON string in devcontainer.json,
// where every quote is escaped twice. `key=value` has no quoting hazard in
// either direction, and Go reads it with the standard library.
type hostedSetupMarker struct {
	// Schema is 0 for a marker written before the schema line existed, or for a
	// marker that is present but empty. Presence still means setup completed.
	Schema      int
	StartedAt   string
	CompletedAt string
	// SetupSeconds is how long the issue's setupCommand took. Nil when absent.
	SetupSeconds *int
	// PrebuildEnvRaw / PrebuildEnvSet are EVIDENCE about CODESPACE_PREBUILD, not
	// a verdict from it — see the note above. Kept apart so "unset" and "set to
	// empty" stay distinguishable, which is the whole reason the raw value is
	// worth recording at all.
	PrebuildEnvRaw string
	PrebuildEnvSet bool
	CodespaceName  string
	IssueID        string
}

// setupMarkerContractEnv is set in the generated devcontainer's containerEnv by
// §1.3. Its PRESENCE is what lets an absent marker mean "setup did not finish"
// instead of "this image is older than the marker contract" — absent for both,
// and only one of them is a broken box.
const setupMarkerContractEnv = "PROMPTSTER_SETUP_MARKER_CONTRACT"

// imageCarriesSetupMarkerContract reports whether this container was built by a
// generator that writes the marker at all.
func imageCarriesSetupMarkerContract() bool {
	return strings.TrimSpace(os.Getenv(setupMarkerContractEnv)) != ""
}

// parseHostedSetupMarker reads the `key=value` body.
//
// An unrecognised key is ignored rather than rejected: a newer generator adding
// a field must not make an older CLI report the marker as unreadable, which
// would turn a forward-compatible change into "setup did not complete".
func parseHostedSetupMarker(data []byte) hostedSetupMarker {
	var m hostedSetupMarker
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "schema":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				m.Schema = n
			}
		case "startedAt":
			m.StartedAt = value
		case "completedAt":
			m.CompletedAt = value
		case "setupSeconds":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				m.SetupSeconds = &n
			}
		case "prebuildEnvRaw":
			m.PrebuildEnvRaw = value
		case "prebuildEnvSet":
			m.PrebuildEnvSet = strings.EqualFold(strings.TrimSpace(value), "yes")
		case "codespaceName":
			m.CodespaceName = value
		case "issueId":
			m.IssueID = value
		}
	}
	return m
}

// readHostedSetupMarker returns the marker body and whether the marker EXISTS.
//
// The bool is the load-bearing half and it is deliberately not `err == nil` on a
// parse: an empty or malformed marker still means onCreateCommand ran to
// completion, because §1.3 writes it last and only on success. Presence is the
// signal; the body is detail.
func readHostedSetupMarker() (hostedSetupMarker, bool) {
	data, err := os.ReadFile(hostedSetupMarkerPath())
	if err != nil {
		return hostedSetupMarker{}, false
	}
	return parseHostedSetupMarker(data), true
}

// hostedBootReport is the POST body.
//
// Pointers, and NO `omitempty` on any of them. `nil` marshals to an explicit
// `null`, which is the reporter saying "I looked and could not tell"; an omitted
// key is a reporter that does not know the question exists, and the route
// rejects that with a 400. Adding `omitempty` here silently converts every
// unknown into the second, which is why it is called out rather than left to
// taste.
type hostedBootReport struct {
	Prebuilt          *bool    `json:"prebuilt"`
	OnCreateCompleted *bool    `json:"onCreateCompleted"`
	BootSeconds       *float64 `json:"bootSeconds"`
	TreeMatches       *bool    `json:"treeMatches"`
}

// codespaceFacts is the subset of GitHub's `codespace` resource this needs.
//
// `Prebuild` is a pointer because GitHub's own schema marks the field nullable
// ("Whether the codespace was created from a prebuild"). Flattening their null
// into `false` at the edge would defeat the entire report.
type codespaceFacts struct {
	Prebuild  *bool  `json:"prebuild"`
	CreatedAt string `json:"created_at"`
}

// githubAPIBase honours $GITHUB_API_URL, which GitHub documents as a default
// codespace environment variable, so this works on GHES without a second knob.
func githubAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.github.com"
}

// fetchCodespaceFacts asks GitHub about the codespace this process is in.
//
// Every failure here is a `nil, err` that becomes `null` in the report, never a
// substituted value. The endpoint needs the `codespace` scope and the ambient
// $GITHUB_TOKEN carrying it is UNVERIFIED (task 0.2, unrun) — so the un-scoped
// path is not an edge case to tolerate, it is the expected path until somebody
// measures it, and it has to land somewhere honest.
func fetchCodespaceFacts(name, token string) (*codespaceFacts, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("CODESPACE_NAME is not set")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("no GitHub token in the environment")
	}
	req, err := http.NewRequest(http.MethodGet, githubAPIBase()+"/user/codespaces/"+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 is the interesting one: the token exists but lacks `codespace`.
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var facts codespaceFacts
	if err := json.NewDecoder(resp.Body).Decode(&facts); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &facts, nil
}

// hostedBootBudgetCeilingSeconds is the route's upper bound on bootSeconds. It
// is not the 60-second budget — that is the backend's to judge — it is the point
// past which a number is no longer a boot measurement at all.
const hostedBootBudgetCeilingSeconds = 3600

// resolveBootSeconds turns a creation timestamp into an elapsed boot time, or
// into an honest nil.
//
// Nil, never a clamp, in both out-of-range directions. A candidate who stops and
// resumes a codespace tomorrow makes `now - created_at` a day; clamping that to
// the 3600 ceiling would file "this box took an hour to come up" as a
// measurement, and a fleet report would then carry a fabricated worst case. A
// negative elapsed is a clock disagreement and is equally not a boot time.
func resolveBootSeconds(createdAt string, now time.Time) *float64 {
	if strings.TrimSpace(createdAt) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return nil
	}
	elapsed := now.Sub(t).Seconds()
	if elapsed < 0 || elapsed > hostedBootBudgetCeilingSeconds {
		return nil
	}
	return &elapsed
}

// resolveOnCreateCompleted answers whether the environment's setup finished.
//
// The three-way split is the point. §2's doctor already refuses to merge the two
// causes of an absent marker in its COPY; this is the same refusal in a value,
// because a report cannot print two sentences.
func resolveOnCreateCompleted(markerPresent bool, contractDeclared bool) *bool {
	if markerPresent {
		return boolPtr(true)
	}
	if contractDeclared {
		// The image says it writes a marker. It did not. Setup failed.
		return boolPtr(false)
	}
	// No marker and no claim that there would be one: nobody was recording.
	return nil
}

// resolveTreeMatches maps the adopt verdict onto the report.
//
// `unverified` and `unreadable` are nil, NOT false, and the difference is a
// state that blames our provisioning: the route turns `treeMatches: false` into
// `tree-mismatch`, which is one of only two verdicts that say the environment
// blocked the candidate's work. §2.1's check is inert wherever the catalog
// carries no expectedTreeSha, so `false` there would report every such session
// as a broken box AND hand a real empty submission an excuse.
func resolveTreeMatches(state string) *bool {
	switch state {
	case hostedTreeVerified:
		return boolPtr(true)
	case hostedTreeMismatch:
		return boolPtr(false)
	default:
		return nil
	}
}

func boolPtr(b bool) *bool { return &b }

// buildHostedBootReport assembles the four facts. Pure, so the collapse this
// whole task is about is testable without a codespace or a network.
func buildHostedBootReport(
	markerPresent bool,
	contractDeclared bool,
	treeState string,
	facts *codespaceFacts,
	now time.Time,
) hostedBootReport {
	r := hostedBootReport{
		OnCreateCompleted: resolveOnCreateCompleted(markerPresent, contractDeclared),
		TreeMatches:       resolveTreeMatches(treeState),
	}
	if facts != nil {
		// Note this copies GitHub's pointer through as-is: a documented-nullable
		// field that came back null stays null.
		r.Prebuilt = facts.Prebuild
		r.BootSeconds = resolveBootSeconds(facts.CreatedAt, now)
	}
	return r
}

// apiReportHostedBoot posts the report. §3.4 keeps this OFF /v1/hooks/ingest on
// purpose: that path normalises to a `kind`, lands in timeline_events and
// becomes rubric evidence, so our own infrastructure failures would enter the
// candidate's replay and grade.
func apiReportHostedBoot(sessionToken string, r hostedBootReport) (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/hosted-boot", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		msg, _ := errBody["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("%s", msg)
	}
	var body struct {
		Verdict struct {
			State string `json:"state"`
		} `json:"verdict"`
	}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	return body.Verdict.State, nil
}

// hostedBootReportedPath is the once-only sentinel, beside the setup marker and
// outside the workspace for the same reason.
func hostedBootReportedPath() string {
	return filepath.Join(filepath.Dir(hostedSetupMarkerPath()), "hosted-boot-reported")
}

// hostedBootAlreadyReported reports whether THIS session has already filed.
//
// Once-only is a correctness requirement, not a politeness one. The route
// overwrites rather than appends, and `bootSeconds` is measured from codespace
// creation to now — so a second `start` an hour in would overwrite a good
// measurement with a worse one, and a second one a day in would overwrite it
// with `null`. The first report is the only one that is about the boot.
//
// Keyed by session id so a genuinely new session in a reused box still reports.
func hostedBootAlreadyReported(sessionID string) bool {
	data, err := os.ReadFile(hostedBootReportedPath())
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == strings.TrimSpace(sessionID)
}

func markHostedBootReported(sessionID string) {
	path := hostedBootReportedPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	os.WriteFile(path, []byte(sessionID), 0o644) //nolint:errcheck
}

// reportHostedBoot files the boot report for a hosted session. Best-effort.
//
// Silent on failure BY DESIGN, and the design is the server's: a report that
// never arrives lands as `uninstrumented-start`, which §3.4 keeps distinct from
// every healthy state precisely so an absence is visible on the other side.
// There is nothing here for the candidate to act on and no reason to spend a
// line of their terminal on our telemetry — but `--verbose` says everything,
// because "the report was not sent" and "the report said the box is fine" must
// stay distinguishable to whoever is debugging the lane.
func reportHostedBoot(session Session) {
	if strings.TrimSpace(session.SessionToken) == "" {
		verbosef("hosted-boot: no session token, not reporting")
		return
	}
	if hostedBootAlreadyReported(session.SessionID) {
		verbosef("hosted-boot: already reported for this session, skipping")
		return
	}

	_, markerPresent := readHostedSetupMarker()
	facts, err := fetchCodespaceFacts(codespaceName(), os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		// Expected, not exceptional — see fetchCodespaceFacts. prebuilt and
		// bootSeconds go out as null.
		verbosef("hosted-boot: could not read codespace facts (%v) — reporting prebuilt/bootSeconds as unknown", err)
	}

	report := buildHostedBootReport(
		markerPresent,
		imageCarriesSetupMarkerContract(),
		session.TreeVerification,
		facts,
		time.Now().UTC(),
	)
	verbosef("hosted-boot: %s", describeHostedBootReport(report))

	state, err := apiReportHostedBoot(session.SessionToken, report)
	if err != nil {
		verbosef("hosted-boot: report failed: %v", err)
		return
	}
	verbosef("hosted-boot: server verdict %q", state)
	markHostedBootReported(session.SessionID)
}

// describeHostedBootReport renders the report for --verbose, printing "unknown"
// where a value is nil. A log line that showed `false` for both would make the
// one thing this change is about invisible in exactly the place someone would go
// looking for it.
func describeHostedBootReport(r hostedBootReport) string {
	b := func(p *bool) string {
		if p == nil {
			return "unknown"
		}
		return strconv.FormatBool(*p)
	}
	boot := "unknown"
	if r.BootSeconds != nil {
		boot = fmt.Sprintf("%.1fs", *r.BootSeconds)
	}
	return fmt.Sprintf(
		"prebuilt=%s onCreateCompleted=%s bootSeconds=%s treeMatches=%s",
		b(r.Prebuilt), b(r.OnCreateCompleted), boot, b(r.TreeMatches),
	)
}
