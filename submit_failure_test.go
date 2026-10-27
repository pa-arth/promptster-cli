package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecordSubmitFailureReachesTheRecordOnceAndStatus(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	var mu sync.Mutex
	var kinds []string
	var data []map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hooks/ingest" {
			var e struct {
				Kind string                 `json:"kind"`
				Data map[string]interface{} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&e)
			mu.Lock()
			kinds = append(kinds, e.Kind)
			data = append(data, e.Data)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	t.Setenv("PROMPTSTER_API_URL", server.URL)
	t.Setenv("PROMPTSTER_BUFFER_PATH", filepath.Join(stateDir(), "buffer.jsonl"))

	session := Session{SessionID: "sess-1", SessionToken: "PST-TEST"}
	tooLarge := errors.New("workspace too large — exceeds 50 MB after exclusions")

	// A snapshot failing the same way every 30s is one record, not one per retry.
	recordSubmitFailure(session, "snapshot", "bundle", tooLarge)
	recordSubmitFailure(session, "snapshot", "bundle", tooLarge)
	// `done` always records: each run is a candidate action.
	recordSubmitFailure(session, "done", "bundle", tooLarge)

	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 2 || kinds[0] != "submission_failed" || kinds[1] != "submission_failed" {
		t.Fatalf("ingested kinds = %v, want two submission_failed", kinds)
	}
	if data[1]["source"] != "done" || data[1]["stage"] != "bundle" || data[1]["error"] != tooLarge.Error() {
		t.Fatalf("done event data = %v", data[1])
	}

	f, ok := loadSubmitFailure()
	if !ok || f.Source != "done" || f.Error != tooLarge.Error() {
		t.Fatalf("status state = %+v ok=%v", f, ok)
	}
	clearSubmitFailure()
	if _, ok := loadSubmitFailure(); ok {
		t.Fatal("an accepted upload must clear the failure status shows")
	}
}
