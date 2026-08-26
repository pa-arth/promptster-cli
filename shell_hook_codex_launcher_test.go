package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The codex launcher is the whole codex rail on the terminal side, and it is
// written in shell, so string assertions about the template prove nothing about
// what a shell actually does with it. These tests run the rendered hook in a
// real interactive shell with a fake codex and a fake promptster on PATH, and
// check where a typed `codex` ends up.
//
// The four cases are the four ways the old design failed. It selected the
// provider in a GLOBAL config file, so: it could not tell inside-the-workspace
// from outside, it could not stop applying when the session ended, it could not
// avoid clobbering a user who had their own codex setup, and it broke a machine
// with no live session at all.

// runHookScript sources the rendered hook in a real interactive shell and
// returns everything it printed. preamble is sourced BEFORE the hook, the way a
// user's own rc definitions would already be in place when ours is appended.
func runHookScript(t *testing.T, shell, home, pathDir, preamble, body string) string {
	t.Helper()
	bin, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s not available", shell)
	}

	script := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(script, []byte(shellHookScript()), 0o644); err != nil {
		t.Fatal(err)
	}

	// -i because the hook deliberately no-ops in non-interactive shells (it
	// installs a DEBUG trap, which breaks scp and friends). --norc/-f so the
	// developer's own rc files stay out of it.
	//
	// _promptster_hook_script is for the bodies that need to source the hook a
	// SECOND time — the re-source path, where a self-recursive wrapper would
	// otherwise be born.
	args := []string{"-i", "-c",
		"_promptster_hook_script=" + script + "\n" + preamble + "\n. " + script + "\n" + body}
	if shell == "bash" {
		args = append([]string{"--norc", "--noprofile"}, args...)
	} else {
		args = append([]string{"-f"}, args...)
	}

	// A regression in the launcher is a runaway, not a wrong answer: the wrapper
	// calls itself, and bash with FUNCNEST unset recurses until the machine
	// gives out. Bound both — FUNCNEST so bash reports instead of spinning, and
	// the context so any other hang fails the test rather than wedging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+pathDir+":/usr/bin:/bin", "FUNCNEST=50")
	out, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s hung on the hook (30s):\n%s", shell, out)
	}
	return string(out)
}

// codexLauncherFixture stands up a fake workspace with a live session, plus a
// fake `codex` binary and a fake `promptster` to tell the two paths apart.
func codexLauncherFixture(t *testing.T) (home, ws, elsewhere, binDir string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	ws = filepath.Join(root, "ws")
	elsewhere = filepath.Join(root, "elsewhere")
	binDir = filepath.Join(root, "bin")
	for _, d := range []string{filepath.Join(home, ".promptster"), filepath.Join(ws, ".promptster"), elsewhere, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".promptster", "active-workspace"), []byte(ws), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".promptster", "session.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("codex", `echo "REAL-CODEX $*"`)
	write("fakepromptster", `echo "VIA-PROMPTSTER $*"`)
	return home, ws, elsewhere, binDir
}

func eachShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook")
	}
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) { fn(t, shell) })
	}
}

// Inside the workspace a typed `codex` must reach `promptster codex`, which is
// what supplies the provider and the credential. Outside it must reach the real
// binary, unchanged — the property the global config could never have.
func TestCodexLauncherRoutesByWorkspace(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		home, ws, elsewhere, binDir := codexLauncherFixture(t)
		out := runHookScript(t, shell, home, binDir, "", `
_promptster_bin=`+filepath.Join(binDir, "fakepromptster")+`
cd `+ws+` && printf 'inside:'  && codex hello
cd `+elsewhere+` && printf 'outside:' && codex hello
`)
		if !strings.Contains(out, "inside:VIA-PROMPTSTER codex hello") {
			t.Errorf("inside the workspace codex did not route through promptster:\n%s", out)
		}
		if !strings.Contains(out, "outside:REAL-CODEX hello") {
			t.Errorf("outside the workspace codex did not reach the real binary:\n%s", out)
		}
	})
}

