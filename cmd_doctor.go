package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdDoctor checks the local Promptster setup and prints the status of each
// component. Any misconfiguration is reported with a human-readable fix command.
func cmdDoctor() {
	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, ".promptster", "bin")
	session, sessionErr := loadSession()

	// Tool relevance — only surface a tool's checks when this session actually
	// instruments it. With no session we default to Claude (the historical
	// default) so a fresh machine still gets a meaningful environment check.
	claudeRelevant := sessionErr != nil || hasTool(session.Tools, toolClaude)
	codexRelevant := codexProxyConfigured() || (sessionErr == nil && hasTool(session.Tools, toolCodex))
	// Cursor used to be a third relevant tool here. openspec
	// changes/employer-supplied-model-key retired it as a selectable tool, so
	// there is no Cursor binary check and no Cursor hooks section any more.
	//
	// One case still needs saying out loud: a session.json written by an OLDER
	// CLI can name a retired tool. Reporting nothing there is the wrong silence —
	// the candidate would see a doctor that never mentions the tool their
	// instructions told them to use. Reported below as a stated fact, not a
	// failure: nothing is broken on this machine.
	retiredInSession := sessionErr == nil && hasRetiredTool(session.Tools, session.AllowedTools)

	fmt.Println("Promptster Doctor")
	fmt.Println(strings.Repeat("─", 44))
	fmt.Printf("  CLI version:  %s\n", version)
	fmt.Printf("  API:          %s\n", apiURL())
	fmt.Printf("  Platform:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()

	fmt.Println("Environment")
	check("git", func() (string, string) {
		p, err := exec.LookPath("git")
		if err != nil {
			return "", "git not found in PATH\n    Fix: " + gitInstallHint()
		}
		if ver := getToolVersion("git"); ver != "" {
			return ver, ""
		}
		return p, ""
	})
	if claudeRelevant {
		check("claude binary", func() (string, string) {
			p, err := exec.LookPath("claude")
			if err != nil {
				return "", "claude not found in PATH\n    Fix: " + claudeInstallHint()
			}
			if ver := getToolVersion("claude"); ver != "" {
				return ver, ""
			}
			return p, ""
		})
	}
	if codexRelevant {
		check("codex binary", func() (string, string) {
			p, err := exec.LookPath("codex")
			if err != nil {
				return "", "codex not found in PATH\n    Fix: " + toolInstallHint(toolCodex)
			}
			return p, ""
		})
	}
	if retiredInSession {
		check("cursor (retired)", func() (string, string) {
			return "", "this session names Cursor, which Promptster no longer instruments\n" +
				"    Cursor routes model traffic through its own backend, so the hiring\n" +
				"    team's key cannot be used or metered on it. Nothing is wrong with\n" +
				"    your machine.\n" +
				"    Fix: ask whoever sent you this assessment to switch it to Claude Code\n" +
				"    or Codex. You can still use Cursor as your editor."
		})
	}
	check("promptster binary installed", func() (string, string) {
		bin := promptsterBin()
		info, err := os.Stat(bin)
		if err != nil {
			return "", "not installed at " + bin + "\n    Fix: run promptster start (auto-installs) or re-install CLI"
		}
		if info.Mode()&0o111 == 0 {
			return "", bin + " is not executable\n    Fix: chmod +x " + bin
		}
		return bin, ""
	})
	check("PATH includes ~/.promptster/bin", func() (string, string) {
		for _, d := range filepath.SplitList(os.Getenv("PATH")) {
			if d == binDir {
				return "yes", ""
			}
		}
		rc := "~/.bashrc or ~/.zshrc"
		if runtime.GOOS == "darwin" {
			rc = "~/.zshrc"
		}
		return "", fmt.Sprintf(
			"~/.promptster/bin not in PATH\n    Fix: add to %s:\n      export PATH=\"$HOME/.promptster/bin:$PATH\"",
			rc,
		)
	})
	if claudeRelevant {
		check("Conflicting Anthropic auth in shell", func() (string, string) {
			// As of 1.2.0 the proxy is wired via Claude Code's apiKeyHelper, which
			// has LOWER precedence than these env vars. If the candidate (or a stale
			// pre-1.2.0 session) has either exported, Claude Code uses that instead
			// of the helper — routing around the proxy and breaking capture.
			var found []string
			if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
				found = append(found, "ANTHROPIC_API_KEY")
			}
			if strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")) != "" {
				found = append(found, "ANTHROPIC_AUTH_TOKEN")
			}
			if len(found) == 0 {
				return "none", ""
			}
			return "", strings.Join(found, "/") + " set in this shell — out-ranks the proxy apiKeyHelper\n    Fix: eval \"$(promptster env --clear)\" (or unset it) before opening Claude Code"
		})
	}
	fmt.Println()

	// ── Codex (OpenAI) proxy ────────────────────────────────────────────────
	// Only shown when codex is instrumented for this session or a managed block
	// is present. codex config is GLOBAL, so a block left by a dead session would
	// break the user's personal codex everywhere — reconcile auto-heals that.
	if codexRelevant {
		reconcileCodexProxyIfStale()

		fmt.Println("Codex")
		check("Codex proxy config (model_provider + provider block)", func() (string, string) {
			data, err := os.ReadFile(codexConfigPath())
			if err != nil || !strings.Contains(string(data), codexProxyMarkerBegin) {
				return "", "Promptster provider not in " + codexConfigPath() + "\n    Fix: promptster start --tools codex"
			}
			if !strings.Contains(string(data), fmt.Sprintf("model_provider = %q", codexProxyProviderID)) {
				return "", "model_provider not set to " + codexProxyProviderID + " in " + codexConfigPath() + "\n    Fix: re-run promptster start --tools codex"
			}
			return codexConfigPath(), ""
		})
		if sessionErr == nil {
			check("Codex proxy token resolves", func() (string, string) {
				if strings.TrimSpace(session.SessionToken) == "" || strings.TrimSpace(session.TaskRoot) == "" {
					return "", "session missing token or workspace — PROMPTSTER_PROXY_TOKEN will be empty, codex will 401\n    Fix: re-run promptster start"
				}
				if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
					return "", "session expired — PROMPTSTER_PROXY_TOKEN will be empty, codex will 401\n    Fix: promptster start PST-XXXX-XXXX (or promptster reset)"
				}
				return "yes", ""
			})
			// Same reasoning as the Claude check above, and the reason this file
			// needed one at all: every other Codex check here reads local config,
			// so doctor reported all-green on sessions whose org OpenAI key had
			// been dead for months. See codex_auth_check.go.
			check("Codex model reachable through proxy", func() (string, string) {
				if blocked := proxyProbeBlocker(session); blocked != "" {
					return "skipped (" + blocked + " — see above)", ""
				}
				if err := smokeTestCodexProxy(apiURL()+"/v1/proxy/openai/v1", session.SessionToken); err != nil {
					return "", err.Error() + "\n    Fix: if the key was rejected or the budget is spent, contact the recruiter"
				}
				return "yes", ""
			})
		}
		check("Stray OpenAI/Codex auth in shell", func() (string, string) {
			var found []string
			if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
				found = append(found, "OPENAI_API_KEY")
			}
			if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
				found = append(found, "CODEX_API_KEY")
			}
			if len(found) == 0 {
				return "none", ""
			}
			// Our custom provider has its own env_key (PROMPTSTER_PROXY_TOKEN), so
			// these are ignored while model_provider=promptster — but they'd take
			// over (bypassing capture + billing) if codex ever falls back to the
			// built-in openai provider. Flag so it's a deliberate choice.
			return "", strings.Join(found, "/") + " set in this shell — ignored while the Promptster provider is selected, but would bypass capture if codex falls back to the built-in openai provider\n    Fix: unset " + strings.Join(found, " ") + " to be safe"
		})
		fmt.Println()
	}

	fmt.Println("Session")
	check("Active session", func() (string, string) {
		if sessionErr != nil {
			return "", "no active session\n    Fix: promptster start PST-XXXX-XXXX"
		}
		return fmt.Sprintf("session %s", session.SessionID), ""
	})
	if sessionErr == nil {
		check("Session token present", func() (string, string) {
			if strings.TrimSpace(session.SessionToken) == "" {
				return "", "session has no API token\n    Fix: promptster start PST-XXXX-XXXX"
			}
			return "yes", ""
		})
		// The proxy-config check below only proves the apiKeyHelper is *wired*.
		// This proves it would actually *emit* a token: cmdAuthToken returns
		// nothing (and self-evicts) when the token/workspace is missing or the
		// session has expired, in which case Claude Code silently 401s. Mirror
		// its exact conditions so an expired session can't pass doctor. Claude
		// only — Codex resolves its token separately (see the Codex section).
		if claudeRelevant {
			check("apiKeyHelper resolves a token", func() (string, string) {
				if strings.TrimSpace(session.SessionToken) == "" || strings.TrimSpace(session.TaskRoot) == "" {
					return "", "session is missing token or workspace — apiKeyHelper emits nothing, Claude Code will 401\n    Fix: re-run promptster start"
				}
				if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
					ago := time.Since(session.ExpiresAt).Round(time.Minute)
					return "", fmt.Sprintf("session expired %s ago — apiKeyHelper emits no token (self-evicts), Claude Code will 401\n    Fix: promptster start PST-XXXX-XXXX (or promptster reset)", ago)
				}
				if session.ExpiresAt.IsZero() {
					return "yes (no local TTL)", ""
				}
				return fmt.Sprintf("yes (expires in %s)", time.Until(session.ExpiresAt).Round(time.Minute)), ""
			})
			// Everything above is local: config wired, token present, not expired.
			// None of it can see a hiring-team key that the provider rejects, which
			// is the failure candidates actually hit. Spend a few tokens and ask.
			check("Claude model reachable through proxy", func() (string, string) {
				if blocked := proxyProbeBlocker(session); blocked != "" {
					return "skipped (" + blocked + " — see above)", ""
				}
				if err := smokeTestProxy(apiURL()+"/v1/proxy/anthropic", session.SessionToken); err != nil {
					return "", err.Error() + "\n    Fix: if the key was rejected or the budget is spent, contact the recruiter"
				}
				return "yes", ""
			})
		}
		check("Workspace pointer", func() (string, string) {
			pointer := filepath.Join(home, ".promptster", "active-workspace")
			data, err := os.ReadFile(pointer)
			if err != nil {
				return "", "no workspace pointer at " + pointer + "\n    Fix: re-run promptster start"
			}
			ws := strings.TrimSpace(string(data))
			if ws == "" {
				return "", pointer + " is empty\n    Fix: re-run promptster start"
			}
			if _, err := os.Stat(ws); err != nil {
				return "", "pointer references missing directory: " + ws + "\n    Fix: re-run promptster start"
			}
			return ws, ""
		})
	}
	fmt.Println()

	if claudeRelevant && sessionErr == nil && session.TaskRoot != "" {
		fmt.Println("Claude hooks")
		settingsPath := claudeProjectSettingsPath(session.TaskRoot)
		settings, settingsErr := readSettings(settingsPath)

		check("Claude settings file", func() (string, string) {
			if settingsErr != nil {
				if os.IsNotExist(settingsErr) {
					return "", "missing: " + settingsPath + "\n    Fix: run promptster start"
				}
				return "", "invalid JSON at " + settingsPath + ": " + settingsErr.Error() + "\n    Fix: re-run promptster start"
			}
			return settingsPath, ""
		})

		if settingsErr == nil {
			for _, hookPoint := range claudeHookPointNames {
				hp := hookPoint
				check("  "+hp+" hook", func() (string, string) {
					if !isHookPointConfigured(settings, hp) {
						return "", hp + " not registered in " + settingsPath + "\n    Fix: run promptster start"
					}
					return "registered", ""
				})
			}

			check("Proxy config (apiKeyHelper + base URL)", func() (string, string) {
				env, _ := settings["env"].(map[string]interface{})
				base := ""
				if env != nil {
					base, _ = env["ANTHROPIC_BASE_URL"].(string)
				}
				helper, _ := settings["apiKeyHelper"].(string)
				if base == "" {
					return "", "env.ANTHROPIC_BASE_URL not set in " + settingsPath + "\n    Fix: re-run promptster start"
				}
				if !strings.Contains(helper, "auth-token") {
					return "", "apiKeyHelper not set to 'promptster auth-token' in " + settingsPath + "\n    Fix: re-run promptster start"
				}
				// A baked-in token would out-rank the helper and re-leak the secret.
				if env != nil {
					var stale []string
					if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
						stale = append(stale, "ANTHROPIC_AUTH_TOKEN")
					}
					if _, ok := env["ANTHROPIC_API_KEY"]; ok {
						stale = append(stale, "ANTHROPIC_API_KEY")
					}
					if len(stale) > 0 {
						return "", "stale " + strings.Join(stale, "/") + " in " + settingsPath + " out-ranks apiKeyHelper\n    Fix: re-run promptster start"
					}
				}
				return base, ""
			})
		}

		shellPath := shellHookPath()
		check("Shell hook script", func() (string, string) {
			if _, err := os.Stat(shellPath); err != nil {
				return "", "missing: " + shellPath + "\n    Fix: re-run promptster start"
			}
			return shellPath, ""
		})

		check("Shell hook sourced in RC file(s)", func() (string, string) {
			rcs := shellRCPathsForInstall()
			if len(rcs) == 0 {
				return "", "could not determine shell RC path\n    Fix: manually source " + shellPath + " in your shell RC"
			}
			var sourced []string
			for _, rc := range rcs {
				if alreadyHasHook(rc) {
					sourced = append(sourced, shortenHome(rc, home))
				}
			}
			if len(sourced) == 0 {
				return "", "marker not found in " + strings.Join(rcPathsForDisplay(rcs, home), ", ") +
					"\n    Fix: re-run promptster start, or source it in the current shell:\n      source " + shellPath
			}
			return strings.Join(sourced, ", "), ""
		})
		fmt.Println()
	}

	fmt.Println("Legacy cleanup")
	check("Claude global hooks clean", func() (string, string) {
		userSettingsPath := claudeUserSettingsPath()
		data, err := os.ReadFile(userSettingsPath)
		if err != nil {
			if os.IsNotExist(err) {
				return "not present", ""
			}
			return "", "could not read " + userSettingsPath + "\n    Fix: inspect file permissions and re-run promptster start"
		}
		var s map[string]interface{}
		if err := json.Unmarshal(data, &s); err != nil {
			return "", userSettingsPath + " contains invalid JSON\n    Fix: inspect and correct the file manually"
		}
		if hasAnyPromptsterHook(s) {
			return "", "legacy hooks still in " + userSettingsPath + "\n    Fix: run promptster start to migrate"
		}
		return "clean", ""
	})
	fmt.Println()

	// Editor attention capture — installed, activated, and capturing.
	// Deliberately before Connectivity: all three of these are local facts, and
	// a candidate reading this list wants the local ones together.
	workspaceForEditor := ""
	if sessionErr == nil {
		workspaceForEditor = session.TaskRoot
	}
	doctorEditorExtension(workspaceForEditor)

	fmt.Println("Connectivity")
	check("API reachable", func() (string, string) {
		if err := apiHealth(); err != nil {
			return "", fmt.Sprintf("could not reach %s/v1/health: %v\n    Fix: check network; if behind a proxy, set HTTPS_PROXY", apiURL(), err)
		}
		return apiURL(), ""
	})
	if sessionErr == nil && session.SessionID != "" && session.SessionToken != "" {
		check("Session fetchable", func() (string, string) {
			if _, err := apiGetSession(session.SessionID, session.SessionToken); err != nil {
				return "", fmt.Sprintf("GET /v1/sessions/%s failed: %v\n    Fix: your session may have expired — re-run promptster start", session.SessionID, err)
			}
			return "yes", ""
		})
	}
	fmt.Println()
}

