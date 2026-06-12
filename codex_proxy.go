package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Codex has no apiKeyHelper-style credential hook (Claude Code's escape hatch).
// A custom model provider in ~/.codex/config.toml is the only way to route its
// Responses-API traffic through the Promptster proxy, and that config is GLOBAL
// — codex won't scope a provider to a workspace. So unlike the Claude path,
// where a crash leaves no mess (workspace-scoped settings), a crash here can
// leave a live `model_provider = "promptster"` in the user's global config,
// silently breaking their personal codex everywhere. Every mutation is therefore
// marker-fenced and backed by a sidecar state file so revert is exact, and a
// reconciler strips a block whose owning session is gone.

const codexProxyProviderID = "promptster"

// Marker substring shared with the shell hook (shell_hook.go greps for it to
// decide whether to export PROMPTSTER_PROXY_TOKEN). Keep these in sync.
const codexProxyMarker = "promptster proxy (managed)"
const codexProxyMarkerBegin = "# >>> " + codexProxyMarker + " >>>"
const codexProxyMarkerEnd = "# <<< " + codexProxyMarker + " <<<"

// codexConfigPath returns $CODEX_HOME/config.toml (or ~/.codex/config.toml).
func codexConfigPath() string {
	return filepath.Join(codexHome(), "config.toml")
}

// codexProxyState records what the global config looked like before we touched
// it, so revert is exact. Persisted at ~/.promptster/codex-proxy-state.json.
type codexProxyState struct {
	ConfigPath        string `json:"configPath"`
	HadConfig         bool   `json:"hadConfig"`
	HadModelProvider  bool   `json:"hadModelProvider"`
	PrevModelProvider string `json:"prevModelProvider,omitempty"`
}

func codexProxyStatePath() string { return filepath.Join(stateDir(), "codex-proxy-state.json") }

func loadCodexProxyState() (codexProxyState, bool) {
	data, err := os.ReadFile(codexProxyStatePath())
	if err != nil {
		return codexProxyState{}, false
	}
	var s codexProxyState
	if err := json.Unmarshal(data, &s); err != nil {
		return codexProxyState{}, false
	}
	return s, true
}

func saveCodexProxyState(s codexProxyState) error {
	if err := os.MkdirAll(filepath.Dir(codexProxyStatePath()), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(codexProxyStatePath(), data, 0o600)
}

// rootModelProviderRe matches a top-level `model_provider = "..."` assignment.
// Applied only to the region before the first [table] header so we never touch
// a provider key scoped inside a [profiles.x] / [model_providers.x] table.
var rootModelProviderRe = regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=[ \t]*"([^"]*)"[ \t]*$`)

// codexProxyBlock renders the marker-fenced managed block. base_url is the proxy
// prefix; codex (wire_api = "responses") POSTs to <base_url>/responses.
func codexProxyBlock(baseURL string) string {
	return strings.Join([]string{
		codexProxyMarkerBegin,
		fmt.Sprintf("model_provider = %q", codexProxyProviderID),
		"",
		fmt.Sprintf("[model_providers.%s]", codexProxyProviderID),
		`name = "Promptster"`,
		fmt.Sprintf("base_url = %q", baseURL),
		`wire_api = "responses"`,
		`env_key = "PROMPTSTER_PROXY_TOKEN"`,
		"requires_openai_auth = false",
		codexProxyMarkerEnd,
	}, "\n")
}

// stripCodexProxyBlock removes the marker-fenced managed block (and any blank
// line immediately following it) from config content. Returns the cleaned
// content and whether a block was present.
func stripCodexProxyBlock(content string) (string, bool) {
	begin := strings.Index(content, codexProxyMarkerBegin)
	if begin == -1 {
		return content, false
	}
	endMarker := strings.Index(content, codexProxyMarkerEnd)
	if endMarker == -1 {
		// Corrupt/half-written block: drop from the begin marker to EOF rather
		// than leaving a dangling provider definition.
		return strings.TrimRight(content[:begin], "\n"), true
	}
	end := endMarker + len(codexProxyMarkerEnd)
	// Swallow the trailing newline(s) we inserted as a separator so revert is
	// byte-exact (configure writes "<block>\n\n<rest>").
	for end < len(content) && content[end] == '\n' {
		end++
	}
	cleaned := content[:begin] + content[end:]
	return cleaned, true
}

// codexProxyConfigured reports whether the global config currently carries our
// managed block.
func codexProxyConfigured() bool {
	data, err := os.ReadFile(codexConfigPath())
	if err != nil {
		return false
	}
	return strings.Contains(string(data), codexProxyMarkerBegin)
}

