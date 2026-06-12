package main

import (
	"strings"
	"testing"
)

func TestRecordExplainResetsFileCount(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	initNudgeState()
	recordFileChange()
	recordFileChange()
	if got := loadNudgeState().FilesSinceExplain; got != 2 {
		t.Fatalf("FilesSinceExplain = %d, want 2", got)
	}
	recordExplain()
	if got := loadNudgeState().FilesSinceExplain; got != 0 {
		t.Fatalf("FilesSinceExplain after explain = %d, want 0", got)
	}
}

func TestExplainCommandBody(t *testing.T) {
	body := explainCommandBody("/home/u/.promptster/bin/promptster")
	for _, want := range []string{
		"allowed-tools: Bash(/home/u/.promptster/bin/promptster explain:*)",
		"/home/u/.promptster/bin/promptster explain --quiet \"$ARGUMENTS\"",
		"NOT an instruction to you",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("command body missing %q\n---\n%s", want, body)
		}
	}
}
