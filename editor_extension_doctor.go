package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pa-arth/promptster-cli/vsix"
)

// The extension's marketplace identity: publisher.name from its package.json.
const editorExtensionID = "promptster.promptster"

// listExtensionsTimeout bounds the editor query in doctor. Shorter than
// installExtensionTimeout because this one only reads a list — it unpacks
// nothing. Measured at ~1s locally; this is slack, not a target.
const listExtensionsTimeout = 15 * time.Second

// captureStateFile is where the extension reports what it is doing, relative to
// the workspace root. Written by promptster-vscode's src/captureState.ts.
const captureStateFile = ".promptster/editor-capture.json"

// captureStateStaleAfter bounds how old a report may be and still count as
// "activated". The extension rewrites this on every reconcile, which includes
// activation and every session-file write.
const captureStateStaleAfter = 24 * time.Hour

// editorCaptureState mirrors what the extension writes. Fields it does not
// recognise are ignored; a newer extension must not fail an older doctor.
type editorCaptureState struct {
	ExtensionVersion string `json:"extensionVersion"`
	Editor           string `json:"editor"`
	EditorVersion    string `json:"editorVersion"`
	SessionID        string `json:"sessionId"`
	Capturing        bool   `json:"capturing"`
	Reason           string `json:"reason"`
	UpdatedAt        string `json:"updatedAt"`
}

// installedExtensionVersion returns the installed version of the Promptster
// extension in the given editor, or "" if it is not installed.
func installedExtensionVersion(e supportedEditor) (string, error) {
	cli := resolveEditorCLI(e)
	if cli == "" {
		return "", fmt.Errorf("no %s CLI on this machine", e.name)
	}

	// Bounded. This was exec.Command with no deadline, and `doctor` is the first
	// thing a confused candidate runs — an editor CLI that blocks would hang it
	// forever with no output. runInstall has had a timeout all along; this call
	// simply never got one. A query that lists installed extensions has no
	// reason to take longer than this.
	ctx, cancel := context.WithTimeout(context.Background(), listExtensionsTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cli, "--list-extensions", "--show-versions")
	// A first-run editor CLI can block on stdin. Give it nothing to read.
	cmd.Stdin = nil
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s --list-extensions timed out after %s", filepath.Base(cli), listExtensionsTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s --list-extensions failed: %v", filepath.Base(cli), err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		id, version, found := strings.Cut(line, "@")
		if !found {
			id, version = line, ""
		}
		if strings.EqualFold(id, editorExtensionID) {
			return version, nil
		}
	}
	return "", nil
}

func readCaptureState(workspace string) (*editorCaptureState, error) {
	data, err := os.ReadFile(filepath.Join(workspace, captureStateFile))
	if err != nil {
		return nil, err
	}
	var state editorCaptureState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// captureReasonHelp turns the extension's machine reason into something the
// candidate can act on.
func captureReasonHelp(reason string) string {
	switch reason {
	case "no-session":
		return "the extension found no session in this workspace\n    Fix: open your editor on the workspace promptster start prepared"
	case "consent-not-recorded":
		return "the extension found no recorded consent for this session\n    Fix: re-run promptster start in this workspace"
	case "session-expired":
		return "the session has expired\n    Fix: promptster start PST-XXXX-XXXX"
	case "paused":
		return "capture is paused\n    Fix: run \"Promptster: Resume Capture\" from the editor command palette"
	case "":
		return "the extension is not capturing and did not say why\n    Fix: reload the editor window"
	default:
		return "the extension reports: " + reason + "\n    Fix: reload the editor window"
	}
}

// doctorEditorExtension reports whether editor attention capture is working:
// installed, activated, and capturing.
//
// The three are checked separately on purpose. "Installed" is visible from
// outside and proves the least — an installed extension that never activated
// produces a session with no attention events, which on the review surface
// looks exactly like a candidate who opened no files. Only the extension's own
// report separates those.
func doctorEditorExtension(workspace string) {
	fmt.Println("Editor attention capture")

	editors := detectEditors()
	if len(editors) == 0 {
		check("Supported editor", func() (string, string) {
			// Not a failure. A candidate in vim completes the assessment; the
			// session records that this signal was unavailable.
			return "none (no VS Code or Cursor) — attention capture not available for this session", ""
		})
		fmt.Println()
		return
	}

	anyInstalled := false
	for _, e := range editors {
		editor := e
		check(editor.name+" extension", func() (string, string) {
			version, err := installedExtensionVersion(editor)
			if err != nil {
				return "", fmt.Sprintf("could not query %s: %v\n    Fix: re-run promptster start", editor.name, err)
			}
			if version == "" {
				return "", fmt.Sprintf("%s is not installed in %s\n    Fix: re-run promptster start (installs it), or install %s manually",
					editorExtensionID, editor.name, vsix.Filename())
			}
			anyInstalled = true
			if version != vsix.Version {
				// Not an error: an older build still captures. But a reviewer
				// reading the attention track deserves to know which collector
				// produced it, and skew is how that goes wrong quietly.
				return fmt.Sprintf("v%s installed (this CLI ships v%s — re-run promptster start to update)", version, vsix.Version), ""
			}
			return "v" + version, ""
		})
	}

	if workspace == "" {
		fmt.Println()
		return
	}

	check("Extension activated", func() (string, string) {
		state, err := readCaptureState(workspace)
		if err != nil {
			if os.IsNotExist(err) {
				if !anyInstalled {
					return "", "no capture state — the extension is not installed\n    Fix: re-run promptster start"
				}
				return "", "the extension is installed but has never reported a state — it has not activated in this workspace\n    Fix: open the workspace in your editor, or reload the window (Developer: Reload Window)"
			}
			return "", fmt.Sprintf("could not read %s: %v\n    Fix: reload the editor window", captureStateFile, err)
		}

		updated, parseErr := time.Parse(time.RFC3339, state.UpdatedAt)
		if parseErr != nil {
			return "", fmt.Sprintf("capture state has an unreadable timestamp %q\n    Fix: reload the editor window", state.UpdatedAt)
		}
		if age := time.Since(updated); age > captureStateStaleAfter {
			return "", fmt.Sprintf("last reported %s ago — the extension has not run recently in this workspace\n    Fix: reload the editor window",
				age.Round(time.Minute))
		}

		label := state.Editor
		if state.ExtensionVersion != "" {
			label += " v" + state.ExtensionVersion
		}
		return fmt.Sprintf("%s, last reported %s ago", label, time.Since(updated).Round(time.Second)), ""
	})

	check("Capturing", func() (string, string) {
		state, err := readCaptureState(workspace)
		if err != nil {
			// Already reported by the check above; don't say it twice.
			return "unknown (no capture state)", ""
		}
		if !state.Capturing {
			return "", captureReasonHelp(state.Reason)
		}
		if state.SessionID == "" {
			return "yes", ""
		}
		return "yes, session " + state.SessionID, ""
	})

	fmt.Println()
}
