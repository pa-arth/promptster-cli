package main

import "testing"

// The exact wrapper shape observed in production session 3b749f67 (codex-tui
// 0.149.1). Note the mixed key quoting the extractor has to tolerate: a bare
// `cmd:` identifier key next to a quoted "workdir" key.
func TestCodexUnwrapAgainstProductionShape(t *testing.T) {
	input := `const r = await tools.exec_command({cmd:"sed -n '1,240p' task.md","workdir":"/home/dev/checkout"});`
	name, args := unwrapCodexExec(map[string]interface{}{"input": input})
	if name != "exec_command" {
		t.Fatalf("name = %q, want exec_command", name)
	}
	if got := codexCommandString(args); got != "sed -n '1,240p' task.md" {
		t.Fatalf("cmd = %q", got)
	}
}
