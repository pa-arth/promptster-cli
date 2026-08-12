package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config is the per-engineer experiment identity. It is deliberately tiny: the
// assignment log is the ground truth, this file only says who is writing to it.
type Config struct {
	OrgID      string `json:"orgId"`
	EngineerID string `json:"engineerId"`
	// ExperimentKey is the backend's registered experiment identity and the
	// first component of its idempotency key.
	ExperimentKey string `json:"experimentKey"`
	// EngineerKey is the PSE- key `sync` posts with. Optional: the env var
	// PROMPTSTER_ENGINEER_KEY and the --key flag both work without it, and both
	// beat it. Stored in a 0600 file, so it is exactly as protected as the rest
	// of this directory and no more — do not put a key here on a shared machine.
	EngineerKey string `json:"engineerKey,omitempty"`
	// Enabled is the kill switch. Set false and every hook returns silently —
	// the experiment can be stopped mid-flight without uninstalling hooks.
	Enabled bool `json:"enabled"`
}

// rootDir is where all experiment state lives. Overridable for tests and for
// running a second (e.g. dry-run) instance without touching the real log.
func rootDir() string {
	if p := os.Getenv("PROMPTSTER_EXPERIMENT_DIR"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".promptster-experiment")
}

func configPath() string { return filepath.Join(rootDir(), "config.json") }

func loadConfig() (Config, error) {
	var c Config
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", configPath(), err)
	}
	if c.EngineerID == "" {
		return c, fmt.Errorf("%s has no engineerId", configPath())
	}
	if c.ExperimentKey == "" {
		return c, fmt.Errorf("%s has no experimentKey", configPath())
	}
	return c, nil
}

func saveConfig(c Config) error {
	if err := os.MkdirAll(rootDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), append(data, '\n'), 0o600)
}

// defaultEngineerID falls back to the git email so `init` needs no arguments.
func defaultEngineerID() string {
	out, err := exec.Command("git", "config", "user.email").Output()
	if err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
