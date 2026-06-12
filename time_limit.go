package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// checkTimeLimit checks the session time limit and warns/auto-submits as needed.
// Called from hook handlers alongside maybeNudgeExplain.
func checkTimeLimit() {
	session, err := loadSession()
	if err != nil || session.TimeLimitMinutes <= 0 || session.StartedAt.IsZero() {
		return
	}

	deadline := session.StartedAt.Add(time.Duration(session.TimeLimitMinutes) * time.Minute)
	remaining := time.Until(deadline)

	if remaining <= 0 {
		// Time is up — auto-submit
		fmt.Fprintf(os.Stderr, "\n[promptster] ⏰ Time limit reached. Auto-submitting your assessment...\n")

		// Run promptster done in the background
		bin := promptsterBin()
		cmd := exec.Command(bin, "done")
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
