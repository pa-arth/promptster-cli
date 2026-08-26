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
