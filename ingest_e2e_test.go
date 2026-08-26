package main

import (
	"bufio"
	"net/http"
	"os"
	"testing"
	"time"
)

// End-to-end: a REAL codex 0.149.1 rollout -> codexRolloutProcessor -> the same
// ingestEventWithClient the watcher calls -> POST /v1/hooks/ingest.
// Gated: needs ROLLOUT, INGEST_SESSION and INGEST_KEY.
func TestIngestRealRolloutE2E(t *testing.T) {
	path, sess, key := os.Getenv("ROLLOUT"), os.Getenv("INGEST_SESSION"), os.Getenv("INGEST_KEY")
	if path == "" || sess == "" || key == "" {
		t.Skip("set ROLLOUT, INGEST_SESSION and INGEST_KEY")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	p := newCodexRolloutProcessor(sess, false)
	client := &http.Client{Timeout: 15 * time.Second}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024*1024), 8*1024*1024)

	sent, failed := 0, 0
	counts := map[string]int{}
	for sc.Scan() {
		for _, e := range p.process(sc.Bytes()) {
			if err := ingestEventWithClient(client, e, key); err != nil {
				failed++
				if failed <= 3 {
					t.Logf("INGEST FAIL %s: %v", e.Kind, err)
				}
				continue
			}
			sent++
			counts[e.Kind]++
		}
	}
	for k, v := range counts {
		t.Logf("sent %-14s %d", k, v)
	}
	t.Logf("TOTAL sent=%d failed=%d", sent, failed)
	if failed > 0 {
		t.Fatalf("%d events rejected by ingest", failed)
	}
}
