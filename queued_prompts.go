package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Hook-mode capture of mid-turn messages. A message the candidate types while
// Claude is working is injected into the running turn without firing
// UserPromptSubmit (see handleQueuedPrompt), so the hook stream never sees it.
// Every Claude hook payload names the transcript, though, so each hook reads
// the lines appended since the previous hook and lifts the queued prompts out.
// The transcript watcher does the same in transcript-capture mode; there the
// events are suppressed like any other prompt the watcher owns.

// queuedScanMaxBytes bounds one scan, so a hook that finds a large unread
// backlog still returns promptly. The rest is read by the next hook.
const queuedScanMaxBytes = 8 << 20

// queuedScanState is the per-transcript byte offset already scanned.
type queuedScanState struct {
	Offsets map[string]int64 `json:"offsets"`
}

func queuedScanStatePath() string {
	return filepath.Join(stateDir(), "claude-queued-scan.json")
}

// drainQueuedPrompts returns prompt events for queued messages written to
// transcriptPath since the last call. Messages stamped before notBefore are
// skipped: a transcript resumed from before the session started is not part of
// it. The read and the offset update happen under one lock so concurrent hooks
// never emit the same message twice.
func drainQueuedPrompts(transcriptPath, sessionID string, notBefore time.Time) []Event {
	if transcriptPath == "" {
		return nil
	}
	statePath := queuedScanStatePath()
	var events []Event
	err := withBufferLock(statePath+".lock", func() error {
		state := queuedScanState{Offsets: map[string]int64{}}
		if data, err := os.ReadFile(statePath); err == nil {
			_ = json.Unmarshal(data, &state)
			if state.Offsets == nil {
				state.Offsets = map[string]int64{}
			}
		}
		offset := state.Offsets[transcriptPath]

		f, err := os.Open(transcriptPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if info, err := f.Stat(); err == nil && info.Size() < offset {
			offset = 0 // rewritten or truncated: start over
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		chunk, err := io.ReadAll(io.LimitReader(f, queuedScanMaxBytes))
		if err != nil {
			return err
		}
		// Only whole lines; a partial last line is read again next time.
		end := bytes.LastIndexByte(chunk, '\n')
		if end < 0 {
			if len(chunk) == queuedScanMaxBytes {
				// One line longer than the cap carries nothing we read; skip it.
				state.Offsets[transcriptPath] = offset + int64(len(chunk))
				return saveQueuedScanState(statePath, state)
			}
			return nil
		}
		chunk = chunk[:end+1]

		p := newClaudeTranscriptProcessor(sessionID, false)
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			// Cheap prefilter: nearly every line is something else.
			if !bytes.Contains(line, []byte(`"queued_command"`)) {
				continue
			}
			for _, e := range p.process(redactBytes(line)) {
				if t, err := time.Parse(time.RFC3339Nano, e.Ts); err == nil && t.Before(notBefore) {
					continue
				}
				events = append(events, e)
			}
		}
		state.Offsets[transcriptPath] = offset + int64(len(chunk))
		return saveQueuedScanState(statePath, state)
	})
	if err != nil {
		hookDebugf("queued prompt scan: %v", err)
	}
	return events
}

func saveQueuedScanState(path string, state queuedScanState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
