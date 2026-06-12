package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	decisionQueueVersion     = 1
	decisionPromptCooldown   = 2 * time.Minute
	decisionQueueWatchPeriod = 5 * time.Second
)

const (
	decisionPromptModeQueue    = "queue"
	decisionPromptModeBlocking = "blocking"
	decisionPromptModeOff      = "off"
)

var openDecisionTTY = func() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

type decisionCandidate struct {
	ID               string                 `json:"id"`
	SessionID        string                 `json:"sessionId"`
	DetectedAt       string                 `json:"detectedAt"`
	Source           string                 `json:"source"`
	TriggerEventID   string                 `json:"triggerEventId"`
	TriggerKind      string                 `json:"triggerKind"`
	Title            string                 `json:"title"`
	ChosenOption     string                 `json:"chosenOption"`
	Context          string                 `json:"context"`
	TradeoffsHint    string                 `json:"tradeoffsHint,omitempty"`
	CategoryHint     string                 `json:"categoryHint,omitempty"`
	ImpactScore      int                    `json:"impactScore"`
	Severity         string                 `json:"severity"`
	Reason           string                 `json:"reason"`
	CapturedVia      string                 `json:"capturedVia,omitempty"`
	RawEvent         map[string]interface{} `json:"rawEvent,omitempty"`
	InstructionLabel string                 `json:"instructionLabel,omitempty"`
}

type decisionCaptureRecord struct {
	Title             string
	Context           string
	ImpactScore       int
	ChosenOption      string
	Tradeoffs         string
	Rationale         string
	DecisionID        string
	SessionID         string
	SourceService     string
	CapturedVia       string
	TriggerEventID    string
	TriggerKind       string
	CategoryHint      string
	InstructionLabel  string
	RationalePrompted bool
}

func decisionQueuePath() string {
	if p := os.Getenv("PROMPTSTER_DECISION_QUEUE_PATH"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "decision-queue.jsonl")
}

func decisionPromptStatePath() string {
	if p := os.Getenv("PROMPTSTER_DECISION_PROMPT_STATE"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "decision-prompt.state")
}

func readDecisionPromptState() (string, time.Time) {
	data, err := os.ReadFile(decisionPromptStatePath())
	if err != nil {
		return "", time.Time{}
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), "|", 2)
	if len(parts) != 2 {
		return "", time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return "", time.Time{}
	}
	return parts[0], ts
}

func writeDecisionPromptState(key string, ts time.Time) {
	_ = os.MkdirAll(filepath.Dir(decisionPromptStatePath()), 0o755)
	_ = os.WriteFile(decisionPromptStatePath(), []byte(key+"|"+ts.UTC().Format(time.RFC3339Nano)), 0o644)
}

