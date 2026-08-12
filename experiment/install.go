package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// binPath is where install-hooks points the hook commands. Resolved from the
// running binary so a hook entry never references a path that does not exist.
func binPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "promptster-experiment"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// hookEntries is the settings.json fragment. Matcher "*" everywhere: the binary
// branches on `source`/`trigger` itself, which keeps the enforcement logic in
// one testable place instead of split across a config file.
func hookEntries() map[string]any {
	bin := binPath()
	entry := func(sub string) any {
		return map[string]any{
			"matcher": "*",
			"hooks": []any{
				map[string]any{"type": "command", "command": bin + " hook " + sub},
			},
		}
	}
	return map[string]any{
		"SessionStart":     []any{entry("session-start")},
		"PreCompact":       []any{entry("pre-compact")},
		"UserPromptSubmit": []any{entry("user-prompt-submit")},
	}
}

func cmdInstallHooks(args []string) int {
	fs := flag.NewFlagSet("install-hooks", flag.ExitOnError)
	write := fs.String("write", "", "merge into this settings.json (a .bak copy is made first)")
	_ = fs.Parse(args)

	frag := map[string]any{"hooks": hookEntries()}
	pretty, _ := json.MarshalIndent(frag, "", "  ")

	if *write == "" {
		fmt.Println("Add to your Claude Code settings.json (e.g. ~/.claude/settings.json):")
		fmt.Println()
		fmt.Println(string(pretty))
		fmt.Println()
		fmt.Println("Then verify with:  promptster-experiment status")
		return 0
	}

	path := *write
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s is not valid JSON: %v\n", path, err)
			return 1
		}
		if err := os.WriteFile(path+".promptster-experiment.bak", data, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "error: could not back up %s: %v\n", path, err)
			return 1
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for point, entries := range hookEntries() {
		existing, _ := hooks[point].([]any)
		// Idempotent: drop any prior entry of ours, then append the current one.
		kept := make([]any, 0, len(existing))
		for _, e := range existing {
			b, _ := json.Marshal(e)
			if strings.Contains(string(b), "promptster-experiment hook") {
				continue
			}
			kept = append(kept, e)
		}
		hooks[point] = append(kept, entries.([]any)...)
	}
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("installed 3 hook points into %s (backup: %s.promptster-experiment.bak)\n", path, path)
	fmt.Println("restart Claude Code sessions for the hooks to take effect")
	return 0
}
