package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// Codex has no preflight of its own. Claude Code's proxy is smoke-tested at
// start (claude_auth_check.go) so auth and network failures surface before the
// clock matters; codex-only sessions used to skip that step entirely and find
// out on the candidate's first prompt instead.
//
// That gap was not theoretical. The proxy bumps org_openai_keys.auth_failure_count
// and writes a credential_audit_log row on a 401 — but only when a request
// actually reaches it. With no codex smoke test, nothing reached it until the
// candidate started work, so a rejected org key read as healthy in the
// dashboard indefinitely. One sat dead for two months at auth_failure_count 0.
//
// Checking here turns that into a start-time failure the recruiter can act on.

// smokeTestCodexProxy fires one minimal Responses API request through the
// Promptster codex proxy to verify the session token works, the proxy is
// reachable, and the hiring team's OpenAI key actually resolves and
// authenticates upstream. Returns nil on success, or a short human-readable
// error on failure.
//
// The model is a fixed cheap one rather than whatever codex will actually send.
// This mirrors the Claude check (which pins Haiku) and matches what the test is
// for: reachability, token validity, key resolution, and budget — all of which
// are model-independent. Measured cost is $0.0002 per run (8 in / 16 out),
// which `promptster doctor` now spends too — see cmd_doctor.go.
func smokeTestCodexProxy(baseURL, sessionToken string) error {
	body := map[string]interface{}{
		"model": "gpt-4o-mini",
		"input": "ping",
		// The Responses API floor is 16; anything lower is rejected outright and
		// would fail the check for a reason that has nothing to do with the proxy.
		"max_output_tokens": 16,
		"stream":            false,
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/responses", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Codex sends the env_key value as `Authorization: Bearer`, and the proxy
	// reads that first. Use the same header so this exercises the real path.
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return proxyHTTPError(resp)
}

// printCodexProxySmokeTestFailure prints a styled warning block when the codex
// smoke test fails. Non-fatal — the candidate can still try to work, and the
// rollout-JSONL watcher captures the session either way — but the most likely
// cause is a key only the hiring team can fix, so say so plainly.
func printCodexProxySmokeTestFailure(err error) {
	printAlertBox("Codex API proxy check failed", []string{
		err.Error(),
		"",
		"Codex will not be able to reach a model until this is resolved.",
		"Common causes:",
		"  • The hiring team's OpenAI key was rejected — contact the recruiter",
		"  • Network / VPN blocking " + apiHost(),
		"  • Session key expired — rerun:  promptster start PST-XXXX",
		"  • Assessment budget exhausted — contact the recruiter",
	}, alertWarn)
}
