package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// NOTE: there is deliberately no subscription-detection / forced-`/logout`
// step here. Claude Code's credential precedence puts an `apiKeyHelper`
// (positions: cloud creds > ANTHROPIC_AUTH_TOKEN > ANTHROPIC_API_KEY >
// apiKeyHelper > CLAUDE_CODE_OAUTH_TOKEN > subscription OAuth) ABOVE a
// logged-in Max/Pro subscription, so the proxy wins over the candidate's own
// subscription with no logout required (empirically confirmed 2026-05-30).
// The only thing that out-ranks the helper is a shell- or settings-exported
// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY — that case is caught by
// `promptster doctor` and the proxy-config strip in configureClaudeProxy.

// smokeTestProxy fires one minimal Messages API request through the Promptster
// proxy to verify the session token works and the proxy is reachable. Returns
// nil on success, or a short human-readable error on failure. Cost is ~5
// Haiku tokens (fractions of a cent).
func smokeTestProxy(proxyURL, sessionToken string) error {
	body := map[string]interface{}{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 4,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", sessionToken)
	req.Header.Set("anthropic-version", "2023-06-01")

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

// proxyHTTPError renders a non-2xx proxy response as a short human-readable
// error. Shared by both provider smoke tests: the Anthropic proxy returns
// {"error":{...}} and the OpenAI one {"type":"error","error":{...}}, so reading
// `.error.message` covers both, with the raw body as the fallback.
func proxyHTTPError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(bodyBytes, &parsed)
	msg := parsed.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(bodyBytes))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
}

// printProxySmokeTestFailure prints a styled warning block when the smoke test
// fails. Non-fatal — the candidate can still try to work, but we want them to
// see the problem here rather than on their first prompt in Claude Code.
func printProxySmokeTestFailure(err error) {
	printAlertBox("Claude API proxy check failed", []string{
		err.Error(),
		"",
		"Claude Code may not work until this is resolved.",
		"Common causes:",
		"  • Network / VPN blocking " + apiHost(),
		"  • Session key expired — rerun:  promptster start PST-XXXX",
		"  • Assessment budget exhausted — contact the recruiter",
	}, alertWarn)
}

// Alert severities control the border color of printAlertBox.
const (
	alertWarn  = "warn"
	alertError = "error"
	alertInfo  = "info"
)

// printAlertBox renders a bordered callout box with a title and body lines.
// Used for pre-flight warnings (e.g. proxy unreachable) that the candidate
// needs to see but that don't block startup.
func printAlertBox(title string, lines []string, severity string) {
	var borderColor lipgloss.Color
	switch severity {
	case alertError:
		borderColor = lipgloss.Color("#ef4444")
	case alertInfo:
		borderColor = lipgloss.Color("#38bdf8")
	default:
		borderColor = lipgloss.Color("#eab308")
	}

	titleStyle := lipgloss.NewStyle().
		Foreground(borderColor).
		Bold(true).
		PaddingBottom(1)

	bodyStyle := lipgloss.NewStyle().
		Foreground(cBody).
		Width(64)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(1, 2).
		Width(70)

	var content strings.Builder
	content.WriteString(titleStyle.Render(title))
	content.WriteString("\n")
	content.WriteString(bodyStyle.Render(strings.Join(lines, "\n")))

	fmt.Println()
	fmt.Println(boxStyle.Render(content.String()))
	fmt.Println()
}
