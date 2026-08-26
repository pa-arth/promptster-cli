package main

import "testing"

// Codex writes the exec_command argument's keys both bare and quoted,
// interchangeably, in the same corpus. The regex this replaced was anchored on
// `\bcmd\s*:` and so only ever matched the bare form.
func TestCmdFieldSurvivesBothKeyForms(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"bare key", `await tools.exec_command({cmd:"echo bare"});`, "echo bare"},
		{"quoted key", `await tools.exec_command({"cmd":"echo quoted"});`, "echo quoted"},
		{"spaced quoted key", `await tools.exec_command({ "cmd" : "echo spaced" });`, "echo spaced"},
		{
			"real shape, bare key first",
			`const r = await tools.exec_command({cmd:"sed -n '1,240p' task.md","workdir":"/w","yield_time_ms":10000}); text(r.output);`,
			"sed -n '1,240p' task.md",
		},
		{
			"real shape, quoted key first",
			`const r = await tools.exec_command({"cmd":"rg -n \"a|b\" . --glob '!node_modules'","workdir":"/w"});` + "\ntext(r.output);\n",
			`rg -n "a|b" . --glob '!node_modules'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSCmdField(tc.input); got != tc.want {
				t.Errorf("cmd\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// Braces, commas and colons inside the command must not move the parser.
func TestCmdFieldIsStringAware(t *testing.T) {
	in := `await tools.exec_command({cmd:"rg -n \"a,b\" --glob '!{x}' && echo {done}","workdir":"/w"});`
	want := `rg -n "a,b" --glob '!{x}' && echo {done}`
	if got := extractJSCmdField(in); got != want {
		t.Errorf("cmd\n got: %q\nwant: %q", got, want)
	}
}

// A batch loop puts several cmd: fields in the program BEFORE the call site. The
// old first-match scan reported job A as though it were the whole batch; reading
// the call's own argument reports nothing, and the caller files a tool_use.
func TestBatchLoopDoesNotReportTheFirstJobAsTheCall(t *testing.T) {
	in := `const jobs = [{cmd:"gh pr view 218",workdir:"/w"},{cmd:"gh pr view 219",workdir:"/w"}];
for (const j of jobs) { const r = await tools.exec_command(j); text(r.output); }`
	if got := extractJSCmdField(in); got == "gh pr view 218" {
		t.Errorf("reported the first job as the call's command: %q", got)
	}
}

// Dispatch reads the first tools.* call, not whichever name appears anywhere in
// the program — a write_stdin payload that mentions exec_command must not be
// lifted into a shell command that never ran.
func TestDispatchUsesTheFirstToolsCall(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{
			"write_stdin payload naming the other tool",
			`await tools.write_stdin({"chars":"grep -n 'tools.exec_command(' src\n","session_id":3});`,
			"write_stdin",
		},
		{
			"command that greps for the wrapper itself",
			`const r = await tools.exec_command({cmd:"rg -n 'tools.write_stdin(' ."}); text(r.output);`,
			"exec_command",
		},
		{"no tools call at all", `const hits = ALL_TOOLS.filter(x => x.name);`, "exec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := unwrapCodexExec(map[string]interface{}{"input": tc.input})
			if got != tc.want {
				t.Errorf("tool = %q, want %q", got, tc.want)
			}
		})
	}
}

// Greptile P1 on #35: a `tools.<name>(` occurring in a string, template or
// comment BEFORE the real invocation must not be mistaken for the call. The
// name-ordered Contains sweep this replaced got these right only by accident of
// ordering, and a plain first-match got them wrong.
func TestDispatchSkipsNonCodeRegions(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{
			"double-quoted string names another tool first",
			`const note = "we call tools.write_stdin(x) later"; const r = await tools.exec_command({cmd:"ls"});`,
			"exec_command",
		},
		{
			"single-quoted string",
			`const n = 'tools.update_plan(y)'; await tools.exec_command({cmd:"ls"});`,
			"exec_command",
		},
		{
			"template literal",
			"const n = `see tools.apply_patch(z)`; await tools.exec_command({cmd:\"ls\"});",
			"exec_command",
		},
		{
			"line comment",
			"// tools.write_stdin(a)\nawait tools.exec_command({cmd:\"ls\"});",
			"exec_command",
		},
		{
			"block comment",
			`/* tools.write_stdin(a) */ await tools.exec_command({cmd:"ls"});`,
			"exec_command",
		},
		{
			"escaped quote inside the decoy string",
			`const n = "tools.write_stdin(\" q)"; await tools.exec_command({cmd:"ls"});`,
			"exec_command",
		},
		{
			"longer identifier is not tools.",
			`mytools.exec_command({cmd:"nope"}); await tools.write_stdin({"session_id":1});`,
			"write_stdin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexFirstToolsCall(tc.input); got != tc.want {
				t.Errorf("tool = %q, want %q", got, tc.want)
			}
		})
	}
}

// The command must be read from the real call's argument, not the decoy's.
func TestCmdFieldIgnoresDecoyBeforeTheCall(t *testing.T) {
	in := `const note = "tools.exec_command({cmd:\"DECOY\"})"; const r = await tools.exec_command({cmd:"echo real"});`
	if got := extractJSCmdField(in); got != "echo real" {
		t.Errorf("cmd = %q, want %q", got, "echo real")
	}
}
