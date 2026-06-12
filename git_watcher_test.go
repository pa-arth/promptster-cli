package main

import (
	"strings"
	"testing"
)

func TestSplitUnifiedDiff(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/src/main.ts b/src/main.ts",
		"index abc1234..def5678 100644",
		"--- a/src/main.ts",
		"+++ b/src/main.ts",
		"@@ -1,3 +1,4 @@",
		" import { foo } from './foo';",
		"-const x = 1;",
		"+const x = 2;",
		"+const y = 3;",
		" console.log(x);",
		"diff --git a/README.md b/README.md",
		"index 1111111..2222222 100644",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1,2 +1,2 @@",
		"-# Old Title",
		"+# New Title",
		" Some content",
	}, "\n")

	results := splitUnifiedDiff(raw)

	if len(results) != 2 {
		t.Fatalf("expected 2 file diffs, got %d", len(results))
	}

	// First file
	if results[0].path != "src/main.ts" {
		t.Errorf("file 0: expected path src/main.ts, got %q", results[0].path)
	}
	if results[0].linesAdded != 2 {
		t.Errorf("file 0: expected 2 lines added, got %d", results[0].linesAdded)
	}
	if results[0].linesRemoved != 1 {
		t.Errorf("file 0: expected 1 line removed, got %d", results[0].linesRemoved)
	}

	// Second file
	if results[1].path != "README.md" {
		t.Errorf("file 1: expected path README.md, got %q", results[1].path)
	}
	if results[1].linesAdded != 1 {
		t.Errorf("file 1: expected 1 line added, got %d", results[1].linesAdded)
	}
	if results[1].linesRemoved != 1 {
		t.Errorf("file 1: expected 1 line removed, got %d", results[1].linesRemoved)
	}
}

func TestSplitUnifiedDiffEmpty(t *testing.T) {
	results := splitUnifiedDiff("")
	if len(results) != 0 {
		t.Fatalf("expected 0 file diffs for empty input, got %d", len(results))
	}
}

func TestSplitUnifiedDiffNewFile(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/new-file.txt b/new-file.txt",
		"new file mode 100644",
		"index 0000000..abc1234",
		"--- /dev/null",
		"+++ b/new-file.txt",
		"@@ -0,0 +1,3 @@",
		"+line 1",
		"+line 2",
		"+line 3",
	}, "\n")

	results := splitUnifiedDiff(raw)
	if len(results) != 1 {
		t.Fatalf("expected 1 file diff, got %d", len(results))
	}
	if results[0].path != "new-file.txt" {
		t.Errorf("expected path new-file.txt, got %q", results[0].path)
	}
	if results[0].linesAdded != 3 {
		t.Errorf("expected 3 lines added, got %d", results[0].linesAdded)
	}
	if results[0].linesRemoved != 0 {
		t.Errorf("expected 0 lines removed, got %d", results[0].linesRemoved)
	}
}
