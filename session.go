package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// newHoneypotToken returns a random hex string used as a per-session trap
// marker. Returns an empty string if the system's secure RNG is unavailable.
func newHoneypotToken() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return "PST-HP-" + hex.EncodeToString(buf)
}

// Session holds the local state persisted in <workspace>/.promptster/session.json.
type Session struct {
	SessionID       string `json:"sessionId"`
	SessionToken    string `json:"sessionToken"`
	Key             string `json:"key"`
	AssessmentID    string `json:"assessmentId"`
	AssessmentTitle string `json:"assessmentTitle,omitempty"`
	OrgName         string `json:"orgName,omitempty"`
	TaskBrief       string `json:"taskBrief"`
	// Brief is the structured assessment brief (scenario, codebase
	// orientation, phases, evaluation dimensions, ground rules,
	// deliverables). Nil for older assessments — TaskBrief carries the
	// legacy flat string in that case.
	Brief             *Brief `json:"brief,omitempty"`
	RepoURL           string `json:"repoUrl,omitempty"`
	RepoCommit        string `json:"repoCommit,omitempty"`
	RepoSubdir        string `json:"repoSubdir,omitempty"`
	SetupInstructions string `json:"setupInstructions,omitempty"`
	WorkspaceCommit   string `json:"workspaceCommit,omitempty"`
	TaskRoot          string `json:"taskRoot,omitempty"`
	TimeLimitMinutes  int    `json:"timeLimitMinutes"`
	// ApiURL is the resolved API URL at the time of `start`. Hooks read this
	// as a fallback when the PROMPTSTER_API_URL env var is not set (e.g. Cursor
	// launched from Dock won't inherit shell env vars).
	ApiURL string `json:"apiUrl,omitempty"`
	// ConsentAccepted is true when the candidate accepted the ToS during `redeem`.
	// When true, `start` skips the inline consent prompt.
	ConsentAccepted bool `json:"consentAccepted,omitempty"`
	// ConsentToIntegrity is true when the candidate opted in to cadence-based
	// authorship checks (prompt timing + honeypot detection). Off by default.
	ConsentToIntegrity bool `json:"consentToIntegrity,omitempty"`
	// HoneypotToken is a per-session random marker written into a trap file
	// inside the workspace. The backend looks for this token in prompts and
	// command output to flag likely cross-session copy/paste.
	HoneypotToken string    `json:"honeypotToken,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	// ExpiresAt is the candidate-key expiry (mirrored from the backend on start)
	// so the shell hook can self-evict locally when the key ages out.
	ExpiresAt time.Time `json:"expiresAt"`
	// Git author metadata — captured during start for commit attribution
	GitAuthorName  string `json:"gitAuthorName,omitempty"`
	GitAuthorEmail string `json:"gitAuthorEmail,omitempty"`
	// OSS issue ID — used for CI test lookup
	IssueID string `json:"issueId,omitempty"`
	// Tools lists the AI coding tools instrumented for this session
	// ("claude", "codex", and/or "cursor"), chosen at start. Drives which
	// hooks/watchers are configured and torn down. Empty = legacy session
	// (treat as claude).
	Tools []string `json:"tools,omitempty"`
	// AllowedTools is the recruiter-chosen subset of {claude,codex,cursor} the
	// candidate may instrument, mirrored from the redeem response so `start` can
	// constrain tool selection. Empty/nil = unconstrained (older server) →
	// fall back to all tools.
	AllowedTools []string `json:"allowedTools,omitempty"`
	// AuthMode records how model credentials are supplied:
	//   ""/"managed"       — Promptster proxy bills the org key (default)
	//   "byo-subscription" — candidate's own Claude/OpenAI subscription; no
	//                        proxy wiring, cost is ESTIMATED from transcripts
	AuthMode string `json:"authMode,omitempty"`
	// CaptureMode selects the Claude Code capture channel:
	//   ""/"hooks"   — hook-driven capture (default)
	//   "transcript" — claude-watch tails the transcript JSONL; hooks fall
	//                  back only when the watcher is unhealthy
	CaptureMode string `json:"captureMode,omitempty"`
}

func sessionPath() string {
	return filepath.Join(stateDir(), "session.json")
}

// parseExpiresAt converts a RedeemResponse.ExpiresAt ISO-8601 string to time.Time.
// An empty input or parse failure returns the zero value, which downstream code
// treats as "no local TTL — fall back to server-side expiry".
func parseExpiresAt(iso string) time.Time {
	if iso == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return time.Time{}
	}
	return t
}

func saveSession(s Session) error {
	path := sessionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadSession() (Session, error) {
	data, err := os.ReadFile(sessionPath())
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, fmt.Errorf("no active session — run: promptster start PST-XXXX-XXXX")
		}
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, fmt.Errorf("corrupt session file: %w", err)
	}
	return s, nil
}

func deleteSession() error {
	return os.Remove(sessionPath())
}

// cleanupPromptsterState removes all ephemeral files created during an
// assessment session. The binary (~/.promptster/bin/) is intentionally
// kept so future assessments don't need to re-install.
func cleanupPromptsterState(taskRoot string) {
	dir := stateDir()

	// Session-scoped state files
	ephemeralFiles := []string{
		"session.json",
		"session.key",
		"buffer.jsonl",
		"last_prompt.ts",
		"hook-debug.log",
		"debug-hooks",
		"nudge-state.json",
		"decision-queue.jsonl",
		"decision-prompt.state",
		"decision-watcher.json",
		"decision-watcher-surface.json",
		"decision-watcher.log",
		"time-warned-30",
		"time-warned-15",
		"time-warned-10",
		"time-warned-5",
		"time-warned-2",
		"time-warned-1",
		"git-watcher.json",
		"git-watcher.log",
		"git-watcher-ref",
		"codex-watcher.json",
		"codex-watcher.log",
		"codex-watcher-progress.json",
		"codex-proxy-state.json",
		"diff-hashes.json",
		"diff-hashes.json.lock",
	}
	for _, f := range ephemeralFiles {
		_ = os.Remove(filepath.Join(dir, f))
	}

	// Try to remove the .promptster dir itself if empty
	_ = os.Remove(dir)

	// Clear the global pointer
	clearActiveWorkspace()

	// Workspace hook configs (project-scoped, written by configureHooks)
	if taskRoot != "" {
		_ = os.Remove(filepath.Join(taskRoot, ".claude", "settings.local.json"))
		removeExplainCommand(taskRoot)
		// Cursor hooks: restore the candidate's pre-existing .cursor/hooks.json
		// (if we backed one up) or remove the file we created.
		removeCursorHooks(taskRoot)
	}
}
