package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Codex has no apiKeyHelper-style credential hook (Claude Code's escape hatch),
// and its config.toml is GLOBAL — codex will not scope a model provider to a
// workspace. Promptster used to write `model_provider = "promptster"` into that
// global file for the duration of a session. That was wrong twice over:
//
//  1. It hijacked EVERY codex run on the machine, not just the ones inside the
//     assessment workspace. A candidate's own codex, in their own project, in
//     another terminal, started demanding PROMPTSTER_PROXY_TOKEN — a variable
//     only the assessment workspace ever had.
//
//  2. It outlived the session. `done` and `abort` reverted it, but a crash, a
//     closed terminal, an expired key nobody came back to, or a wiped
//     ~/.promptster left `model_provider = "promptster"` live forever, and codex
//     stayed broken on that machine until someone hand-edited TOML. The sidecar
//     state file that made revert exact was itself in the directory that gets
//     wiped, so the "exact" revert was the first thing to disappear.
//
// So NOTHING is written to ~/.codex/config.toml any more. The provider is passed
// per launch as `-c` overrides by `promptster codex` (cmd_codex.go), which codex
// overlays on top of whatever config the user has. The blast radius is now
// exactly one process: there is no persistent state, so there is nothing a crash
// can strand and nothing a teardown can miss.
//
// What is left in this file is the overlay builder plus a PURGE for the block
// older versions wrote, so installing this version heals a machine that a
// 1.9-or-earlier session left broken.

const codexProxyProviderID = "promptster"

// Marker substring written by CLI ≤1.9 into ~/.codex/config.toml. Nothing
// writes it any more; it is kept solely so purgeLegacyCodexProxyBlock can
// recognise and remove what those versions left behind.
const codexProxyMarker = "promptster proxy (managed)"
const codexProxyMarkerBegin = "# >>> " + codexProxyMarker + " >>>"
const codexProxyMarkerEnd = "# <<< " + codexProxyMarker + " <<<"

// codexConfigPath returns $CODEX_HOME/config.toml (or ~/.codex/config.toml).
func codexConfigPath() string {
	return filepath.Join(codexHome(), "config.toml")
}

// codexProxyArgs returns the `-c key=value` overrides that select the Promptster
// provider for ONE codex launch. baseURL is the proxy prefix; codex
// (wire_api = "responses") POSTs to <baseURL>/responses.
//
// Values are TOML literals because that is how codex parses the right-hand side
// of `-c`. They are passed as separate argv entries by the caller, so no shell
// quoting is involved.
//
// The credential is NOT here. It rides PROMPTSTER_PROXY_TOKEN in the child's
// environment, named by env_key and set by cmdCodex from the 0600 session.json —
// so it never lands in a config file and never enters the user's shell.
func codexProxyArgs(baseURL string) []string {
	kv := []string{
		fmt.Sprintf("model_provider=%q", codexProxyProviderID),
		fmt.Sprintf("model_providers.%s.name=%q", codexProxyProviderID, "Promptster"),
		fmt.Sprintf("model_providers.%s.base_url=%q", codexProxyProviderID, baseURL),
		fmt.Sprintf("model_providers.%s.wire_api=%q", codexProxyProviderID, "responses"),
		fmt.Sprintf("model_providers.%s.env_key=%q", codexProxyProviderID, codexProxyTokenEnv),
		fmt.Sprintf("model_providers.%s.requires_openai_auth=false", codexProxyProviderID),
	}
	args := make([]string, 0, len(kv)*2)
	for _, pair := range kv {
		args = append(args, "-c", pair)
	}
	return args
}

// codexProxyTokenEnv is the environment variable codex reads the proxy
// credential from (referenced by env_key in the overlay above).
const codexProxyTokenEnv = "PROMPTSTER_PROXY_TOKEN"

// ── Legacy purge ────────────────────────────────────────────────────────────

// codexProxyState is the sidecar CLI ≤1.9 wrote to record the model_provider it
// displaced. Read-only now: we still restore from it when purging, but nothing
// writes one.
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

// rootModelProviderRe matches a top-level `model_provider = "..."` assignment.
// Applied only to the region before the first [table] header so we never touch
// a provider key scoped inside a [profiles.x] / [model_providers.x] table.
var rootModelProviderRe = regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=[ \t]*"([^"]*)"[ \t]*$`)

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
	for end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:begin] + content[end:], true
}

// legacyCodexProxyBlockPresent reports whether the global codex config still
// carries a managed block written by CLI ≤1.9.
func legacyCodexProxyBlockPresent() bool {
	data, err := os.ReadFile(codexConfigPath())
	if err != nil {
		return false
	}
	return strings.Contains(string(data), codexProxyMarkerBegin)
}

// purgeLegacyCodexProxyBlock removes the managed block CLI ≤1.9 wrote into the
// user's GLOBAL codex config and restores the model_provider it displaced.
//
// Unconditional, not session-gated, and safe to call from anywhere: the block is
// no longer written by anything, so its presence is by definition a leftover —
// including the one case that mattered most, a machine whose session died
// without ever running `done`. That is why this is not the old
// "reconcile if the owning session looks stale" check: staleness was decided
// from ~/.promptster, and a wiped ~/.promptster read as "no session", which is
// also what a healthy machine looks like.
//
// Fast path: two failed stats when there is nothing to do.
func purgeLegacyCodexProxyBlock() {
	state, hadState := loadCodexProxyState()
	if !legacyCodexProxyBlockPresent() && !hadState {
		return
	}

	path := codexConfigPath()
	if state.ConfigPath != "" {
		path = state.ConfigPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		_ = os.Remove(codexProxyStatePath())
		return
	}
	content, hadBlock := stripCodexProxyBlock(string(raw))
	if !hadBlock && !hadState {
		return
	}

	// Restore the user's original root model_provider, if we displaced one.
	if hadState && state.HadModelProvider && state.PrevModelProvider != "" {
		if rootModelProviderRe.FindString(content) == "" {
			content = fmt.Sprintf("model_provider = %q\n", state.PrevModelProvider) + strings.TrimLeft(content, "\n")
		}
	}

	// If ≤1.9 created the file and it is now effectively empty, remove it so the
	// machine is left exactly as Promptster found it.
	if hadState && !state.HadConfig && strings.TrimSpace(content) == "" {
		_ = os.Remove(path)
		_ = os.Remove(codexProxyStatePath())
		return
	}

	if hadBlock {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err == nil {
			_ = os.Rename(tmp, path)
		}
	}
	_ = os.Remove(codexProxyStatePath())
}
