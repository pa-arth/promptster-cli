package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendEventToLocalBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "buffer.jsonl")
	t.Setenv("PROMPTSTER_BUFFER_PATH", path)

	event := newEvent("prompt", "sess-1")
	event.Data = map[string]interface{}{"text": "hello"}

	if err := appendEventToLocalBuffer(&event); err != nil {
		t.Fatalf("appendEventToLocalBuffer: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read buffer: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "\"kind\":\"prompt\"") {
		t.Fatalf("buffer missing kind field: %s", s)
	}
	if !strings.Contains(s, "\"sessionId\":\"sess-1\"") {
		t.Fatalf("buffer missing sessionId field: %s", s)
	}
}
