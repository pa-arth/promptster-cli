package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/pa-arth/promptster-cli/vsix"
)

// Editor attention capture — installing the promptster-vscode extension.
//
// The extension records which files the CANDIDATE opened and for how long. That
// is the one thing no other rail captures: agent file reads are already covered
// on the rails this product instruments — the Claude Code hooks and the Codex
// normalizer — and the signal only means something as a difference: files the
// human opened that the agent never read, files the agent edited that the human
// never opened, dwell before the first prompt.
//
// This used to cite `cursor_hooks.go` as one of those rails. There is no such
// file in THIS repo — it lives in promptster-teams-cli, a different product —
// and Cursor is not a rail here at all: `tool_select.go`'s allTools is
// {claude, codex}, Cursor having been retired by
// openspec changes/employer-supplied-model-key. A comment naming a sibling
// repo's file as if it were local is the same failure as a capability map
// grounded in the wrong repo; name the repo, or do not name the file.
//
// Two properties this file exists to hold:
//
//   - Installation is NON-FATAL. A candidate whose editor cannot take the .vsix
//     completes the assessment. Nothing here may exit, and nothing here may
//     block for long.
//   - The shortfall is RECORDED. A session with no attention events because no
//     instrumented editor was present must never read as a candidate who opened
//     no files. Those two readings support opposite hiring decisions, and the
//     reviewer has no way to tell them apart unless we say which happened.

// editorCaptureStatus is the availability of editor attention capture for a
// session. It is recorded on the session and surfaces on the review timeline.
type editorCaptureStatus string

const (
	// The extension was installed into at least one editor.
	editorCaptureInstalled editorCaptureStatus = "installed"
	// A supported editor is present but the install failed.
	editorCaptureInstallFailed editorCaptureStatus = "install_failed"
	// No supported editor found — vim, emacs, JetBrains, a remote shell.
	editorCaptureNoEditor editorCaptureStatus = "no_supported_editor"
	// Explicitly declined via --no-editor-extension.
	editorCaptureDeclined editorCaptureStatus = "declined"
)

// editorCaptureResult is what gets recorded on the session. Every field is
// about availability, never about the candidate.
type editorCaptureResult struct {
	Status editorCaptureStatus `json:"status"`
	// Editors the extension was successfully installed into ("vscode", "cursor").
	Installed []string `json:"installed,omitempty"`
	// Editors found but not installed into, with the reason.
	Failed map[string]string `json:"failed,omitempty"`
	// Supported editors detected on this machine, whether or not install worked.
	Detected []string `json:"detected,omitempty"`
	// Which extension build. Recorded so a reviewer reading an attention track
	// can tell which collector version produced it.
	ExtensionVersion string `json:"extensionVersion,omitempty"`
	VsixSHA256       string `json:"vsixSha256,omitempty"`
	// One-line human-readable summary for the review surface.
	Note string `json:"note,omitempty"`
}

// supportedEditor is an editor the extension can be installed into.
type supportedEditor struct {
	key string // stable id recorded on the session
	// Display name for CLI output.
	name string
	// CLI command name, and the macOS app-bundle path its CLI lives in when the
	// command is not on PATH. Candidates routinely have the app without ever
	// having run "Shell Command: Install 'code' command in PATH".
	command    string
	bundlePath string
}

func supportedEditors() []supportedEditor {
	return []supportedEditor{
		{
			key:        "vscode",
			name:       "VS Code",
			command:    "code",
			bundlePath: "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code",
		},
		{
			key:        "cursor",
			name:       "Cursor",
			command:    "cursor",
			bundlePath: "/Applications/Cursor.app/Contents/Resources/app/bin/cursor",
		},
	}
}

// resolveEditorCLI returns the path to the editor's CLI, or "" if not present.
func resolveEditorCLI(e supportedEditor) string {
	if path, err := exec.LookPath(e.command); err == nil {
		return path
	}
	if runtime.GOOS == "darwin" && e.bundlePath != "" {
		if st, err := os.Stat(e.bundlePath); err == nil && !st.IsDir() {
			return e.bundlePath
		}
	}
	return ""
}

// detectEditors returns the supported editors present on this machine.
func detectEditors() []supportedEditor {
	var found []supportedEditor
	for _, e := range supportedEditors() {
		if resolveEditorCLI(e) != "" {
			found = append(found, e)
		}
	}
	return found
}

