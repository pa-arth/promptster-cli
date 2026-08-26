//go:build !windows

package main

import "syscall"

// execCodex REPLACES this process with codex. No wrapper process is left sitting
// on the TTY, so codex owns the terminal exactly as a bare `codex` would — its
// TUI, its Ctrl-C handling, its exit code — and `promptster codex` costs the
// candidate nothing but the environment we injected. Returns only on failure.
func execCodex(bin string, argv []string, env []string) error {
	return syscall.Exec(bin, argv, env)
}
