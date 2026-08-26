package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

const gitWatchInterval = 60 * time.Second

// codexTurnAttributionWindow: a working-tree change observed while a
// workspace-matched codex rollout was written within this window is treated
// as agent work. Codex edits files through shell commands and successive
// mid-turn patches that the rollout doesn't itemize per-edit, so the poll
// would otherwise stamp them likely_human (observed live: 26 of 58 diffs in
// a codex-only session mislabeled human). Slack past gitWatchInterval covers
// everything a single poll tick could have observed since the last rollout
// write. Cost: a genuine human edit within ~90s of a codex turn ending is
// credited to the agent — strictly better than crediting agent work to the
// human, which inflates the candidate's contribution.
const codexTurnAttributionWindow = gitWatchInterval + 30*time.Second

// gitWatcherState tracks the background git-diff polling process.
type gitWatcherState struct {
	PID int `json:"pid"`
	// SessionID and Workspace record WHICH session this watcher was booted for.
	// Same reason as the two capture watchers (cmd_codex_watch.go): the session
	// is resolved once at boot while the state file is resolved per write
	// through the active-workspace pointer, so a watcher from the previous
	// session keeps the new session's liveness check satisfied while diffing the
	// old workspace and posting under the old session id.
	SessionID     string `json:"sessionId,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	StartedAt     string `json:"startedAt"`
	LogPath       string `json:"logPath,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	DiffsSent     int    `json:"diffsSent,omitempty"`
}

func gitWatcherStatePath() string {
	return filepath.Join(stateDir(), "git-watcher.json")
}

func gitWatcherLogPath() string {
	return filepath.Join(stateDir(), "git-watcher.log")
}

// lastDiffRefPath stores the git commit/tree hash from the last successful poll,
// so we only send diffs for genuinely new changes.
func lastDiffRefPath() string {
	return filepath.Join(stateDir(), "git-watcher-ref")
}

func loadGitWatcherState() (gitWatcherState, error) {
	data, err := os.ReadFile(gitWatcherStatePath())
	if err != nil {
		return gitWatcherState{}, err
	}
	var s gitWatcherState
	if err := json.Unmarshal(data, &s); err != nil {
		return gitWatcherState{}, err
	}
	return s, nil
}

