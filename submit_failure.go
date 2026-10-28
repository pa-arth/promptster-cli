package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// submitFailure is the last workspace upload that did not reach the server,
// from either `promptster done` or a periodic snapshot.
//
// Both paths used to fail where only one person could see it: `done` printed to
// the candidate's terminal and exited 1, and snapshots wrote to the hook debug
// log. Session 73ee97cc failed `done` three times and stopped snapshotting after
// minute 21; the session record held three exit codes and nothing else, and
// the hosted box that held the debug log was gone before anyone asked.
type submitFailure struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // "done" | "snapshot"
	Stage  string    `json:"stage"`
	Error  string    `json:"error"`
}

func submitFailurePath() string {
	return filepath.Join(stateDir(), "last-submit-failure.json")
}

func loadSubmitFailure() (submitFailure, bool) {
	data, err := os.ReadFile(submitFailurePath())
	if err != nil {
		return submitFailure{}, false
	}
	var f submitFailure
	if json.Unmarshal(data, &f) != nil {
		return submitFailure{}, false
	}
	return f, true
}

// recordSubmitFailure persists the failure for `promptster status` and sends it
// to the session record as a `submission_failed` event. A snapshot that fails
// the same way it failed last time is not re-sent: snapshots retry every 30s and
// the record needs the failure, not a copy of it per retry.
func recordSubmitFailure(session Session, source, stage string, err error) {
	f := submitFailure{At: time.Now().UTC(), Source: source, Stage: stage, Error: err.Error()}
	prev, had := loadSubmitFailure()
	if data, mErr := json.Marshal(f); mErr == nil {
		_ = os.MkdirAll(filepath.Dir(submitFailurePath()), 0o700)
		_ = os.WriteFile(submitFailurePath(), data, 0o644)
	}
	if source == "snapshot" && had && prev.Source == source && prev.Stage == stage && prev.Error == f.Error {
		return
	}
	if session.SessionID == "" || session.SessionToken == "" {
		return
	}
	event := newEvent("submission_failed", session.SessionID)
	event.Data = map[string]interface{}{
		"source": source,
		"stage":  stage,
		"error":  string(redactBytes([]byte(f.Error))),
	}
	sendHookEvent(session, event)
}

// clearSubmitFailure runs after any upload the server accepted.
func clearSubmitFailure() {
	_ = os.Remove(submitFailurePath())
}
