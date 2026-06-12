package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// checkForUpdate queries the API for the minimum required CLI version
// and warns (or exits) if the current binary is too old.
func checkForUpdate() {
	if version == "dev" {
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/cli/version-check?version="+version, nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Promptster-CLI-Version", version)

	resp, err := client.Do(req)
	if err != nil {
		return // network error — don't block the user
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return
	}

	var result struct {
		UpdateRequired    bool   `json:"updateRequired"`
		UpdateRecommended bool   `json:"updateRecommended"`
		LatestVersion     string `json:"latestVersion"`
		MinVersion        string `json:"minVersion"`
		Message           string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	warningStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#f59e0b")).
		Bold(true)

	errorStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ef4444")).
		Bold(true)

	if result.UpdateRequired {
		fmt.Println()
		fmt.Println(errorStyle.Render("✗ Your CLI version is no longer supported."))
		fmt.Printf("  Current: %s  |  Minimum: %s  |  Latest: %s\n", version, result.MinVersion, result.LatestVersion)
		if result.Message != "" {
			fmt.Printf("  %s\n", result.Message)
		}
		fmt.Println()
		printUpdateHint()
		fmt.Println()
		os.Exit(1)
	}

	if result.UpdateRecommended {
		fmt.Println()
		fmt.Println(warningStyle.Render("⚠ A newer CLI version is available."))
		fmt.Printf("  Current: %s  →  Latest: %s\n", version, result.LatestVersion)
		printUpdateHint()
		fmt.Println()
	}
}

// printUpdateHint shows the hosted installer for the default Promptster API.
// Self-hosted setups (PROMPTSTER_API_URL) distribute the binary themselves, so
// the hosted curl one-liner would be wrong there.
func printUpdateHint() {
	if usingDefaultAPI() {
		fmt.Println("  Update with: curl -fsSL https://get.promptster.ai | sh")
		fmt.Println("         or:   npm install -g @promptster/cli@latest")
		return
	}
	fmt.Println("  Update with: npm install -g @promptster/cli@latest")
	fmt.Println("         or:   via your organization's install channel")
}

// compareVersions returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := parseVersion(a)
	bParts := parseVersion(b)
	for i := 0; i < 3; i++ {
		if aParts[i] < bParts[i] {
			return -1
		}
		if aParts[i] > bParts[i] {
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var result [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(strings.Split(parts[i], "-")[0])
		result[i] = n
	}
	return result
}
