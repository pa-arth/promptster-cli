package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sessionDeadline returns the moment this session's time limit expires, and
// whether the session has one at all.
func sessionDeadline(session Session) (time.Time, bool) {
	if session.TimeLimitMinutes <= 0 || session.StartedAt.IsZero() {
		return time.Time{}, false
	}
	return session.StartedAt.Add(time.Duration(session.TimeLimitMinutes) * time.Minute), true
}

// checkTimeLimit checks the session time limit and warns/auto-submits as needed.
//
// CALLED FROM THREE PLACES, AND THAT IS THE POINT. It used to be called only
// from the two hook handlers, which meant the client learned its own deadline
// had passed only when the agent next emitted a hook — minutes later for a
// candidate reading code, and never for one who had stopped typing. Meanwhile
// the server's timer fires on schedule, so the server won the race in the
// ORDINARY case, not the unlucky one, and the candidate's last work was locked
// out before it could be delivered.
//
// The `promptster diff-watch` daemon runs for every session and re-reads this
// same session file every poll, so it now calls this too and sleeps to the
// deadline rather than past it. The hook call sites remain as a backstop for a
// session whose watcher died; the marker below is what keeps the two paths from
// acting twice.
func checkTimeLimit() { checkTimeLimitFrom(surfaceVisible) }

// checkTimeLimitFromWatcher is the `promptster diff-watch` entry point. The
// daemon's stdout and stderr are both git-watcher.log, so anything it prints is
// invisible to the candidate — it leaves a notice for the next hook instead.
func checkTimeLimitFromWatcher() { checkTimeLimitFrom(surfaceLogFile) }

// Whether the caller owns a stream the candidate actually reads.
type expirySurface int

const (
	surfaceVisible expirySurface = iota // a hook: its stderr reaches the IDE
	surfaceLogFile                      // the watcher: its stderr reaches a log
)

