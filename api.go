package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultAPIURL = "https://api.promptster.ai"

func apiURL() string {
	if u := os.Getenv("PROMPTSTER_API_URL"); u != "" {
		return u
	}
	return defaultAPIURL
}

// usingDefaultAPI reports whether the CLI is pointed at the hosted Promptster
// API (vs a self-hosted backend via PROMPTSTER_API_URL). Used to suppress
// hosted-service-specific hints (installer URLs etc.) for third-party setups.
func usingDefaultAPI() bool {
	return apiURL() == defaultAPIURL
}

// apiHost returns the host portion of the configured API URL for display in
// diagnostics, falling back to the raw URL if it doesn't parse.
func apiHost() string {
	if u, err := url.Parse(apiURL()); err == nil && u.Host != "" {
		return u.Host
	}
	return apiURL()
}

// versionTransport injects X-Promptster-CLI-Version on every outbound request.
type versionTransport struct {
	base http.RoundTripper
}

func (t *versionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Promptster-CLI-Version", version)
	return t.base.RoundTrip(req)
}

var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &versionTransport{base: http.DefaultTransport},
}

// RedeemResponse is the response from POST /v1/candidate/redeem.
type RedeemResponse struct {
	SessionID         string `json:"sessionId"`
	SessionToken      string `json:"sessionToken"`
	AssessmentID      string `json:"assessmentId"`
	AssessmentTitle   string `json:"assessmentTitle"`
	OrgName           string `json:"orgName"`
	TaskBrief         string `json:"taskBrief"`
	RepoURL           string `json:"repoUrl"`
	RepoCommit        string `json:"repoCommit"`
	SetupInstructions string `json:"setupInstructions"`
	RepoSubdir        string `json:"repoSubdir"`
	TimeLimitMinutes  int    `json:"timeLimitMinutes"`
	IssueID           string `json:"issueId"`
	// ExpiresAt is the candidate-key expiration timestamp. The shell hook uses
	// this for local staleness checks so it can self-evict on stale sessions
	// without an API round-trip per new shell.
	ExpiresAt string `json:"expiresAt"`
	// TosAccepted is true when the candidate already accepted the ToS via the
	// web flow. When true, the CLI skips the inline consent prompt.
	TosAccepted bool `json:"tosAccepted"`
	// AllowedTools is the recruiter-chosen subset of {claude,codex,cursor} the
	// candidate is permitted to instrument for this assessment. Empty/nil means
	// the server didn't send it (older API) — the CLI falls back to all tools.
	AllowedTools []string `json:"allowedTools"`
}

