package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// openBriefWindow launches `promptster brief --here` (the live brief TUI) in
// a brand-new terminal window so the brief stays visible alongside the
// candidate's editor. Returns an error when no GUI terminal could be opened —
// callers should fall back to rendering inline.
func openBriefWindow() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return openMacTerminalWindow(briefShellCommand(exe))
	case "linux":
		return openLinuxTerminalWindow(briefShellCommand(exe))
	case "windows":
		// `start` opens a new console window. Env passthrough is skipped;
		// stateDir falls back to the active-workspace pointer.
		cmd := exec.Command("cmd", "/c", "start", "Promptster Brief", exe, "brief", "--here")
		return cmd.Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// briefShellCommand builds the POSIX shell command the new window runs.
// PROMPTSTER_STATE_DIR is forwarded when set (the spawned shell won't inherit
// it); otherwise stateDir() resolves via the active-workspace pointer.
func briefShellCommand(exe string) string {
	var b strings.Builder
	if sd := os.Getenv("PROMPTSTER_STATE_DIR"); sd != "" {
		b.WriteString("PROMPTSTER_STATE_DIR=" + shellQuoteArg(sd) + " ")
	}
	b.WriteString(shellQuoteArg(exe) + " brief --here")
	return b.String()
}

// shellQuoteArg single-quotes a string for safe interpolation into a POSIX
// shell command line.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptQuote escapes a string for embedding inside an AppleScript
// double-quoted string literal.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func openMacTerminalWindow(shellCmd string) error {
	quoted := appleScriptQuote(shellCmd)

	// Spawn a window in the terminal the candidate is already using when we
	// can script it; otherwise fall back to Terminal.app (always present).
	var script string
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		script = fmt.Sprintf(`tell application "iTerm"
	activate
	set newWindow to (create window with default profile)
	tell current session of newWindow to write text "%s"
end tell`, quoted)
	} else {
		script = fmt.Sprintf(`tell application "Terminal"
	activate
	do script "%s"
end tell`, quoted)
	}

	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func openLinuxTerminalWindow(shellCmd string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no graphical session (DISPLAY/WAYLAND_DISPLAY unset)")
	}

	// Each emulator has its own "run a command" convention; all of them can
	// hand off to `sh -c`.
	candidates := []struct {
		bin  string
		args []string
	}{
		{"x-terminal-emulator", []string{"-e"}},
		{"gnome-terminal", []string{"--"}},
		{"konsole", []string{"-e"}},
		{"xfce4-terminal", []string{"-x"}},
		{"kitty", nil},
		{"alacritty", []string{"-e"}},
		{"wezterm", []string{"start", "--"}},
		{"xterm", []string{"-e"}},
	}

	for _, c := range candidates {
		path, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		args := append(append([]string{}, c.args...), "sh", "-c", shellCmd)
		cmd := exec.Command(path, args...)
		if err := cmd.Start(); err == nil {
			// Detach: the terminal owns the process from here.
			go cmd.Wait() //nolint:errcheck
			return nil
		}
	}
	return fmt.Errorf("no supported terminal emulator found")
}
