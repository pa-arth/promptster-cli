package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// drainQueuedPrompts passes emit a prompt event for each queued message
// written to transcriptPath since the last call. Messages stamped before
// notBefore are skipped: a transcript resumed from before the session started
// is not part of it. Read, emit and offset update happen under one lock, and the
// offset only advances once every event in the batch has been emitted, so
// concurrent hooks never send a message twice and a hook killed mid-batch
// leaves the batch to be read again rather than lost. emit runs under the lock:
// it should persist the event, not wait on the network.
func drainQueuedPrompts(transcriptPath, sessionID string, notBefore time.Time, emit func(Event)) {
	if transcriptPath == "" {
		return
	}
	statePath := queuedScanStatePath()
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
				emit(e)
			}
		}
		state.Offsets[transcriptPath] = offset + int64(len(chunk))
		return saveQueuedScanState(statePath, state)
	})
	if err != nil {
		hookDebugf("queued prompt scan: %v", err)
	}
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

// Queued prompts are signed and buffered under the scan lock, but posted after
// it is released so no hook waits on the network behind another. Between the
// two, each one waits as a file in queuedPendingDir: a hook killed mid-send
// leaves the rest there, and the next hook sends them. Nothing else retries
// buffer.jsonl, so this directory is what makes the hand-off durable.

func queuedPendingDir() string {
	return filepath.Join(stateDir(), "queued-pending")
}

// savePendingQueued records a buffered queued prompt awaiting ingest.
func savePendingQueued(e Event) error {
	dir := queuedPendingDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, e.ID+".json")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// flushPendingQueued sends every pending queued prompt, oldest first, deleting
// each once the server has it. A send that may succeed later (network error,
// 5xx, 408, 429) stops the flush and keeps the rest for the next hook; a
// permanent rejection is dropped so it cannot block the queue forever. When
// another hook is already flushing, this one returns at once rather than wait
// or send the same prompts twice.
func flushPendingQueued(send func(Event) error) {
	dir := queuedPendingDir()
	_, err := tryBufferLock(dir+".lock", func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		type pending struct {
			path  string
			event Event
		}
		var queue []pending
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			var e Event
			data, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(data, &e) != nil {
				_ = os.Remove(path) // unreadable: nothing to send
				continue
			}
			queue = append(queue, pending{path, e})
		}
		sort.Slice(queue, func(i, j int) bool { return queue[i].event.Ts < queue[j].event.Ts })
		for _, q := range queue {
			if err := send(q.event); err != nil && !ingestRejectedPermanently(err) {
				return err
			}
			_ = os.Remove(q.path)
		}
		return nil
	})
	if err != nil {
		hookDebugf("queued prompt flush: %v", err)
	}
}
