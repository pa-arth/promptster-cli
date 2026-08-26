//go:build windows

package main

import (
	"os"
	"os/exec"
)

// execCodex runs codex as a child with inherited stdio and propagates its exit
// code, because Windows has no exec-replace. The wrapper process staying alive
// is the cost; it is invisible to the candidate as long as the exit code and the
// three standard streams pass straight through.
func execCodex(bin string, argv []string, env []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