// readSettings loads and parses a Claude settings file.
func readSettings(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s map[string]interface{}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s, nil
}

// isHookPointConfigured returns true when the given hook point has any entry
// that references "promptster hook".
func isHookPointConfigured(settings map[string]interface{}, hookPoint string) bool {
	hooksSection, _ := settings["hooks"].(map[string]interface{})
	if hooksSection == nil {
		return false
	}
	arr, _ := hooksSection[hookPoint].([]interface{})
	for _, entry := range arr {
		b, _ := json.Marshal(entry)
		if strings.Contains(string(b), "promptster hook") {
			return true
		}
	}
	return false
}

// shortenHome replaces the home prefix with ~ for display.
func shortenHome(p, home string) string {
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func rcPathsForDisplay(paths []string, home string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, shortenHome(p, home))
	}
	return out
}

// check prints a single doctor item. fn returns (info, problem); if problem
// is non-empty the item is marked as failed and the problem text is printed.
// proxyProbeBlocker reports why a live proxy call would be meaningless right
// now, or "" when it is worth making.
//
// The checks that precede each probe already diagnose a missing token and an
// expired session precisely, and they name the right fix: re-run `promptster
// start`. Probing anyway produces one of two wrong answers. If the backend
// rejects the stale token the probe prints "contact the recruiter", which sends
// the candidate chasing a key problem they do not have. If it does NOT reject it
// — and neither proxy checks expires_at, only candidate_keys.status, so an
// expired-but-still-active key sails through — the probe prints a green "model
// reachable" directly beneath "session expired", telling the candidate the
// broken thing is fine. Skip instead, and let the check above own the diagnosis.
func proxyProbeBlocker(session Session) string {
	if strings.TrimSpace(session.SessionToken) == "" {
		return "no session token"
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		return "session expired"
	}
	return ""
}

func check(label string, fn func() (info string, problem string)) {
	info, problem := fn()
	if problem == "" {
		if info != "" {
			fmt.Printf("  \033[32m✓\033[0m  %-36s %s\n", label, info)
		} else {
			fmt.Printf("  \033[32m✓\033[0m  %s\n", label)
		}
	} else {
		fmt.Printf("  \033[31m✗\033[0m  %s\n", label)
		for _, line := range strings.Split(problem, "\n") {
			fmt.Printf("       %s\n", line)
		}
		fmt.Println()
	}
}

// hasRetiredTool reports whether either tool list names a tool Promptster used to
// instrument and no longer does. Both lists are checked because they answer
// different questions — Tools is what this run selected, AllowedTools is what the
// recruiter configured — and a session written by an older CLI can carry a
// retired name in either one.
func hasRetiredTool(selected, allowed []string) bool {
	for _, list := range [][]string{selected, allowed} {
		for _, t := range list {
			if isRetiredToolToken(t) {
				return true
			}
		}
	}
	return false
}
