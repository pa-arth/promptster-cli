package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// decisionWatcherState is the shape of the state files that older CLI
// versions wrote to track background watcher processes. We keep the type
// and path helpers so we can locate and terminate any such orphaned
// processes left behind on upgrade.
type decisionWatcherState struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Command   string `json:"command"`
}

var runCombinedOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func decisionWatcherStatePath() string {
	if p := os.Getenv("PROMPTSTER_DECISION_WATCHER_STATE"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "decision-watcher.json")
}

func decisionWatcherSurfaceStatePath() string {
	if p := os.Getenv("PROMPTSTER_DECISION_WATCHER_SURFACE_STATE"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "decision-watcher-surface.json")
}

func loadDecisionProcessState(path string) (decisionWatcherState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return decisionWatcherState{}, err
	}
	var state decisionWatcherState
	if err := json.Unmarshal(data, &state); err != nil {
		return decisionWatcherState{}, err
	}
	return state, nil
}

func clearDecisionProcessState(path string) {
	_ = os.Remove(path)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := runCombinedOutput("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid))
		if err != nil {
			return false
		}
		text := string(out)
		return strings.Contains(text, fmt.Sprintf(" %d ", pid)) || strings.Contains(text, fmt.Sprintf(",%d", pid))
	}
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}

// stopDecisionWatchers terminates any legacy watcher processes left behind
// by older CLI versions. Modern builds no longer spawn these, but upgrades
// may leave running daemons tied to stale state files.
//
// Strategy: signal the PID recorded in each state file, then sweep pgrep for
// any orphan whose state file was lost. Each call blocks until the process
// exits or is SIGKILLed after a 2s grace period.
func stopDecisionWatchers() {
	stopDecisionProcess(decisionWatcherStatePath())
	stopDecisionProcess(decisionWatcherSurfaceStatePath())
	killStalePromptsterDaemons("promptster decide service")
	killStalePromptsterDaemons("promptster decide watch")
	clearDecisionProcessState(decisionWatcherStatePath())
	clearDecisionProcessState(decisionWatcherSurfaceStatePath())
}

func stopDecisionProcess(statePath string) {
	state, err := loadDecisionProcessState(statePath)
	if err != nil || state.PID <= 0 {
		return
	}
	signalAndWaitForExit(state.PID)
}

func pendingDecisionCount() (int, error) {
	items, err := loadDecisionQueue()
	if err != nil {
		return 0, err
	}
	return len(items), nil
}
