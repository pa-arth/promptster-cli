package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pa-arth/promptster-cli/vsix"
)

// withEmptyPath makes editor detection deterministic by removing every editor
// CLI from PATH. Without it these tests pass or fail depending on whether the
// machine running them happens to have VS Code installed.
func withEmptyPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
}

// The load-bearing promise of §2.1: a candidate whose editor cannot take the
// .vsix still completes the assessment. Nothing in this path may return an
// error, exit, or panic — every failure is an availability state.
func TestInstallEditorExtensionIsNonFatal(t *testing.T) {
	withEmptyPath(t)

	// On macOS the app-bundle fallback can still find a real editor, and this
	// test must not install anything into a developer's actual editor. Only
	// assert the properties that hold either way.
	result := installEditorExtension(true)

	if result.Status == "" {
		t.Fatal("install must always produce a status")
	}
	if result.Note == "" {
		t.Error("install must always produce a note for the review surface")
	}
	if result.ExtensionVersion != vsix.Version {
		t.Errorf("extensionVersion = %q, want %q", result.ExtensionVersion, vsix.Version)
	}
	if result.VsixSHA256 != vsix.SHA256 {
		t.Errorf("recorded checksum %q does not match the embedded artifact %q",
			result.VsixSHA256, vsix.SHA256)
	}
}

func TestInstallEditorExtensionDeclined(t *testing.T) {
	result := installEditorExtension(false)

	if result.Status != editorCaptureDeclined {
		t.Errorf("status = %q, want %q", result.Status, editorCaptureDeclined)
	}
	if len(result.Installed) != 0 {
		t.Errorf("declined must install nothing, got %v", result.Installed)
	}
	// Even when declined, the version and checksum are recorded: the session
	// should say which build was on offer, not just that nothing happened.
	if result.ExtensionVersion == "" || result.VsixSHA256 == "" {
		t.Error("declined must still record which artifact was declined")
	}
}

// The distinction the whole change turns on: a session with no attention events
// because no editor was present is a different state from a candidate who
// opened no files, and the note has to say so in words a reviewer will read.
func TestEveryUnavailableStatusExplainsItself(t *testing.T) {
	cases := []struct {
		name   string
		result editorCaptureResult
	}{
		{"declined", installEditorExtension(false)},
		{"no editor", editorCaptureResult{
			Status: editorCaptureNoEditor,
			Note:   "No VS Code or Cursor found. This session has no editor attention capture; it is not a candidate who opened no files.",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.result.Status == editorCaptureInstalled {
				t.Skip("this case is about unavailability")
			}
			note := strings.ToLower(tc.result.Note)
			if !strings.Contains(note, "no editor attention") &&
				!strings.Contains(note, "not a candidate who opened no files") {
				t.Errorf("note does not distinguish unavailable from absent behaviour: %q", tc.result.Note)
			}
		})
	}
}

func TestEditorCaptureResultSerialisesForTheSession(t *testing.T) {
	result := editorCaptureResult{
		Status:           editorCaptureInstalled,
		Installed:        []string{"vscode"},
		Detected:         []string{"vscode", "cursor"},
		Failed:           map[string]string{"cursor": "timed out after 45s"},
		ExtensionVersion: vsix.Version,
		VsixSHA256:       vsix.SHA256,
		Note:             "installed",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back map[string]interface{}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// These key names are the contract with the backend's metadata merge and
	// the review surface. Renaming one silently blanks the review surface.
	for _, key := range []string{"status", "installed", "detected", "failed", "extensionVersion", "vsixSha256", "note"} {
		if _, ok := back[key]; !ok {
			t.Errorf("serialised result is missing %q", key)
		}
	}
}

func TestResolveEditorCLIReturnsEmptyWhenAbsent(t *testing.T) {
	withEmptyPath(t)

	fake := supportedEditor{
		key:        "nope",
		name:       "Nonexistent Editor",
		command:    "promptster-nonexistent-editor-cli",
		bundlePath: "/Applications/Definitely Not Installed.app/bin/nope",
	}
	if got := resolveEditorCLI(fake); got != "" {
		t.Errorf("resolveEditorCLI = %q, want empty", got)
	}
}

func TestRunInstallReportsFailureRatherThanHanging(t *testing.T) {
	// A CLI that does not exist must produce an error promptly, not block.
	done := make(chan error, 1)
	go func() {
		done <- runInstall("/definitely/not/a/real/editor/cli", "/tmp/nope.vsix")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a nonexistent editor CLI")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runInstall blocked on a nonexistent CLI")
	}
}

func TestWriteVsixToTempWritesTheEmbeddedBytes(t *testing.T) {
	path, cleanup, err := writeVsixToTemp()
	if err != nil {
		t.Fatalf("writeVsixToTemp: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged vsix: %v", err)
	}
	if len(data) != len(vsix.Bytes()) {
		t.Errorf("staged %d bytes, embedded artifact is %d", len(data), len(vsix.Bytes()))
	}
	if filepath.Base(path) != vsix.Filename() {
		t.Errorf("staged as %q, want %q", filepath.Base(path), vsix.Filename())
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cleanup left the staged artifact behind")
	}
}