// The session ending must return `codex` to normal in shells that are ALREADY
// OPEN. This is the half of the reported bug a config file cannot fix: a global
// `model_provider` written at start stayed authoritative in every shell,
// forever, including after `done`.
func TestCodexLauncherGoesInertWhenTheSessionEnds(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		home, ws, _, binDir := codexLauncherFixture(t)
		out := runHookScript(t, shell, home, binDir, "", `
_promptster_bin=`+filepath.Join(binDir, "fakepromptster")+`
cd `+ws+` && printf 'live:' && codex hello
rm `+filepath.Join(ws, ".promptster", "session.json")+`
printf 'ended:' && codex hello
`)
		if !strings.Contains(out, "live:VIA-PROMPTSTER codex hello") {
			t.Errorf("live session did not route through promptster:\n%s", out)
		}
		if !strings.Contains(out, "ended:REAL-CODEX hello") {
			t.Errorf("codex stayed hijacked after the session ended, in the same shell:\n%s", out)
		}
	})
}

// A user who already has their own `codex` function or alias keeps it. Promptster
// gets one uninvited global change to a candidate's machine less than it used to,
// not one more.
func TestCodexLauncherDoesNotShadowTheUsersOwn(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		home, ws, _, binDir := codexLauncherFixture(t)
		body := `
_promptster_bin=` + filepath.Join(binDir, "fakepromptster") + `
cd ` + ws + ` && codex hello
`
		// Sanity: with nothing else defined, the launcher is active here.
		if out := runHookScript(t, shell, home, binDir, "", body); !strings.Contains(out, "VIA-PROMPTSTER") {
			t.Fatalf("fixture broken, expected the launcher to be active:\n%s", out)
		}

		// And it stands down when the user got there first.
		out := runHookScript(t, shell, home, binDir, `codex() { echo "USER-OWN $*"; }`, body)
		if !strings.Contains(out, "USER-OWN hello") || strings.Contains(out, "VIA-PROMPTSTER") {
			t.Errorf("the hook shadowed the user's own codex function:\n%s", out)
		}
	})
}

// Sourcing the hook on a machine with no codex installed must not invent one.
// A defined-but-broken `codex` would be a new failure on a machine that simply
// does not have the tool.
func TestCodexLauncherAbsentWhenCodexIsNotInstalled(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		home, _, _, binDir := codexLauncherFixture(t)
		if err := os.Remove(filepath.Join(binDir, "codex")); err != nil {
			t.Fatal(err)
		}
		out := runHookScript(t, shell, home, binDir, "", "type codex 2>&1 | head -1")
		if strings.Contains(out, "function") {
			t.Errorf("hook defined a codex function with no codex installed:\n%s", out)
		}
	})
}

// Sourcing the hook twice in one shell must leave a working `codex`, not a
// function that calls itself. `command -v codex` answers with our own function
// NAME once the wrapper is installed, so a second source that wrote that answer
// straight into the variable the wrapper calls produced unbounded recursion —
// zsh aborts with "maximum nested function level reached", bash (FUNCNEST
// unset) never stops. Every `source ~/.zshrc` after an rc edit hit this.
func TestCodexLauncherSurvivesBeingSourcedTwice(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		home, ws, elsewhere, binDir := codexLauncherFixture(t)
		out := runHookScript(t, shell, home, binDir, "", `
. "$_promptster_hook_script"
_promptster_bin=`+filepath.Join(binDir, "fakepromptster")+`
cd `+ws+` && printf 'inside:'  && codex hello
cd `+elsewhere+` && printf 'outside:' && codex hello
`)
		if !strings.Contains(out, "inside:VIA-PROMPTSTER codex hello") {
			t.Errorf("after a re-source, inside the workspace codex did not route through promptster:\n%s", out)
		}
		if !strings.Contains(out, "outside:REAL-CODEX hello") {
			t.Errorf("after a re-source, codex outside the workspace did not reach the real binary:\n%s", out)
		}
	})
}
