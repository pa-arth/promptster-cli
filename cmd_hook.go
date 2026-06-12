package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func hookDebugEnabled() bool {
	if os.Getenv("PROMPTSTER_DEBUG") == "1" {
		return true
	}
	// Allow enabling hook debug without environment plumbing (Cursor may not pass env).
	_, err := os.Stat(filepath.Join(stateDir(), "debug-hooks"))
	return err == nil
}

func hookDebugf(format string, args ...interface{}) {
	if !hookDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "promptster hook: "+format+"\n", args...)
}

func hookDebugLogPath() string {
	return filepath.Join(stateDir(), "hook-debug.log")
}

func hookDebugAppend(line string) {
	if !hookDebugEnabled() {
		return
	}
	p := hookDebugLogPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func hookBufferPath() string {
	if p := os.Getenv("PROMPTSTER_BUFFER_PATH"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "buffer.jsonl")
}

// appendEventToLocalBuffer signs the event (if a session keypair exists),
// chains it to the previous buffered event's signature, appends it to the
// shared buffer.jsonl under an exclusive flock, and mutates the event with
// Sig/PrevSig so the caller can POST the signed version unchanged.
//
// If no session keypair is provisioned (old CLI or offline case), the event
// is appended unsigned and the buffer stays chain-less — the server marks
// sig_verified as null and `promptster verify` reports "unsigned session".
func appendEventToLocalBuffer(event *Event) error {
	// Defense-in-depth scrub at the choke point every capture path funnels
	// through, before the event is signed or persisted anywhere.
	scrubEvent(event)
	p := hookBufferPath()
	return withBufferLock(p, func() error {
		priv, err := loadSessionKeypair()
		if err != nil {
			hookDebugf("load session keypair: %v", err)
			// Fall through — still append unsigned so we don't drop events.
		}

		if priv != nil {
			prevSig, err := readLastChainSig(p)
			if err != nil {
				hookDebugf("read last chain sig: %v", err)
				// Empty prevSig is semantically fine if the buffer is corrupt;
				// chain-integrity check at verify time will catch the break.
				prevSig = ""
			}
			sig, _, err := signEvent(*event, prevSig, priv)
			if err != nil {
				return fmt.Errorf("sign event: %w", err)
			}
			event.Sig = sig
			event.PrevSig = prevSig
		}

		b, err := json.Marshal(event)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	})
}

// writeCursorHookResponse emits the stdout response Cursor expects, chosen so
// Promptster never blocks or alters the candidate's action. Of the events we
// register, only beforeSubmitPrompt is a gating hook — it requires
// {"continue":true} to let the prompt through. Every other registered event is
// an observer that needs no output (exit 0 implies proceed). We never emit a
// deny/exit-2, so a crashed handler always fails open.
func writeCursorHookResponse(eventName string) {
	if eventName == "beforeSubmitPrompt" {
		fmt.Fprintln(os.Stdout, `{"continue":true}`)
	}
}

// cmdHook is called by Claude Code hooks, Cursor hooks, and the shell hook.
// It must never block or disrupt IDE execution; all failures are best-effort.
func cmdHook(args []string) {
	// Route shell-cmd subcommand (from shell hook)
	if len(args) > 0 && args[0] == "shell-cmd" {
		cmdHookShellCmd(args[1:])
		return
	}

	// Cursor hooks all point at `promptster hook cursor`. Force the source so
	// detectSource is deterministic regardless of payload shape, and guarantee a
	// non-blocking response on every return path (see writeCursorHookResponse).
	cursorRoute := len(args) > 0 && args[0] == "cursor"
	if cursorRoute {
		os.Setenv("PROMPTSTER_HOOK_SOURCE", "cursor")
	}

	payloadRaw, err := io.ReadAll(os.Stdin)
	if err != nil {
		hookDebugf("stdin read error: %v", err)
		hookDebugAppend(time.Now().UTC().Format(time.RFC3339Nano) + " stdin_read_error")
		if cursorRoute {
			writeCursorHookResponse("")
		}
		return
	}
	if len(payloadRaw) == 0 {
		hookDebugf("no stdin payload")
		hookDebugAppend(time.Now().UTC().Format(time.RFC3339Nano) + " no_stdin_payload")
		if cursorRoute {
			writeCursorHookResponse("")
		}
		return
	}

	// Emit Cursor's non-blocking response up front — before the session checks,
	// redaction, or the synchronous ingest below — so we never delay or block the
	// candidate's action even if no session is active or ingest is slow.
	if cursorRoute {
		var probe struct {
			Name string `json:"hook_event_name"`
		}
		_ = json.Unmarshal(payloadRaw, &probe)
		writeCursorHookResponse(probe.Name)
	}

	session, err := loadSession()
	if err != nil {
		// No active session is expected in many environments; stay silent unless debugging.
		hookDebugf("session unavailable: %v", err)
		return
	}
	if session.SessionID == "" || session.SessionToken == "" {
		hookDebugf("session file missing sessionId or sessionToken")
		return
	}

	// GUI-launched editors may not inherit shell env vars.
	// Fall back to the API URL persisted in session.json during `promptster start`.
	if os.Getenv("PROMPTSTER_API_URL") == "" && session.ApiURL != "" {
		os.Setenv("PROMPTSTER_API_URL", session.ApiURL)
	}

	// Scrub secrets before any further processing or upload.
	payloadRaw = redactBytes(payloadRaw)

	var payload map[string]interface{}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		hookDebugf("invalid JSON payload: %v", err)
		return
	}

	event, ok := normalize(payload, session.SessionID)
	if !ok {
		return
	}

	// Strip the workspace prefix from any path fields so events emitted by
	// editor hooks (which carry absolute paths like /Users/x/workspace/foo.go)
	// unify with git-watcher events (which carry repo-relative paths like
	// foo.go). Without this, the replay treats them as two different files
	// and the editCount doubles.
	relativizeEventPaths(&event, session.TaskRoot)

	// Idempotency across capture channels: if the git watcher (or another
	// channel) already emitted a file_diff for this exact resulting content,
	// drop this one so the edit isn't double-counted. The hook normally wins
	// (it fires per-edit, faster than the 60s git poll), so this mostly guards
	// the rare reverse ordering.
	if !dedupeFileDiff(session.TaskRoot, &event) {
		hookDebugf("file_diff deduped (already emitted by another channel)")
		return
	}

	// Track file-change count for `promptster status` display. /explain is
	// fully optional commentary — the CLI never prompts anyone to write one.
	if event.Kind == "file_diff" {
		recordFileChange()
	}

	checkTimeLimit()
	// Push a workspace snapshot so server-side auto-submit (on time-limit
	// expiry) has candidate code to run tests against, even if the candidate
	// never manually runs `promptster done`. Internally throttled to ~30s.
	// File-edit hooks bypass the throttle so code is always fresh immediately
	// after a change, which matters when the time limit hits mid-edit.
	if event.Kind == "file_diff" {
		go forceSnapshotWorkspace(session)
	} else {
		go maybeSnapshotWorkspace(session)
	}

	// Transcript-capture mode (BYO subscription): the claude-watch daemon owns
	// the kinds it can read from the transcript JSONL; hooks stay installed as
	// a FALLBACK and for the lifecycle/intent kinds the transcript doesn't
	// carry. All side effects above (snapshot push, time-limit check, file
	// change counter) already ran — suppression only skips buffer + ingest.
	if suppressForTranscriptCapture(session, &event) {
		hookDebugf("suppressed kind=%s (transcript watcher healthy)", event.Kind)
		return
	}

	if err := appendEventToLocalBuffer(&event); err != nil {
		hookDebugf("buffer append error: %v", err)
	}

	hookDebugAppend(time.Now().UTC().Format(time.RFC3339Nano) + " sending " + apiURL() + "/v1/hooks/ingest")
	client := &http.Client{Timeout: 3 * time.Second}
	err = ingestEventWithClient(client, event, session.SessionToken)
	if err != nil {
		hookDebugAppend(time.Now().UTC().Format(time.RFC3339Nano) + " send_error " + err.Error())
		hookDebugf("send request: %v", err)
		return
	}
	hookDebugAppend(time.Now().UTC().Format(time.RFC3339Nano) + " sent kind=" + event.Kind)
	hookDebugf("event sent: kind=%v", event.Kind)
}