func checkTimeLimitFrom(surface expirySurface) {
	// Before anything else, and deliberately before the session load below: the
	// watcher may have auto-submitted and then had `done` delete the session out
	// from under us, so a drain that ran after the load would never happen.
	//
	// ONLY A VISIBLE CALLER MAY DRAIN. The watcher writes the notice and then
	// keeps polling; if it drained too, its very next iteration would consume its
	// own message into git-watcher.log and the hook would find nothing left. A
	// drain is a delivery, and only a caller whose stderr the candidate reads can
	// deliver.
	if surface == surfaceVisible {
		drainExpiryNotice()
	}

	session, err := loadSession()
	if err != nil {
		return
	}
	deadline, ok := sessionDeadline(session)
	if !ok {
		return
	}
	remaining := time.Until(deadline)

	if remaining <= 0 {
		// Fire ONCE. Without this marker every hook after the deadline spawned
		// another `promptster done`, and since the spawn does not wait they ran
		// concurrently — printing "assessment NOT marked complete" into the
		// candidate's IDE over and over while their work never left the machine.
		//
		// The marker is written BEFORE the spawn, not after it returns: this
		// function returns immediately by design, so writing it afterwards would
		// leave the same window open.
		if alreadyAutoSubmitted() {
			return
		}
		markAutoSubmitted()

		// Time is up — auto-submit.
		banner := "\n[promptster] ⏰ Time limit reached. Auto-submitting your assessment...\n"
		fmt.Fprint(os.Stderr, banner)

		// WHERE THE CANDIDATE ACTUALLY READS THIS. The hook call sites write to a
		// stderr the IDE surfaces, so on those paths the line above is seen. The
		// watcher is a DAEMON whose stdout and stderr are both `git-watcher.log`
		// (git_watcher.go, spawnGitWatcher) — so on what is now the ORDINARY
		// detection path, everything printed here and everything `done` prints
		// afterwards lands in a log file nobody opens. Detection was fixed by
		// moving the check into the watcher; the announcement must not be lost in
		// the same move.
		//
		// Two channels, because neither alone covers every lane:
		//   1. /dev/tty, when the daemon still shares the candidate's terminal.
		//   2. a durable one-shot notice drained by the next hook (drainExpiryNotice,
		//      called at the top of this function and at the top of `cmdHook`).
		// The notice lives in the GLOBAL dir on purpose: `done` runs
		// cleanupPromptsterState, which wipes the workspace state dir, and the
		// notice has to outlive that to be read afterwards.
		//
		// Only the invisible caller leaves one. A hook has already printed the
		// banner above to a stream the candidate reads, and `done`'s own submitted
		// box follows it there — a notice on that path would just say it twice.
		//
		// The watcher leaves one even when /dev/tty opens. A tty existing is not
		// the same as the candidate watching it — the terminal that launched the
		// session is often not the pane they are working in — and the two failure
		// modes are not comparable: a line they read twice versus never being told
		// their assessment was submitted at all.
		out := io.Writer(os.Stderr)
		if surface == surfaceLogFile {
			writeExpiryNotice(session)
			if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
				defer tty.Close()
				fmt.Fprint(tty, banner)
				out = tty
			}
		}

		// Run promptster done in the background.
		//
		// `--auto` is not cosmetic. It suppresses the interactive decision-notes
		// notice (this path is the definition of non-interactive), and it tells
		// submitWorkspaceCode to submit rather than abort when it finds work
		// stranded outside the bundle — refusing there would strand the session
		// open forever. The failure message this handler used to print already
		// advised the candidate to "retry `promptster done --auto`" while the
		// handler itself did not pass it.
		bin := promptsterBin()
		cmd := exec.Command(bin, "done", "--auto")
		cmd.Stdout = out
		cmd.Stderr = out
		_ = cmd.Start()
		// Don't wait — let the hook return so the IDE doesn't hang
		return
	}

	// Time reminders at key intervals
	minutesLeft := int(remaining.Minutes())

	// Only warn at specific thresholds to avoid spam
	thresholds := []int{30, 15, 10, 5, 2, 1}
	for _, t := range thresholds {
		if minutesLeft == t {
			// Check if we already warned for this threshold
			if alreadyWarnedForThreshold(t) {
				return
			}
			markThresholdWarned(t)

			urgency := "info"
			if t <= 5 {
				urgency = "warning"
			}
			if t <= 2 {
				urgency = "urgent"
			}

			msg := fmt.Sprintf("%d minute%s remaining", t, pluralS(t))
			if urgency == "urgent" {
				msg += " — wrap up and run `promptster done`"
			}

			fmt.Fprintf(os.Stderr, "\n[promptster] ⏰ %s\n", msg)
			return
		}
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// alreadyWarnedForThreshold checks if we've already warned at this minute threshold.
func alreadyWarnedForThreshold(minutes int) bool {
	path := fmt.Sprintf("%s/time-warned-%d", stateDir(), minutes)
	_, err := os.Stat(path)
	return err == nil
}

// markThresholdWarned creates a marker file so we don't repeat warnings.
func markThresholdWarned(minutes int) {
	path := fmt.Sprintf("%s/time-warned-%d", stateDir(), minutes)
	_ = os.MkdirAll(stateDir(), 0o700)
	_ = os.WriteFile(path, []byte("1"), 0o644)
}

// autoSubmitMarkerPath is the "we have already handled expiry" marker. Same
// shape as the threshold warnings above, which have always had one; the expiry
// branch was the one path that recorded nothing about having run.
func autoSubmitMarkerPath() string {
	return fmt.Sprintf("%s/time-auto-submitted", stateDir())
}

func alreadyAutoSubmitted() bool {
	_, err := os.Stat(autoSubmitMarkerPath())
	return err == nil
}

func markAutoSubmitted() {
	_ = os.MkdirAll(stateDir(), 0o700)
	_ = os.WriteFile(autoSubmitMarkerPath(), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// expiryNoticePath is the one-shot message left for the candidate when expiry
// was handled by a process they cannot see.
//
// GLOBAL DIR, NOT THE SESSION STATE DIR, and that is the whole point. The
// auto-submit spawns `promptster done`, which runs cleanupPromptsterState and
// deletes the workspace's `.promptster/` along with the session file. A notice
// written there would be destroyed by the very command it is announcing, before
// any hook could read it. `~/.promptster/` outlives the session; the drain
// removes the file itself, so nothing accumulates.
func expiryNoticePath() string {
	return filepath.Join(globalPromptsterDir(), "expiry-notice")
}

// writeExpiryNotice records what the candidate would have been told, had the
// process that detected expiry owned a surface they read.
func writeExpiryNotice(session Session) {
	appURL := os.Getenv("PROMPTSTER_APP_URL")
	if appURL == "" {
		appURL = "https://promptster.ai"
	}
	msg := "\n[promptster] ⏰ Time limit reached — your assessment was submitted automatically.\n"
	if session.SessionID != "" && session.Key != "" {
		msg += fmt.Sprintf("[promptster] Your results: %s/replay/%s?key=%s\n",
			appURL, session.SessionID, session.Key)
	}
	_ = os.MkdirAll(globalPromptsterDir(), 0o700)
	_ = os.WriteFile(expiryNoticePath(), []byte(msg), 0o600)
}

// drainExpiryNotice prints a pending notice exactly once and removes it.
//
// Called from `checkTimeLimit` (before its session load, which fails once
// `done` has cleaned up) and from the top of `cmdHook` (before ITS session
// load, for the same reason). Removing the file before printing would lose the
// message if the write failed; removing it after is the correct order because a
// second reader finding it already gone is exactly what "once" means.
func drainExpiryNotice() {
	data, err := os.ReadFile(expiryNoticePath())
	if err != nil || len(data) == 0 {
		return
	}
	fmt.Fprint(os.Stderr, string(data))
	_ = os.Remove(expiryNoticePath())
}

// preDeadlineSnapshotMarkerPath throttles the one forced snapshot the watcher
// takes shortly before the deadline.
func preDeadlineSnapshotMarkerPath() string {
	return fmt.Sprintf("%s/pre-deadline-snapshot", stateDir())
}

func alreadyPreDeadlineSnapshotted() bool {
	_, err := os.Stat(preDeadlineSnapshotMarkerPath())
	return err == nil
}

func markPreDeadlineSnapshotted() {
	_ = os.MkdirAll(stateDir(), 0o700)
	_ = os.WriteFile(preDeadlineSnapshotMarkerPath(), []byte("1"), 0o644)
}

// preDeadlineSnapshotLead is how long before the deadline the watcher forces a
// workspace snapshot.
//
// WHY BOTHER, GIVEN THE SERVER'S DELIVERY WINDOW. Because the best outcome is
// the one where the window is never needed: bytes accepted BEFORE the flip are
// graded by the ordinary pipeline with no late-arrival disclosure and no
// re-grade. This makes that the common case and leaves the window as the
// fallback it should be. It does not replace the window — a snapshot needs a git
// workspace, and work done in the final seconds is still after the last one.
const preDeadlineSnapshotLead = 90 * time.Second

// maybeSnapshotBeforeDeadline forces one workspace snapshot in the last
// preDeadlineSnapshotLead before the time limit expires. No-op with no deadline,
// too early, already past it, or already taken.
func maybeSnapshotBeforeDeadline(session Session) {
	deadline, ok := sessionDeadline(session)
	if !ok {
		return
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > preDeadlineSnapshotLead {
		return
	}
	if alreadyPreDeadlineSnapshotted() {
		return
	}
	markPreDeadlineSnapshotted()
	forceSnapshotWorkspace(session)
}
