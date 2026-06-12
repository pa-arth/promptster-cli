package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// configureProxyEnv configures the Claude Code project-level settings so the
// parent claude process AND any child it spawns (hooks, MCP servers) route
// through the Promptster proxy — from settings.local.json alone, with nothing
// exported into the interactive shell.
//
// History: pre-1.2.0 the proxy creds were exported into the shell (originally
// inlined into ~/.promptster/shell-hook.sh, later loaded dynamically via
// `promptster env`). That dragged in a PWD-gated hook block, per-prompt
// eviction, and an `env --clear` escape hatch — all to keep a live token out
// of unrelated shells. The apiKeyHelper approach below feeds the credential to
// the parent claude auth resolution directly, so the shell never touches it.
func configureProxyEnv(workspacePath, proxyURL, sessionToken string) {
	if err := configureClaudeProxy(workspacePath, proxyURL, sessionToken); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not configure Claude Code proxy: %v\n", err)
	}
}

// configureClaudeProxy points Claude Code's project-local settings at the
// Promptster proxy using an apiKeyHelper instead of a baked-in token:
//
//   - apiKeyHelper:            `<promptster-bin> auth-token` — emits the bare
//     PST token from the 0600 session.json on demand. Claude Code sends its
//     stdout as the credential on BOTH `Authorization: Bearer` and `X-Api-Key`,
//     and — unlike the `env` block — feeds the PARENT process's auth resolution.
//   - env.ANTHROPIC_BASE_URL:  the proxy URL (both parent and child read this).
//
// We deliberately do NOT set ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY here:
// either would out-rank apiKeyHelper, and the whole point is to source the
// token from session.json via the helper rather than bake it into this file.
// A logged-in Claude subscription (OAuth in ~/.claude.json) sits BELOW the
// apiKeyHelper in Claude Code's credential precedence, so the helper wins and
// the candidate does not need to `/logout` — the only thing that out-ranks the
// helper is an exported ANTHROPIC_AUTH_TOKEN/API_KEY (which is why we strip any
// baked-in token from the env block just below).
func configureClaudeProxy(workspacePath, proxyURL, sessionToken string) error {
	path := claudeProjectSettingsPath(workspacePath)

	existing := make(map[string]interface{})
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}

	envSection, _ := existing["env"].(map[string]interface{})
	if envSection == nil {
		envSection = make(map[string]interface{})
	}
	envSection["ANTHROPIC_BASE_URL"] = proxyURL
	// Strip any token a prior CLI baked in: it would out-rank apiKeyHelper and
	// re-introduce the on-disk-secret leak the helper exists to avoid.
	delete(envSection, "ANTHROPIC_AUTH_TOKEN")
	delete(envSection, "ANTHROPIC_API_KEY")
	existing["env"] = envSection

	// apiKeyHelper runs via /bin/sh; the binary path may contain spaces, so
	// quote it. The helper prints the bare PST token (or nothing if the session
	// is gone/expired, which Claude Code treats as "no helper credential").
	existing["apiKeyHelper"] = shellQuote(promptsterBin()) + " auth-token"

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return os.Rename(tmp, path)
}

// removeClaudeProxyConfig strips Promptster's proxy wiring (apiKeyHelper +
// env.ANTHROPIC_BASE_URL) from the workspace's Claude settings. Used by
// BYO-subscription mode, where the candidate's own subscription must win the
// credential resolution: a stale apiKeyHelper from a previous managed session
// OUT-RANKS subscription OAuth and would silently re-route traffic.
func removeClaudeProxyConfig(workspacePath string) error {
	path := claudeProjectSettingsPath(workspacePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	existing := make(map[string]interface{})
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	changed := false
	if _, ok := existing["apiKeyHelper"]; ok {
		delete(existing, "apiKeyHelper")
		changed = true
	}
	if envSection, ok := existing["env"].(map[string]interface{}); ok {
		for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
			if _, has := envSection[k]; has {
				delete(envSection, k)
				changed = true
			}
		}
		if len(envSection) == 0 {
			delete(existing, "env")
		} else {
			existing["env"] = envSection
		}
	}
	if !changed {
		return nil
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return os.Rename(tmp, path)
}