// writeVsixToTemp materialises the embedded artifact so an editor CLI can read
// it. Editors take a path, not a stream.
func writeVsixToTemp() (string, func(), error) {
	dir, err := os.MkdirTemp("", "promptster-vsix-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	path := filepath.Join(dir, vsix.Filename())
	if err := os.WriteFile(path, vsix.Bytes(), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// installExtensionTimeout bounds the whole install step. An editor CLI that
// hangs — waiting on a first-run prompt, a locked extensions directory, a
// network-mounted home — must not hold up the assessment.
const installExtensionTimeout = 45 * time.Second

// installEditorExtension installs the embedded .vsix into every supported
// editor found, and returns what happened.
//
// It never returns an error: every failure is a recorded availability state,
// not a reason to stop. The caller prints a line and moves on.
func installEditorExtension(enabled bool) editorCaptureResult {
	result := editorCaptureResult{
		ExtensionVersion: vsix.Version,
		// Measured, not copied from the constant beside it. A checksum that is
		// only ever asserted proves nothing about the bytes actually installed.
		VsixSHA256: vsix.ActualSHA256(),
	}

	if !enabled {
		result.Status = editorCaptureDeclined
		result.Note = "Editor extension declined (--no-editor-extension). No editor attention events for this session."
		return result
	}

	editors := detectEditors()
	if len(editors) == 0 {
		result.Status = editorCaptureNoEditor
		result.Note = "No VS Code or Cursor found. This session has no editor attention capture; it is not a candidate who opened no files."
		return result
	}

	for _, e := range editors {
		result.Detected = append(result.Detected, e.key)
	}

	vsixPath, cleanup, err := writeVsixToTemp()
	if err != nil {
		result.Status = editorCaptureInstallFailed
		result.Failed = map[string]string{"*": fmt.Sprintf("could not stage the extension: %v", err)}
		result.Note = "Editor extension could not be staged for install. No editor attention events for this session."
		return result
	}
	defer cleanup()

	for _, e := range editors {
		cli := resolveEditorCLI(e)
		if cli == "" {
			continue
		}
		if err := runInstall(cli, vsixPath); err != nil {
			if result.Failed == nil {
				result.Failed = map[string]string{}
			}
			result.Failed[e.key] = err.Error()
			continue
		}
		result.Installed = append(result.Installed, e.key)
	}

	switch {
	case len(result.Installed) > 0:
		result.Status = editorCaptureInstalled
		result.Note = fmt.Sprintf("Editor extension %s installed into %s.",
			vsix.Version, strings.Join(result.Installed, ", "))
	default:
		result.Status = editorCaptureInstallFailed
		result.Note = "Editor extension could not be installed. This session has no editor attention capture; it is not a candidate who opened no files."
	}
	return result
}

// runInstall shells out to the editor's --install-extension.
//
// --force so a reinstall of the same version is a no-op rather than a prompt;
// the extension's own reattach handling is what keeps a reinstall from
// duplicating events, not the absence of a reinstall.
func runInstall(cli, vsixPath string) error {
	cmd := exec.Command(cli, "--install-extension", vsixPath, "--force")
	// A first-run editor CLI can block on stdin. Give it nothing to read.
	cmd.Stdin = nil

	done := make(chan error, 1)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not run %s: %v", filepath.Base(cli), err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %s", err, firstLine(output.String()))
		}
		return nil
	case <-time.After(installExtensionTimeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", installExtensionTimeout)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// recordEditorCapture reports editor-capture availability to the backend, on
// the session.
//
// This is the other half of the non-fatal promise. Installing is best-effort;
// SAYING SO is not optional, because absence-of-capture and absence-of-behaviour
// are different states and only one of them is about the candidate. The backend
// merges `editorCapture` from a session_start payload into sessions.metadata
// (routes/hooks.ts) and the review surface reads it from there.
//
// Best-effort delivery: a candidate whose network hiccups here still completes
// the assessment. The local event buffer keeps the record either way.
func recordEditorCapture(session *Session, result editorCaptureResult) {
	if session == nil || session.SessionID == "" {
		return
	}

	payload, err := json.Marshal(result)
	if err != nil {
		verbosef("editor capture: marshal result: %v", err)
		return
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(payload, &asMap); err != nil {
		verbosef("editor capture: remarshal result: %v", err)
		return
	}

	event := Event{
		ID:        newUUID(),
		SessionID: session.SessionID,
		Ts:        time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      "session_start",
		Source:    "cli",
		V:         1,
		Actor:     systemActor(),
		Data: map[string]interface{}{
			"editorCapture": asMap,
			"editorVersion": "promptster-cli",
		},
	}

	if err := appendEventToLocalBuffer(&event); err != nil {
		verbosef("editor capture: buffer append: %v", err)
	}
	if err := ingestEventWithAPIKey(event, session.SessionToken); err != nil {
		// Non-fatal by construction. The buffer above still holds it.
		verbosef("editor capture: ingest: %v", err)
	}
}

// printEditorCaptureLine reports the outcome to the candidate.
//
// Said out loud, in both directions. A candidate is entitled to know that
// something was installed into their editor, and a candidate whose editor could
// not take it is entitled to know that this is fine and their assessment is
// unaffected — rather than discovering a warning-shaped message and wondering
// whether they have broken something.
func printEditorCaptureLine(result editorCaptureResult) {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	check := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
	info := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("·")

	fmt.Println()
	switch result.Status {
	case editorCaptureInstalled:
		names := make([]string, 0, len(result.Installed))
		for _, key := range result.Installed {
			names = append(names, editorDisplayName(key))
		}
		fmt.Printf("  %s  Editor extension installed into %s\n", check, strings.Join(names, " and "))
		fmt.Printf("     %s\n", dim.Render("Records which files you open and for how long. No file contents. Pause any time from the command palette."))
	case editorCaptureNoEditor:
		fmt.Printf("  %s  No VS Code or Cursor found — skipping the editor extension\n", info)
		fmt.Printf("     %s\n", dim.Render("Your assessment is unaffected. The session records that editor attention capture was unavailable."))
	case editorCaptureDeclined:
		fmt.Printf("  %s  Editor extension skipped (--no-editor-extension)\n", info)
		fmt.Printf("     %s\n", dim.Render("Your assessment is unaffected. The session records that editor attention capture was declined."))
	default:
		fmt.Printf("  %s  Editor extension could not be installed — continuing without it\n", info)
		for editor, reason := range result.Failed {
			fmt.Printf("     %s\n", dim.Render(editorDisplayName(editor)+": "+reason))
		}
		fmt.Printf("     %s\n", dim.Render("Your assessment is unaffected. The session records that editor attention capture was unavailable."))
	}
}

func editorDisplayName(key string) string {
	for _, e := range supportedEditors() {
		if e.key == key {
			return e.name
		}
	}
	if key == "*" {
		return "editor"
	}
	return key
}