func enqueueDecisionCandidate(candidate decisionCandidate) error {
	path := decisionQueuePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	row := map[string]interface{}{
		"version":   decisionQueueVersion,
		"candidate": candidate,
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func loadDecisionQueue() ([]decisionCandidate, error) {
	data, err := os.ReadFile(decisionQueuePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []decisionCandidate
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row struct {
			Candidate decisionCandidate `json:"candidate"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Candidate.ID != "" {
			out = append(out, row.Candidate)
		}
	}
	return out, scanner.Err()
}

func saveDecisionQueue(candidates []decisionCandidate) error {
	path := decisionQueuePath()
	if len(candidates) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, candidate := range candidates {
		if err := enc.Encode(map[string]interface{}{
			"version":   decisionQueueVersion,
			"candidate": candidate,
		}); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func detectDecisionCandidate(event Event) *decisionCandidate {
	data, _ := event.Data.(map[string]interface{})
	if data == nil {
		return nil
	}

	base := decisionCandidate{
		ID:             newUUID(),
		SessionID:      event.SessionID,
		DetectedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Source:         firstNonEmpty(event.Source, "hook"),
		TriggerEventID: event.ID,
		TriggerKind:    event.Kind,
		Severity:       "medium",
		ImpactScore:    3,
		RawEvent: map[string]interface{}{
			"id":   event.ID,
			"kind": event.Kind,
			"ts":   event.Ts,
			"data": data,
		},
	}

	switch event.Kind {
	case "planning":
		todoCount := 0
		if todos, ok := data["todos"].([]interface{}); ok {
			todoCount = len(todos)
		}
		if todoCount < 4 {
			return nil
		}
		base.Title = "Defined a concrete implementation plan"
		base.ChosenOption = fmt.Sprintf("Execute a %d-step implementation plan", todoCount)
		base.Context = fmt.Sprintf("The agent wrote a TodoWrite plan with %d items before making code changes.", todoCount)
		base.CategoryHint = "planning"
		base.Reason = "Multi-step planning usually reflects an execution strategy worth preserving."
		return &base
	case "task_dispatch":
		taskPreview, _ := data["taskPreview"].(string)
		if len(strings.TrimSpace(taskPreview)) < 30 {
			return nil
		}
		base.Title = "Delegated work to a sub-task"
		base.ChosenOption = "Use a delegated sub-task instead of keeping all work in one thread"
		base.Context = fmt.Sprintf("The agent dispatched a sub-task: %s", taskPreview)
		base.CategoryHint = "execution"
		base.Reason = "Task decomposition can signal judgment about scope and parallelism."
		return &base
	case "command":
		command, _ := data["command"].(string)
		if !looksArchitecturalCommand(command) {
			return nil
		}
		base.Title = summarizeCommandDecision(command)
		base.ChosenOption = command
		base.Context = fmt.Sprintf("The agent ran an infrastructure or architecture-shaping command: %s", command)
		base.CategoryHint = classifyCommandCategory(command)
		base.Reason = "This command likely changes system shape, dependencies, schema, or deployment behavior."
		base.ImpactScore = 4
		return &base
	case "file_diff":
		path, _ := data["path"].(string)
		oldStr, _ := data["oldString"].(string)
		newStr, _ := data["newString"].(string)
		changeContent := oldStr + "\n" + newStr
		if !looksArchitecturalFile(path, changeContent) {
			return nil
		}
		base.Title = summarizeFileDecision(path)
		base.ChosenOption = fmt.Sprintf("Modify %s", path)
		base.Context = fmt.Sprintf("The agent edited %s", path)
		base.CategoryHint = classifyFileCategory(path, changeContent)
		base.Reason = "This edit touches architecture-sensitive code or configuration."
		base.ImpactScore = 4
		if strings.Contains(strings.ToLower(path), "schema") || strings.Contains(strings.ToLower(changeContent), "migration") {
			base.ImpactScore = 5
			base.Severity = "high"
		}
		return &base
	case "mcp_call":
		server, _ := data["server"].(string)
		tool, _ := data["tool"].(string)
		if tool == "" {
			return nil
		}
		base.Title = "Invoked MCP tooling for a workflow decision"
		base.ChosenOption = fmt.Sprintf("%s/%s", server, tool)
		base.Context = fmt.Sprintf("The agent called MCP server %s tool %s.", server, tool)
		base.CategoryHint = "integration"
		base.Reason = "MCP usage can reflect an integration or workflow tradeoff."
		return &base
	default:
		return nil
	}
}

func looksArchitecturalCommand(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if c == "" {
		return false
	}
	keywords := []string{
		// Package managers (add/remove deps)
		"pnpm add", "pnpm remove", "npm install", "npm uninstall", "yarn add", "yarn remove",
		"pip install", "pip uninstall", "cargo add", "cargo remove", "go mod", "go get",
		"composer require", "gem install", "brew install",
		// Database / migrations
		"drizzle", "db:migrate", "db:generate", "db:push", "migrate", "prisma",
		"knex migrate", "sequelize", "typeorm", "alembic", "django-admin migrate",
		// Infra / CI / deployment
		"docker", "docker-compose", "terraform", "railway", "supabase",
		"vercel", "netlify", "heroku", "kubectl", "helm",
		// Cache / queue / messaging
		"redis", "qstash", "rabbitmq", "kafka",
		// Auth / security
		"clerk", "auth0", "firebase auth",
		// Build system changes
		"turbo", "tsc -b", "make install", "cmake",
		// Git branching
		"git checkout -b", "git branch",
		// Config generation
		"npx create", "yarn create", "pnpm create",
	}
	for _, keyword := range keywords {
		if strings.Contains(c, keyword) {
			return true
		}
	}
	return false
}

func summarizeCommandDecision(command string) string {
	c := strings.TrimSpace(command)
	switch {
	case strings.Contains(c, "db:migrate") || strings.Contains(c, "drizzle"):
		return "Changed the database or migration strategy"
	case strings.Contains(c, "pnpm add") || strings.Contains(c, "npm install") || strings.Contains(c, "go mod"):
		return "Changed project dependencies"
	case strings.Contains(c, "docker") || strings.Contains(c, "railway"):
		return "Changed runtime or deployment configuration"
	default:
		return "Ran a system-shaping command"
	}
}

func classifyCommandCategory(command string) string {
	c := strings.ToLower(command)
	switch {
	case strings.Contains(c, "db:migrate") || strings.Contains(c, "drizzle"):
		return "data-model"
	case strings.Contains(c, "pnpm add") || strings.Contains(c, "npm install") || strings.Contains(c, "go mod"):
		return "dependency"
	case strings.Contains(c, "docker") || strings.Contains(c, "railway"):
		return "deployment"
	default:
		return "workflow"
	}
}

func looksArchitecturalFile(path, diff string) bool {
	p := strings.ToLower(path)
	if p == "" {
		return false
	}

	// Generic architectural file patterns (work across any repo)
	genericPatterns := []string{
		"schema", "migration", "drizzle",
		// Config / build files
		"package.json", "cargo.toml", "go.mod", "go.sum", "requirements.txt",
		"pyproject.toml", "gemfile", "pom.xml", "build.gradle",
		"tsconfig", "webpack", "vite.config", "rollup.config", "babel.config",
		"jest.config", "vitest.config", ".eslintrc", "prettier",
		// CI / deployment
		"dockerfile", "docker-compose", ".github/workflows", ".gitlab-ci",
		"jenkinsfile", "makefile",
		"pnpm-workspace", "turbo.json", "nx.json", "lerna.json",
		// Infrastructure
		"terraform", ".env.example", "vercel.json", "netlify.toml",
		// Architecture docs
		"readme", "architecture", "claude.md", "agents.md", "cursor",
	}
	for _, pattern := range genericPatterns {
		if strings.Contains(p, pattern) {
			return true
		}
	}

	// Generic route/worker/config path patterns (framework-agnostic)
	archPaths := []string{
		"route", "controller", "handler", "middleware",
		"worker", "job", "queue", "cron",
		"plugin", "hook", "adapter",
		"auth", "permission", "policy",
	}
	for _, key := range archPaths {
		if strings.Contains(p, key) {
			return true
		}
	}

	// Generic diff content patterns (aligned with worker HIGH_SIGNAL_KEYWORDS)
	d := strings.ToLower(diff)
	diffKeywords := []string{
		// Auth / security
		"auth", "permission", "credential", "secret", "token", "security",
		// Architecture signals
		"queue", "worker", "middleware", "plugin", "hook",
		"cache", "database", "connection", "endpoint", "boundary",
		"retry", "rate limit", "deployment",
		// Dependency changes
		"import ", "require(", "from '", "from \"",
		// Multi-tenancy / data model
		"orgid", "multi-ten", "tenant", "migration",
	}
	for _, keyword := range diffKeywords {
		if strings.Contains(d, keyword) {
			return true
		}
	}
	return false
}

func summarizeFileDecision(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "schema"):
		return "Changed the data model"
	case strings.Contains(p, "route"):
		return "Changed an API boundary"
	case strings.Contains(p, "worker"):
		return "Changed async job behavior"
	case strings.Contains(p, "hook") || strings.Contains(p, "promptster-cli"):
		return "Changed local hook capture behavior"
	default:
		return fmt.Sprintf("Changed %s", path)
	}
}

func classifyFileCategory(path, diff string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "schema") || strings.Contains(p, "migration"):
		return "data-model"
	case strings.Contains(p, "route") || strings.Contains(p, "api"):
		return "api"
	case strings.Contains(p, "worker") || strings.Contains(strings.ToLower(diff), "queue"):
		return "background-jobs"
	case strings.Contains(p, "hook") || strings.Contains(p, "cursor") || strings.Contains(p, "claude"):
		return "developer-workflow"
	default:
		return "architecture"
	}
}

func shouldSuppressDecisionPrompt(candidate decisionCandidate) bool {
	key := candidate.TriggerKind + "|" + candidate.Title + "|" + candidate.ChosenOption
	lastKey, lastAt := readDecisionPromptState()
	if lastKey != key {
		return false
	}
	return time.Since(lastAt) < decisionPromptCooldown
}

func decisionPromptMode() string {
	if os.Getenv("PROMPTSTER_DISABLE_DECISION_PROMPT") == "1" {
		return decisionPromptModeQueue
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROMPTSTER_DECISION_PROMPT_MODE"))) {
	case "", "queue", "nonblocking", "non-blocking", "async":
		return decisionPromptModeQueue
	case "blocking", "block", "interactive", "prompt":
		return decisionPromptModeBlocking
	case "off", "disabled", "silent":
		return decisionPromptModeOff
	default:
		return decisionPromptModeQueue
	}
}

func hasInteractiveDecisionTTY() bool {
	if decisionPromptMode() == decisionPromptModeOff {
		return false
	}
	tty, err := openDecisionTTY()
	if err != nil {
		return false
	}
	tty.Close()
	return true
}

func maybeCaptureDecisionFromHook(event Event) {
	candidate := detectDecisionCandidate(event)
	if candidate == nil || shouldSuppressDecisionPrompt(*candidate) {
		return
	}

	// Always enqueue — never block the hook, never notify. The queue exists
	// for candidates who WANT to add commentary (`promptster decide` /
	// `promptster explain`); /explain is optional and the CLI never bugs
	// anyone to write an entry.
	if err := enqueueDecisionCandidate(*candidate); err == nil {
		writeDecisionPromptState(candidate.TriggerKind+"|"+candidate.Title+"|"+candidate.ChosenOption, time.Now())
	}
}



func sanitizeImpact(score int) int {
	if score < 1 {
		return 1
	}
	if score > 5 {
		return 5
	}
	return score
}


func persistDecisionCapture(record decisionCaptureRecord) error {
	session, err := loadSession()
	if err != nil {
		return err
	}
	if session.SessionToken == "" {
		return fmt.Errorf("no active session token found")
	}
	event := Event{
		ID:        newUUID(),
		SessionID: record.SessionID,
		Ts:        time.Now().UTC().Format(time.RFC3339Nano),
		Kind:      "decision_event",
		Source:    firstNonEmpty(record.SourceService, "hook"),
		V:         1,
		// /explain rationale is the candidate speaking, whichever channel
		// delivered it.
		Actor:      humanActor(),
		Provenance: humanProvenance(),
		Data: map[string]interface{}{
			"decisionId":           record.DecisionID,
			"title":                record.Title,
			"context":              record.Context,
			"impactScore":          sanitizeImpact(record.ImpactScore),
			"chosenOption":         record.ChosenOption,
			"tradeoffs":            strings.TrimSpace(record.Tradeoffs),
			"rationale":            strings.TrimSpace(record.Rationale),
			"capturedVia":          firstNonEmpty(record.CapturedVia, "manual"),
			"capturedAt":           time.Now().UTC().Format(time.RFC3339Nano),
			"triggerEventId":       record.TriggerEventID,
			"triggerKind":          record.TriggerKind,
			"categoryHint":         record.CategoryHint,
			"instructionVersion":   record.InstructionLabel,
			"rationalePromptedTTY": record.RationalePrompted,
			"severity":             severityFromImpact(record.ImpactScore),
			"description":          record.Title,
			"flagReason":           record.Rationale,
		},
	}
	if err := appendEventToLocalBuffer(&event); err != nil {
		return err
	}
	return ingestEventWithAPIKey(event, session.SessionToken)
}

func ingestEventWithAPIKey(event Event, apiKey string) error {
	return ingestEventWithClient(httpClient, event, apiKey)
}

func ingestEventWithClient(client *http.Client, event Event, apiKey string) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, apiURL()+"/v1/hooks/ingest", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ingest request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ingest failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func severityFromImpact(score int) string {
	switch sanitizeImpact(score) {
	case 5:
		return "high"
	case 4:
		return "medium"
	default:
		return "low"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseImpact(input string, fallback int) int {
	input = strings.TrimSpace(input)
	if input == "" {
		return sanitizeImpact(fallback)
	}
	n, err := strconv.Atoi(input)
	if err != nil {
		return sanitizeImpact(fallback)
	}
	return sanitizeImpact(n)
}
