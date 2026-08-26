package main

import (
	"fmt"
	"os"
	"os/exec"
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
func checkTimeLimit() {
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

		// Time is up — auto-submit
		fmt.Fprintf(os.Stderr, "\n[promptster] ⏰ Time limit reached. Auto-submitting your assessment...\n")

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
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
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