func apiRedeem(key, candidateName, signingPubKey string) (RedeemResponse, error) {
	body := map[string]interface{}{"key": key}
	if candidateName != "" {
		body["candidateName"] = candidateName
	}
	if signingPubKey != "" {
		body["signingPubKey"] = signingPubKey
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/redeem", bytes.NewReader(data))
	if err != nil {
		return RedeemResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return RedeemResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		msg, _ := errBody["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return RedeemResponse{}, fmt.Errorf("%s", msg)
	}

	var result RedeemResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return RedeemResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// CompleteResponse is the response from POST /v1/candidate/complete.
type CompleteResponse struct {
	OK          bool   `json:"ok"`
	CompletedAt string `json:"completedAt"`
}

func apiComplete(sessionID, key string) (CompleteResponse, error) {
	data, _ := json.Marshal(map[string]string{"sessionId": sessionID, "key": key})

	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/complete", bytes.NewReader(data))
	if err != nil {
		return CompleteResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return CompleteResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		msg, _ := errBody["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return CompleteResponse{}, fmt.Errorf("%s", msg)
	}

	var result CompleteResponse
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	return result, nil
}

func apiSaveWorkspaceCommit(sessionID, sessionToken, commitSha string) error {
	data, _ := json.Marshal(map[string]string{
		"sessionId": sessionID,
		"commitSha": commitSha,
	})

	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/workspace-commit", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		msg, _ := errBody["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", msg)
	}

	return nil
}

// apiConfirmConsent calls POST /v1/candidate/consent to mark consent confirmed server-side.
func apiConfirmConsent(key string) error {
	body := []byte(`{"key":"` + key + `"}`)
	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/consent", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("consent confirmation failed (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ConsentDisclosure mirrors the canonical disclosure served by the API. Both
// the web consent page and this CLI render these plain strings — the API is the
// single source of truth so the lists can't drift apart.
type ConsentDisclosure struct {
	Captures       []string `json:"captures"`
	DoesNotCapture []string `json:"doesNotCapture"`
	Evaluated      []string `json:"evaluated"`
	TosURL         string   `json:"tosUrl"`
}

// ConsentInfo is the subset of GET /v1/candidate/consent the CLI consumes:
// whether consent was already given (e.g. via the web flow) plus the canonical
// disclosure text to render.
type ConsentInfo struct {
	AlreadyConfirmed bool              `json:"alreadyConfirmed"`
	Disclosure       ConsentDisclosure `json:"disclosure"`
}

// apiConsentInfo fetches consent state + canonical disclosure via
// GET /v1/candidate/consent. Best-effort by design: on any error the caller
// falls back to the embedded disclosure and treats consent as not-yet-confirmed,
// so the consent screen always works even if the API is briefly unreachable.
func apiConsentInfo(key string) (ConsentInfo, error) {
	var info ConsentInfo
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/candidate/consent?key="+url.QueryEscape(key), nil)
	if err != nil {
		return info, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return info, fmt.Errorf("consent info failed (status %d)", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, fmt.Errorf("decode response: %w", err)
	}
	return info, nil
}

// apiGetAssessment fetches assessment details via GET /v1/assessments/:id with API key auth.
func apiGetAssessment(assessmentID, sessionToken string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/assessments/"+assessmentID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	return result, nil
}

// apiGetIssue fetches OSS issue details via GET /v1/issues/:id with API key auth.
func apiGetIssue(issueID, sessionToken string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/issues/"+issueID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	return result, nil
}

// DeviceCheckRequest is the body for POST /v1/candidate/device-check.
type DeviceCheckRequest struct {
	Checkpoint  string            `json:"checkpoint"`
	Fingerprint DeviceFingerprint `json:"fingerprint"`
}

func apiDeviceCheck(sessionToken string, body DeviceCheckRequest) error {
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/device-check", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		msg, _ := errBody["error"].(string)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", msg)
	}

	return nil
}

// apiHealth pings GET /v1/health. Returns nil on 200 OK.
func apiHealth() error {
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/health", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &versionTransport{base: http.DefaultTransport}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// UploadURLResponse is the response from POST /v1/candidate/upload-url.
type UploadURLResponse struct {
	UploadURL string `json:"uploadUrl"`
	Token     string `json:"token"`
	ObjectKey string `json:"objectKey"`
	ExpiresAt string `json:"expiresAt"`
}

// apiGetUploadURL requests a signed PUT URL for the candidate's tarball.
func apiGetUploadURL(sessionToken, sessionID string) (UploadURLResponse, error) {
	body, _ := json.Marshal(map[string]string{"sessionId": sessionID})
	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/candidate/upload-url", bytes.NewReader(body))
	if err != nil {
		return UploadURLResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return UploadURLResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return UploadURLResponse{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result UploadURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return UploadURLResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// uploadBundle PUTs the tarball at tarballPath to a pre-signed URL. The URL
// already contains auth — no headers required beyond Content-Type.
// Retries 3x with exponential backoff on 5xx / network errors. Fails fast on 4xx.
func uploadBundle(uploadURL, tarballPath string, size int64) error {
	// Separate client with a longer timeout — uploads can run minutes on slow
	// links; the default 15s timeout is for short JSON RPCs.
	client := &http.Client{Timeout: 5 * time.Minute}

	backoffs := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	for attempt := 0; ; attempt++ {
		err := uploadBundleOnce(client, uploadURL, tarballPath, size)
		if err == nil {
			return nil
		}
		// Retry only on retryable conditions (server-side 5xx, network).
		if isRetryable(err) && attempt < len(backoffs) {
			time.Sleep(backoffs[attempt])
			continue
		}
		return err
	}
}

func uploadBundleOnce(client *http.Client, uploadURL, tarballPath string, size int64) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()

	req, err := http.NewRequest(http.MethodPut, uploadURL, f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := client.Do(req)
	if err != nil {
		return &uploadError{retryable: true, msg: "network error: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	return &uploadError{retryable: resp.StatusCode >= 500, msg: msg}
}

type uploadError struct {
	retryable bool
	msg       string
}

func (e *uploadError) Error() string { return e.msg }

func isRetryable(err error) bool {
	var ue *uploadError
	if errors.As(err, &ue) {
		return ue.retryable
	}
	return false
}

// apiGetSession fetches session details via GET /v1/sessions/:id with API key auth.
func apiGetSession(sessionID, apiKey string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/sessions/"+sessionID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
	return result, nil
}
