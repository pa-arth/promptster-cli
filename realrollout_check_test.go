package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"testing"
)

// Throwaway harness: feed a REAL rollout through the processor and report what
// it emits. Run with -run TestRealRolloutCheck -v ROLLOUT=<path>.
func TestRealRolloutCheck(t *testing.T) {
	path := os.Getenv("ROLLOUT")
	if path == "" {
		t.Skip("no ROLLOUT set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := newCodexRolloutProcessor("sess-real", false)
	counts := map[string]int{}
	var cmds, diffs []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024*1024), 8*1024*1024)
	for sc.Scan() {
		for _, e := range p.process(sc.Bytes()) {
			counts[e.Kind]++
			d, _ := e.Data.(map[string]interface{})
			if e.Kind == "command" {
				c, _ := d["command"].(string)
				ec := d["exitCode"]
				cmds = append(cmds, fmt.Sprintf("[exit=%v] %.90s", ec, c))
			}
			if e.Kind == "file_diff" {
				pth, _ := d["path"].(string)
				diffs = append(diffs, fmt.Sprintf("%s +%v -%v", pth, d["linesAdded"], d["linesRemoved"]))
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-14s %d", k, counts[k])
	}
	for _, c := range cmds {
		t.Logf("CMD  %s", c)
	}
	for _, d := range diffs {
		t.Logf("DIFF %s", d)
	}
}
