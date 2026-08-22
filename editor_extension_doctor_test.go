package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCaptureStateFile(t *testing.T, workspace, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".promptster")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "editor-capture.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReadCaptureState(t *testing.T) {
	ws := t.TempDir()
	writeCaptureStateFile(t, ws, `{
  "extensionVersion": "0.3.0",
  "editor": "cursor",
  "editorVersion": "1.95.0",
  "sessionId": "sess_abc",
  "capturing": true,
  "updatedAt": "2026-08-21T10:00:00.000Z"
}`)

	state, err := readCaptureState(ws)
	if err != nil {
		t.Fatalf("readCaptureState: %v", err)
	}
	if !state.Capturing {
		t.Error("capturing should be true")
	}
	if state.SessionID != "sess_abc" || state.Editor != "cursor" || state.ExtensionVersion != "0.3.0" {
		t.Errorf("unexpected state: %+v", state)
	}
	if _, err := time.Parse(time.RFC3339, state.UpdatedAt); err != nil {
		t.Errorf("updatedAt is not RFC3339: %v", err)
	}
}

// A newer extension must not fail an older doctor. Fields doctor does not know
// about are ignored, and the ones it does know about still read.
func TestReadCaptureStateIgnoresUnknownFields(t *testing.T) {
	ws := t.TempDir()
	writeCaptureStateFile(t, ws, `{
  "extensionVersion": "9.9.9",
  "editor": "vscode",
  "capturing": false,
  "reason": "paused",
  "updatedAt": "2026-08-21T10:00:00.000Z",
  "somethingAddedLater": {"nested": true}
}`)

	state, err := readCaptureState(ws)
	if err != nil {
		t.Fatalf("readCaptureState: %v", err)
	}
	if state.Capturing || state.Reason != "paused" {
		t.Errorf("unexpected state: %+v", state)
	}
}

func TestReadCaptureStateMissingFile(t *testing.T) {
	_, err := readCaptureState(t.TempDir())
	if !os.IsNotExist(err) {
		t.Fatalf("want a not-exist error, got %v", err)
	}
}

// Every reason the extension can report must produce advice the candidate can
// act on. A doctor line that says "not capturing" and stops is the failure this
// check exists to prevent.
func TestCaptureReasonHelpCoversEveryReason(t *testing.T) {
	// Kept in sync with NotCapturingReason in promptster-vscode's src/types.ts.
	reasons := []string{"no-session", "consent-not-recorded", "session-expired", "paused"}
	for _, reason := range reasons {
		help := captureReasonHelp(reason)
		if help == "" {
			t.Errorf("%s: no help", reason)
			continue
		}
		if !strings.Contains(help, "Fix:") {
			t.Errorf("%s: help has no actionable Fix: line: %q", reason, help)
		}
	}

	// An unknown reason from a newer extension must still say something useful
	// rather than fall through to nothing.
	unknown := captureReasonHelp("some-future-reason")
	if !strings.Contains(unknown, "some-future-reason") || !strings.Contains(unknown, "Fix:") {
		t.Errorf("unknown reason produced unusable help: %q", unknown)
	}

	// So must a state that reports no reason at all.
	if help := captureReasonHelp(""); !strings.Contains(help, "Fix:") {
		t.Errorf("empty reason produced unusable help: %q", help)
	}
}

func TestExtensionIDMatchesTheArtifact(t *testing.T) {
	// publisher.name from promptster-vscode's package.json, lowercased — this is
	// what `code --list-extensions` prints, and a mismatch makes doctor report
	// "not installed" for an extension that is installed.
	if editorExtensionID != "promptster.promptster" {
		t.Errorf("editorExtensionID = %q; check promptster-vscode package.json publisher+name",
			editorExtensionID)
	}
}

func TestDoctorEditorExtensionDoesNotPanicWithoutAWorkspace(t *testing.T) {
	withEmptyPath(t)
	// doctor runs on machines with no editor, no workspace and no state file.
	// It prints; it must never fail the command.
	doctorEditorExtension("")
	doctorEditorExtension(t.TempDir())
}
