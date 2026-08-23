package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withTestHTTPClient points the package httpClient at a test server for the
// duration of one test.
func withTestHTTPClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = prev })
}

// The happy path, and the wire contract it depends on: codex sends the token as
// `Authorization: Bearer`, and the proxy POSTs to <base_url>/responses.
func TestSmokeTestCodexProxy_SendsBearerToResponsesPath(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	if err := smokeTestCodexProxy(srv.URL+"/v1/proxy/openai/v1", "PST-TEST-KEY9"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if gotPath != "/v1/proxy/openai/v1/responses" {
		t.Errorf("path = %q, want /v1/proxy/openai/v1/responses", gotPath)
	}
	if gotAuth != "Bearer PST-TEST-KEY9" {
		t.Errorf("Authorization = %q, want Bearer PST-TEST-KEY9", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	// Below the Responses API floor of 16 the request is rejected for a reason
	// that has nothing to do with the proxy, which would fail the check wrongly.
	if n, ok := gotBody["max_output_tokens"].(float64); !ok || n < 16 {
		t.Errorf("max_output_tokens = %v, want >= 16", gotBody["max_output_tokens"])
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false — the check parses a plain JSON body", gotBody["stream"])
	}
	if gotBody["input"] == nil {
		t.Error("input is required by the Responses API and was not sent")
	}
}

// The failure this whole check exists for: the hiring team's OpenAI key is
// rejected upstream. The candidate must see the reason, not a generic HTTP 401.
func TestSmokeTestCodexProxy_SurfacesByokAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// The OpenAI proxy's envelope differs from the Anthropic one: it carries a
		// top-level "type" alongside the error object.
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"byok_authentication_failed","message":"The OpenAI API key configured for this assessment was rejected. Please contact the recruiter who sent you this assessment."}}`))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	err := smokeTestCodexProxy(srv.URL, "PST-TEST-KEY9")
	if err == nil {
		t.Fatal("expected an error for a rejected BYOK key")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the status, got %q", err)
	}
	if !strings.Contains(err.Error(), "contact the recruiter") {
		t.Errorf("error should carry the proxy's message, got %q", err)
	}
}

func TestSmokeTestCodexProxy_SurfacesBudgetExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"token_budget_exceeded","message":"Your AI token budget of $25.00 for this assessment has been exceeded"}}`))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	err := smokeTestCodexProxy(srv.URL, "PST-TEST-KEY9")
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected the budget message to survive, got %v", err)
	}
}

// A body we cannot parse must still produce something readable rather than an
// empty error the candidate cannot act on.
func TestSmokeTestCodexProxy_UnparseableBodyStillReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway timeout</html>"))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	err := smokeTestCodexProxy(srv.URL, "PST-TEST-KEY9")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("want status and raw body in the message, got %q", err)
	}
}

func TestSmokeTestCodexProxy_UnreachableProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	withTestHTTPClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))

	err := smokeTestCodexProxy(url, "PST-TEST-KEY9")
	if err == nil || !strings.Contains(err.Error(), "could not reach proxy") {
		t.Fatalf("want a reachability error, got %v", err)
	}
}

// Both providers share proxyHTTPError, so it must read the Anthropic envelope
// too — that one has no top-level "type".
func TestProxyHTTPError_ReadsAnthropicEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	err := smokeTestProxy(srv.URL, "PST-TEST-KEY9")
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("want the Anthropic error message, got %v", err)
	}
}

// The Claude check had no coverage at all before this change; pin its wire
// contract so the shared error path cannot regress it.
func TestSmokeTestProxy_SendsApiKeyToMessagesPath(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		gotVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer srv.Close()
	withTestHTTPClient(t, srv)

	if err := smokeTestProxy(srv.URL+"/v1/proxy/anthropic", "PST-TEST-KEY9"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if gotPath != "/v1/proxy/anthropic/v1/messages" {
		t.Errorf("path = %q, want /v1/proxy/anthropic/v1/messages", gotPath)
	}
	if gotKey != "PST-TEST-KEY9" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header must be sent")
	}
}