// configureCodexProxy installs (or refreshes) the Promptster custom model
// provider in the user's global codex config and selects it via model_provider.
// Idempotent: re-applying first strips any prior managed block. The proxy token
// is NOT written here — it rides PROMPTSTER_PROXY_TOKEN, exported PWD-gated by
// the shell hook from the 0600 session.json.
func configureCodexProxy(baseURL string) error {
	path := codexConfigPath()

	raw, readErr := os.ReadFile(path)
	hadConfig := readErr == nil
	content := ""
	if hadConfig {
		content = string(raw)
	}

	// Idempotent re-apply: remove any block we previously wrote.
	content, hadBlock := stripCodexProxyBlock(content)

	// Capture (once) the user's pre-existing root model_provider so we can
	// restore it on revert, then remove it — our block sets its own. Only the
	// region before the first [table] header counts as "root".
	state, hadState := loadCodexProxyState()
	if !hadState {
		state = codexProxyState{ConfigPath: path, HadConfig: hadConfig}
	}
	// Don't overwrite the originally-captured value on a refresh (hadBlock means
	// our block — and thus our model_provider — was already in place, so any
	// root model_provider we'd find now is our own, already stripped above).
	if !hadBlock {
		root, rest := splitRootRegion(content)
		if m := rootModelProviderRe.FindStringSubmatch(root); m != nil && m[1] != codexProxyProviderID {
			state.HadModelProvider = true
			state.PrevModelProvider = m[1]
			root = rootModelProviderRe.ReplaceAllString(root, "")
			content = root + rest
		}
	}

	block := codexProxyBlock(baseURL)
	trimmed := strings.TrimLeft(content, "\n")
	var next string
	if trimmed == "" {
		next = block + "\n"
	} else {
		next = block + "\n\n" + trimmed
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o644); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return saveCodexProxyState(state)
}

// revertCodexProxy removes the managed block and restores the user's prior
// model_provider. If we created config.toml from scratch and nothing else is
// left, the file is deleted. Safe to call when nothing was configured.
func revertCodexProxy() {
	path := codexConfigPath()
	state, hadState := loadCodexProxyState()
	if state.ConfigPath != "" {
		path = state.ConfigPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		// Nothing to revert (file gone). Clear any stale state.
		_ = os.Remove(codexProxyStatePath())
		return
	}
	content := string(raw)
	content, _ = stripCodexProxyBlock(content)

	// Restore the user's original root model_provider, if we displaced one.
	if hadState && state.HadModelProvider && state.PrevModelProvider != "" {
		if rootModelProviderRe.FindString(content) == "" {
			restore := fmt.Sprintf("model_provider = %q\n", state.PrevModelProvider)
			content = restore + strings.TrimLeft(content, "\n")
		}
	}

	// If we created the file and it's now effectively empty, remove it so we
	// leave the user's machine exactly as we found it.
	if hadState && !state.HadConfig && strings.TrimSpace(content) == "" {
		_ = os.Remove(path)
		_ = os.Remove(codexProxyStatePath())
		return
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
	_ = os.Remove(codexProxyStatePath())
}

// reconcileCodexProxyIfStale strips the managed block when no live session owns
// it — the safety net for a crash that left a global `model_provider =
// "promptster"` pointing at a dead session (which would otherwise break the
// user's personal codex everywhere). Called from `start` (before configuring)
// and `doctor`.
func reconcileCodexProxyIfStale() {
	if !codexProxyConfigured() {
		return
	}
	s, err := loadSession()
	stale := err != nil || s.SessionToken == "" || s.TaskRoot == ""
	if !stale && !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt) {
		stale = true
	}
	if stale {
		revertCodexProxy()
	}
}

// splitRootRegion splits content into (root, rest) at the first TOML table
// header line (a line starting with "["). Root keys must precede any table, so
// this is where a top-level model_provider lives.
func splitRootRegion(content string) (root string, rest string) {
	lines := splitKeepNewlines(content)
	idx := 0
	for ; idx < len(lines); idx++ {
		if strings.HasPrefix(strings.TrimSpace(lines[idx]), "[") {
			break
		}
	}
	return strings.Join(lines[:idx], ""), strings.Join(lines[idx:], "")
}

// splitKeepNewlines splits content into lines, each retaining its trailing "\n".
func splitKeepNewlines(content string) []string {
	var out []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			out = append(out, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		out = append(out, content[start:])
	}
	return out
}