func saveGitWatcherState(s gitWatcherState) error {
	path := gitWatcherStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func clearGitWatcherState() {
	_ = os.Remove(gitWatcherStatePath())
	_ = os.Remove(lastDiffRefPath())
}

// releaseGitWatcherState clears our state on exit, but ONLY while the state on
// disk is still ours — a watcher standing down for a new session must not delete
// the incoming watcher's state or its last-diff reference.
func releaseGitWatcherState(pid int) {
	if s, err := loadGitWatcherState(); err == nil && s.PID != pid {
		return
	}
	clearGitWatcherState()
}

// gitWatcherOwns reports whether a live watcher was booted for this session.
// Unstamped (CLI ≤1.9) counts as foreign.
func gitWatcherOwns(state gitWatcherState, session Session) bool {
	return state.SessionID != "" && state.SessionID == session.SessionID
}

func isGitWatcherRunning() (gitWatcherState, bool) {
	state, err := loadGitWatcherState()
	if err != nil || state.PID <= 0 {
		return gitWatcherState{}, false
	}
	if processExists(state.PID) {
		return state, true
	}
	clearGitWatcherState()
	return gitWatcherState{}, false
}

// ensureGitWatcher launches the git watcher for this session, replacing one left
// running by a previous session. Sweeps orphaned daemons before spawning so
// duplicates don't accumulate when a prior session's state file was lost.
func ensureGitWatcher(session Session) {
	if state, ok := isGitWatcherRunning(); ok {
		if gitWatcherOwns(state, session) {
			return
		}
		verbosef("git-watcher pid %d belongs to session %q, not %q — replacing it",
			state.PID, state.SessionID, session.SessionID)
		stopGitWatcher()
	}
	killStalePromptsterDaemons("promptster diff-watch")
	if err := startGitWatcherProcess(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not start git watcher: %v\n", err)
	}
}

// stopGitWatcher sends SIGINT to the tracked git watcher (if any), then
// sweeps any orphaned `promptster diff-watch` daemons whose state file was
// lost. SIGKILLs stragglers after a 2s grace period.
func stopGitWatcher() {
	state, err := loadGitWatcherState()
	if err == nil && state.PID > 0 {
		signalAndWaitForExit(state.PID)
	}
	killStalePromptsterDaemons("promptster diff-watch")
	clearGitWatcherState()
}

func startGitWatcherProcess() error {
	logPath := gitWatcherLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(promptsterBin(), "diff-watch")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd.Start()
}

// runGitWatcher is the main loop for the `promptster diff-watch` subcommand.
func runGitWatcher() error {
	session, err := loadSession()
	if err != nil {
		return fmt.Errorf("no active session: %w", err)
	}
	if session.TaskRoot == "" {
		return fmt.Errorf("session has no task root")
	}

	// Acquire lock
	if state, ok := isGitWatcherRunning(); ok && state.PID != os.Getpid() {
		return fmt.Errorf("git watcher already running (pid %d)", state.PID)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveGitWatcherState(gitWatcherState{
		PID:           os.Getpid(),
		SessionID:     session.SessionID,
		Workspace:     session.TaskRoot,
		StartedAt:     now,
		LogPath:       gitWatcherLogPath(),
		LastHeartbeat: now,
	}); err != nil {
		return err
	}
	defer releaseGitWatcherState(os.Getpid())

	// Restore API URL from session (background process may not have env vars)
	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	client := &http.Client{Timeout: 5 * time.Second}
	diffsSent := 0

	// Take initial snapshot so we don't send the entire repo as a diff on first poll
	saveCurrentTreeHash(session.TaskRoot)

	fmt.Fprintf(os.Stderr, "git-watcher: started, polling every %s in %s\n", gitWatchInterval, session.TaskRoot)

	for {
		// Stand down when a different session becomes the active one: TaskRoot
		// was resolved once, so continuing would diff the previous assessment's
		// tree and post it under the previous session's id.
		if cur, curErr := loadSession(); curErr != nil || cur.SessionID != session.SessionID {
			fmt.Fprintf(os.Stderr, "git-watcher: session %s is no longer active — exiting (sent %d diffs)\n",
				session.SessionID, diffsSent)
			return nil
		}

		sent, err := pollGitDiffs(session, client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "git-watcher: poll error: %v\n", err)
		}
		diffsSent += sent

		// Update heartbeat
		_ = saveGitWatcherState(gitWatcherState{
			PID:           os.Getpid(),
			SessionID:     session.SessionID,
			Workspace:     session.TaskRoot,
			StartedAt:     now,
			LogPath:       gitWatcherLogPath(),
			LastHeartbeat: time.Now().UTC().Format(time.RFC3339Nano),
			DiffsSent:     diffsSent,
		})

		select {
		case <-signals:
			fmt.Fprintf(os.Stderr, "git-watcher: shutting down (sent %d diffs)\n", diffsSent)
			return nil
		case <-time.After(gitWatchInterval):
		}
	}
}

// pollGitDiffs runs `git diff` against the last known state and sends any
// per-file diffs as file_diff events. Returns the number of events sent.
func pollGitDiffs(session Session, client *http.Client) (int, error) {
	taskRoot := session.TaskRoot
	lastHash := loadLastTreeHash()

	// Build git diff command against our stored baseline. Pathspec excludes
	// keep CLI-generated files (TASK.md, .promptster/) out of file_diff events.
	var args []string
	if lastHash == "" {
		args = []string{"-C", taskRoot, "diff", "HEAD", "--no-color", "--unified=3"}
	} else {
		args = []string{"-C", taskRoot, "diff", lastHash, "--no-color", "--unified=3"}
	}
	args = append(args, gitExcludePathspecs(taskRoot)...)

	diffOutput, err := exec.Command("git", args...).Output()
	if err != nil {
		// git diff returns exit code 1 when there are diffs — that's fine if we got output
		if len(diffOutput) == 0 {
			return 0, nil // No changes
		}
	}

	if len(diffOutput) == 0 {
		return 0, nil
	}

	// Scrub secrets from the working-tree diff before it becomes file_diff
	// events. (Dedup hashes the on-disk file content, not this text, so
	// redaction here does not affect cross-channel dedup matching.)
	diffOutput = redactBytes(diffOutput)

	// Parse the unified diff into per-file chunks
	fileDiffs := splitUnifiedDiff(string(diffOutput))
	if len(fileDiffs) == 0 {
		return 0, nil
	}

	sent := 0
	for _, fd := range fileDiffs {
		event := newEvent("file_diff", session.SessionID)
		event.Source = "cli"
		// What survives dedup below is human work UNLESS a codex turn was
		// actively writing during this poll window — then the change is the
		// agent's (shell writes / mid-turn states the rollout doesn't itemize).
		// Otherwise: a fresh manual edit, or a human revision of a file an AI
		// channel touched earlier (ai-paths ledger).
		aiTouched := wasAiTouchedPath(session.SessionID, fd.path)
		if codexRolloutActiveSince(time.Now().Add(-codexTurnAttributionWindow)) {
			event.Actor = aiActor()
			event.Provenance = codexTurnProvenance()
		} else {
			event.Actor = humanActor()
			event.Provenance = gitPollProvenance(aiTouched)
		}
		event.Data = map[string]interface{}{
			"path":         fd.path,
			"diff":         fd.diff,
			"linesAdded":   fd.linesAdded,
			"linesRemoved": fd.linesRemoved,
			"attribution":  event.Provenance.Attribution,
			"_gitPoll":     true,
		}

		// Idempotency: if an AI channel (Claude hook / codex rollout) already
		// emitted a file_diff for this exact resulting content, skip it — this
		// poll's view is the same edit. What remains are genuine manual edits.
		if !dedupeFileDiff(taskRoot, &event) {
			continue
		}

		if err := appendEventToLocalBuffer(&event); err != nil {
			fmt.Fprintf(os.Stderr, "git-watcher: buffer error for %s: %v\n", fd.path, err)
		}
		if err := ingestEventWithClient(client, event, session.SessionToken); err != nil {
			fmt.Fprintf(os.Stderr, "git-watcher: send error for %s: %v\n", fd.path, err)
			continue
		}
		sent++
	}

	// Update baseline to current state after successful send
	saveCurrentTreeHash(taskRoot)

	if sent > 0 {
		fmt.Fprintf(os.Stderr, "git-watcher: sent %d file_diff event(s)\n", sent)
	}
	return sent, nil
}

// fileDiffChunk represents a parsed per-file diff from unified diff output.
type fileDiffChunk struct {
	path         string
	diff         string
	linesAdded   int
	linesRemoved int
}

// splitUnifiedDiff splits a multi-file unified diff into per-file chunks.
func splitUnifiedDiff(raw string) []fileDiffChunk {
	var results []fileDiffChunk
	lines := strings.Split(raw, "\n")

	var current *fileDiffChunk
	var currentLines []string

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			// Flush previous
			if current != nil {
				current.diff = strings.Join(currentLines, "\n")
				results = append(results, *current)
			}
			current = &fileDiffChunk{}
			currentLines = []string{line}

			// Extract path from "diff --git a/path b/path"
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				current.path = parts[1]
			}
			continue
		}

		if current == nil {
			continue
		}

		currentLines = append(currentLines, line)

		// Count added/removed lines (only hunk content, not headers)
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			current.linesAdded++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			current.linesRemoved++
		}
	}

	// Flush last file
	if current != nil {
		current.diff = strings.Join(currentLines, "\n")
		results = append(results, *current)
	}

	return results
}

// saveCurrentTreeHash records the current working tree state so the next poll
// only picks up new changes. Uses `git stash create` to get a commit object
// representing the working tree without actually stashing anything.
func saveCurrentTreeHash(taskRoot string) {
	// git stash create returns a commit hash representing current working tree
	// state (staged + unstaged). Returns empty if working tree is clean.
	out, err := exec.Command("git", "-C", taskRoot, "stash", "create").Output()
	hash := strings.TrimSpace(string(out))
	if err != nil || hash == "" {
		// No uncommitted changes — use HEAD as baseline
		out, err = exec.Command("git", "-C", taskRoot, "rev-parse", "HEAD").Output()
		if err != nil {
			return
		}
		hash = strings.TrimSpace(string(out))
	}
	_ = os.WriteFile(lastDiffRefPath(), []byte(hash), 0o644)
}

func loadLastTreeHash() string {
	data, err := os.ReadFile(lastDiffRefPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
