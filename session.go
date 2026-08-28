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
	// ExpectedTreeSha is the mirror's `git rev-parse HEAD^{tree}`, recorded at
	// mirror-build time and mirrored from the redeem response. `start --adopt`
	// compares the adopted checkout against it and HARD-FAILS on a mismatch
	// (design.md §5). Empty when the server does not send it — that is an
	// unverified adopt, reported loudly, never a silent pass.
	ExpectedTreeSha string `json:"expectedTreeSha,omitempty"`
	// TreeVerification is the adopt verdict for the record: "verified",
	// "unverified", or absent on the local lane. The two fatal states never
	// reach a saved session — `start` exits on them.
	TreeVerification string `json:"treeVerification,omitempty"`
	// DiffBaseCommit is the commit every diff and bundle is measured against.
	//
	// It exists because RepoCommit CANNOT play that role on an adopted checkout.
	// RepoCommit is the UPSTREAM brokenSha; the mirror is a single orphan commit
	// whose TREE equals upstream's at that sha but whose commit object upstream's
	// sha names does not exist in the mirror at all. `git diff <brokenSha>` there
	// fails outright, and the failure path is a warning plus an EMPTY diff — a
	// submission that silently contains none of the candidate's work.
	//
	// Empty on the local lane, where RepoCommit is fetched and therefore real.
	DiffBaseCommit   string `json:"diffBaseCommit,omitempty"`
	TimeLimitMinutes int    `json:"timeLimitMinutes"`
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
	// ("claude" and/or "codex"), chosen at start. Drives which
	// hooks/watchers are configured and torn down. Empty = legacy session
	// (treat as claude).
	Tools []string `json:"tools,omitempty"`
	// AllowedTools is the recruiter-chosen subset of {claude,codex} the
	// candidate may instrument, mirrored from the redeem response so `start` can
	// constrain tool selection. Empty/nil = unconstrained (older server) →
	// fall back to all tools.
	AllowedTools []string `json:"allowedTools,omitempty"`
	// AuthMode records how model credentials are supplied. There is now only one
	// answer — ""/"managed", the proxy billing the hiring team's key. It is kept
	// as a RECORD of what the server said, not as a switch: nothing in the CLI
	// branches on it any more. See openspec changes/employer-supplied-model-key.
	AuthMode string `json:"authMode,omitempty"`
	// NoSelfEvict disarms TTL SELF-EVICTION for this session.
	//
	// ⛔ What it prevents, measured: on a session whose ExpiresAt has passed,
	// ONE interactive shell — a candidate clicking TERMINAL — wipes
	// `.claude/settings.local.json`, `.promptster/session.json`,
	// `active-workspace`, the codex provider block, the shell hook and its RC
	// line. The shell hook backgrounds `promptster env`, which fires
	// `cmdCleanup`; `promptster auth-token` fires it too, and the apiKeyHelper
	// **is** `auth-token` — so Claude Code triggers the teardown itself, once per
	// API request.
	//
	// That is CORRECT on a candidate's own laptop: an abandoned session must not
	// leave our hooks in their shell forever. It is wrong in a box WE provisioned
	// and own, where the same code destroys the environment rather than tidying
	// up after itself, and the candidate's next action is a support ticket.
	//
	// Two guards ship together and this is the second one. The first is an
	// `expiresAt` seeded to outlive the assessment window — a value someone can
	// get wrong. This is what stops a wrong value from being destructive.
	//
	// ⚠ The default is deliberately the NEGATIVE. Absent (a laptop session, a
	// session.json written by any older CLI) means self-eviction stays armed,
	// exactly as before. Only a seeded start turns it off, and only for the box
	// it seeded. Read it through `selfEvictArmed()`, never directly.
	//
	// openspec private-problem-sandbox-lane design.md §8.6, task 2.5e.
	NoSelfEvict bool `json:"noSelfEvict,omitempty"`
	// SeededAt records that this session arrived on disk as BYTES written by the
	// provisioning worker rather than through `promptster redeem` on this
	// machine. It is the fact that explains every other unusual thing about such
	// a session to whoever reads it next.
	//
	// ⚠ It BRANCHES now, which it did not before. 2.4i deleted `HostedLane` and
	// the `inCodespace()` probe behind it, and the two places that read the old
	// flag for a reason that was never about GitHub — "is this a machine the
	// candidate owns and has to clean up?" — read this instead, through
	// `seededSession`. Everything else the flag gated WAS about Codespaces (the
	// automatic-fork-on-commit hazard, `gh codespace delete`, the storage
	// allowance) and is gone rather than moved.
	SeededAt time.Time `json:"seededAt,omitempty"`
	// CaptureMode selects the Claude Code capture channel:
	//   ""/"hooks"   — hook-driven capture (default)
	//   "transcript" — claude-watch tails the transcript JSONL; hooks fall
	//                  back only when the watcher is unhealthy
	//
	// "transcript" is no longer a configuration. It is armed at start when the
	// proxy smoke test fails, i.e. when proxy capture would produce nothing.
	CaptureMode string `json:"captureMode,omitempty"`
}

// selfEvictArmed reports whether an expired session may tear itself down.
//
// Every caller that fires `fireBackgroundCleanup` on expiry must go through
// this, and there are three of them on hot paths: the shell hook
// (`promptster env`, once per prompt), the Claude Code apiKeyHelper
// (`promptster auth-token`, once per API request) and `promptster codex` on
// launch. A guard added to two of the three is not a guard.
func (s Session) selfEvictArmed() bool {
	return !s.NoSelfEvict
}

// expired reports whether the local TTL has passed.
//
// A zero ExpiresAt means "no local TTL" — a session.json written by a
// pre-feature CLI has nothing to compare against, and treating unknown as
// expired would evict working sessions on upgrade.
func (s Session) expired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
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
		// The expiry markers belong in this list for the same reason the threshold
		// warnings do, and more urgently: stateDir() is WORKSPACE-scoped, so a
		// second assessment taken in the same directory would find
		// `time-auto-submitted` already present, return early from the expiry
		// branch, and never auto-submit at all — reinstating the exact bug the
		// marker was added to fix. Leaving them behind also keeps `.promptster/`
		// non-empty, so the `os.Remove(dir)` below silently fails.
		"time-auto-submitted",
		"pre-deadline-snapshot",
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
		// A Cursor hooks teardown used to live here. Promptster no longer writes
		// .cursor/hooks.json at all (openspec changes/employer-supplied-model-key),
		// so there is nothing of ours left in the candidate's workspace to restore.
	}
}

// seededSession reports whether this session was written by the provisioning
// worker into a box we own — as opposed to redeemed by a candidate on a machine
// that is theirs.
//
// This is the successor to `hostedLaneActive`, narrowed on purpose. The old
// predicate answered "are we on the hosted lane", ORed an env probe over a
// session flag, and gated four unrelated behaviours. Three of those four existed
// only because the machine was a GitHub Codespace and went with it. What is left
// is the one question that still has two answers: does the candidate own this
// machine and have to clean it up afterwards? In a seeded box they do not.
//
// Reads the SESSION rather than the environment. There is no `CODESPACES`
// equivalent to probe for inside a box, and the session file is the stronger
// signal anyway — it survives a `done` or `doctor` run in a bare `sh -c` whose
// environment carries nothing.
func seededSession(s Session) bool {
	return !s.SeededAt.IsZero()
}
