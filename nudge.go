package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// /explain is fully optional commentary — the CLI never prompts for it.
// (The decision-shaped mid-session nudge was removed 2026-06: candidates were
// getting bugged to write entries that aren't required. The state below is
// kept only to power `promptster status` displays.)

type nudgeState struct {
	LastExplainAt     time.Time `json:"lastExplainAt"`
	LastNudgeAt       time.Time `json:"lastNudgeAt"`
	FilesSinceExplain int       `json:"filesSinceExplain"`
	NudgeCount        int       `json:"nudgeCount"`
}

func nudgeStatePath() string {
	return filepath.Join(stateDir(), "nudge-state.json")
}

func loadNudgeState() nudgeState {
	data, err := os.ReadFile(nudgeStatePath())
	if err != nil {
		return nudgeState{}
	}
	var s nudgeState
	json.Unmarshal(data, &s) //nolint:errcheck
	return s
}

func saveNudgeState(s nudgeState) {
	path := nudgeStatePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(s)
	_ = os.WriteFile(path, data, 0o644)
}

// recordExplain resets the decision counter after a successful explain.
func recordExplain() {
	s := loadNudgeState()
	s.LastExplainAt = time.Now()
	s.FilesSinceExplain = 0
	saveNudgeState(s)
}

// recordFileChange bumps the count of files changed since the last explain.
// Called from the hook handler on file_diff events.
func recordFileChange() {
	s := loadNudgeState()
	if s.LastExplainAt.IsZero() {
		return // no active session
	}
	s.FilesSinceExplain++
	saveNudgeState(s)
}

// initNudgeState sets the initial nudge state (called during `promptster start`).
func initNudgeState() {
	saveNudgeState(nudgeState{LastExplainAt: time.Now()})
}

// clearNudgeState removes the nudge state file.
func clearNudgeState() {
	_ = os.Remove(nudgeStatePath())
}

