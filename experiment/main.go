// Command promptster-experiment is the task-envelope harness for the
// practice-effect experiment (openspec change practice-effect-experiment,
// batch-0 task 0.1).
//
// It is deliberately NOT part of the shipped `promptster` binary: that binary
// goes to hiring candidates, and this tool exists only for the internal fleet
// running batch 1. `make build` and the release cross-compile both build the
// root package alone, so nothing here reaches a candidate.
//
// Three things happen here, and nothing else:
//
//  1. `open` bounds a task (design.md: the unit is the task, "open marker ->
//     merge/abandon", never the drift-session), assigns an arm before any work,
//     and appends an immutable assignment row.
//  2. The assigned treatment artifact is shown — C1's contract banner, C2's
//     gate notice, or nothing at all for control.
//  3. The Claude Code hooks enforce C2 and record adherence, which is stored
//     separately from assignment and can never overwrite it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "init":
		os.Exit(cmdInit(args))
	case "open":
		os.Exit(cmdOpen(args))
	case "close":
		os.Exit(cmdClose(args))
	case "status":
		os.Exit(cmdStatus(args))
	case "log":
		os.Exit(cmdLog(args))
	case "sync-payload":
		os.Exit(cmdSyncPayload(args))
	case "sync":
		os.Exit(cmdSync(args))
	case "hook":
		os.Exit(runHook(args))
	case "install-hooks":
		os.Exit(cmdInstallHooks(args))
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `promptster-experiment — task-envelope + assignment harness (batch 0, task 0.1)

  init           --org <id> [--engineer <id>] [--experiment <key>]
  open           --task <key> --class feature|fix|analysis|ops
                 [--size s|m|l] [--title "..."] [--repo owner/name] [--short]
  close          [--task <key>] [--outcome merged|abandoned] [--pr <url>]
  status         show the open envelope and its arm
  log            [--events] [--json]
  sync           [--key PSE-...] [--dry-run] [--only assignments|adherence]
                 post unsynced assignment rows + derived adherence to the backend
  sync-payload   emit POST bodies for /v1/teams/experiments/assignment
  install-hooks  [--write <settings.json>]   (default: print the snippet)
  hook           session-start | pre-compact | user-prompt-submit  (stdin JSON)

State lives in ~/.promptster-experiment (override: PROMPTSTER_EXPERIMENT_DIR).
`)
}

const defaultExperimentKey = "batch1-context-mechanics"

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	org := fs.String("org", "", "org id (required)")
	eng := fs.String("engineer", "", "engineer id (default: git config user.email)")
	experiment := fs.String("experiment", defaultExperimentKey, "registered experiment key")
	key := fs.String("key", "", "engineer key (PSE-...) for `sync`; stored 0600")
	_ = fs.Parse(args)

	if *org == "" {
		fmt.Fprintln(os.Stderr, "error: --org is required")
		return 2
	}
	engineer := *eng
	if engineer == "" {
		engineer = defaultEngineerID()
	}
	cfg := Config{OrgID: *org, EngineerID: engineer, ExperimentKey: *experiment, Enabled: true}
	// Keep an already-stored key when init is re-run to change something else —
	// re-running init should not silently disarm sync.
	if prev, err := loadConfig(); err == nil && prev.EngineerKey != "" {
		cfg.EngineerKey = prev.EngineerKey
	}
	if *key != "" {
		if !strings.HasPrefix(*key, "PSE-") {
			fmt.Fprintln(os.Stderr, "error: --key must be an engineer key (PSE-...)")
			return 2
		}
		cfg.EngineerKey = *key
	}
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("wrote %s\n  org        %s\n  engineer   %s\n  experiment %s\n",
		configPath(), cfg.OrgID, cfg.EngineerID, cfg.ExperimentKey)
	return 0
}

func cmdOpen(args []string) int {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	task := fs.String("task", "", "task key (required, stable, slug-shaped: <repo>/<slug>)")
	class := fs.String("class", "", "feature|fix|analysis|ops (required)")
	size := fs.String("size", "m", "size band s|m|l (analysis covariate)")
	title := fs.String("title", "", "the one artifact this task ships")
	repo := fs.String("repo", "", "repo slug owner/name (default: detected from cwd)")
	short := fs.Bool("short", false, "task is expected to take under 30 minutes (excluded)")
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\nrun `promptster-experiment init --org <id>` first\n", err)
		return 1
	}
	if *task == "" || *class == "" {
		fmt.Fprintln(os.Stderr, "error: --task and --class are required")
		return 2
	}
	// Validate the key HERE, offline, against the backend's contract: a key that
	// works locally but is rejected at sync time strands a task the engineer has
	// already worked under an arm.
	if err := validateTaskKey(*task); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	taskClass, err := validateClass(*class)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	sizeBand, err := validateSize(*size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	cwd, _ := os.Getwd()
	root := repoRootOf(cwd)
	repoSlug := *repo
	if repoSlug == "" {
		repoSlug = repoSlugOf(cwd)
	}

	rows, err := readAssignments()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading assignment log: %v\n", err)
		return 1
	}

	// Re-opening an existing task NEVER re-randomizes. Assignment happens once,
	// at first open, and is immutable — a re-roll is how an experiment quietly
	// becomes self-selection.
	a, existed := findAssignment(rows, cfg, *task)
	if !existed {
		// Only on a FIRST open. Re-opening a task that already drew its arm must
		// never fail: the draw is immutable, so refusing here would block work
		// without protecting anything.
		if err := checkRepoAttribution(*task, repoSlug); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 2
		}
		exclusion := ""
		switch {
		case taskClass == "ops":
			exclusion = "ops_task"
		case *short:
			exclusion = "under_30m"
		}
		env := Envelope{OpenedAt: nowUTC(), RepoRoot: root, Title: truncate(*title, 200)}
		a = newAssignment(cfg, rows, *task, repoSlug, taskClass, sizeBand, env, exclusion, time.Now())
		if err := appendJSONL(assignmentsPath(), a); err != nil {
			fmt.Fprintf(os.Stderr, "error: writing assignment row: %v\n", err)
			return 1
		}
	}

	if err := writeActiveTask(ActiveTask{TaskKey: a.TaskKey, RepoRoot: root, OpenedAt: nowUTC()}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write active-task pointer: %v\n", err)
	}

	event := "task_open"
	if existed {
		event = "task_reopen"
	}
	_ = recordEvent(Event{
		ComplianceEvent: event, OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: a.TaskKey, AssignmentID: a.AssignmentID, Arm: a.Arm,
		ExperimentKey: cfg.ExperimentKey, Source: "cli",
	})

	printArtifact(a)
	return 0
}

// printArtifact shows the assigned treatment artifact — and only the assigned
// one. Control sees no contract text; that is the contrast being measured.
func printArtifact(a Assignment) {
	if !a.Eligible {
		fmt.Print(box("EXPERIMENT · task excluded",
			"Task: "+a.TaskKey+"\nExcluded: "+a.ExclusionCode+"\nRecorded as an exclusion; no arm assigned."))
		return
	}
	switch {
	case a.Factors.C1 && a.Factors.C2:
		fmt.Print(c1Banner(a.TaskKey, a.Envelope.Title))
		fmt.Print(c2Banner(a.TaskKey))
	case a.Factors.C1:
		fmt.Print(c1Banner(a.TaskKey, a.Envelope.Title))
	case a.Factors.C2:
		fmt.Print(c2Banner(a.TaskKey))
	default:
		fmt.Print(controlBanner(a.TaskKey, armLabel(a.Arm)))
	}
	fmt.Printf("  arm %s (%s) · stratum %s · size %s · position %d · %s\n\n",
		armLabel(a.Arm), a.Arm, a.Stratum, a.SizeBand, a.StratumPosition, a.AssignmentID)
}

func cmdClose(args []string) int {
	fs := flag.NewFlagSet("close", flag.ExitOnError)
	task := fs.String("task", "", "task key (default: the open envelope here)")
	outcome := fs.String("outcome", "", "merged|abandoned")
	pr := fs.String("pr", "", "PR url or number (the outcome join point)")
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	cwd, _ := os.Getwd()
	root := repoRootOf(cwd)

	taskKey := *task
	if taskKey == "" {
		active, ok := readActiveTask(root)
		if !ok {
			fmt.Fprintln(os.Stderr, "error: no open task envelope here; pass --task")
			return 2
		}
		taskKey = active.TaskKey
	}
	rows, _ := readAssignments()
	a, ok := findAssignment(rows, cfg, taskKey)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: no assignment row for task %q\n", taskKey)
		return 1
	}
	detail := *outcome
	if *pr != "" {
		detail += " pr=" + *pr
	}
	detail = strings.TrimSpace(detail)
	_ = recordEvent(Event{
		ComplianceEvent: "task_close", OrgID: cfg.OrgID, EngineerID: cfg.EngineerID,
		TaskKey: a.TaskKey, AssignmentID: a.AssignmentID, Arm: a.Arm,
		ExperimentKey: cfg.ExperimentKey, Source: "cli", Detail: detail,
	})
	clearActiveTask(root)
	if detail != "" {
		fmt.Printf("closed %s (arm %s) — %s\n", a.TaskKey, armLabel(a.Arm), detail)
	} else {
		fmt.Printf("closed %s (arm %s)\n", a.TaskKey, armLabel(a.Arm))
	}
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "not configured: %v\n", err)
		return 1
	}
	fmt.Printf("org %s · engineer %s · experiment %s · enabled=%v · spec %s\n",
		cfg.OrgID, cfg.EngineerID, cfg.ExperimentKey, cfg.Enabled, TreatmentSpecVersion)

	cwd, _ := os.Getwd()
	root := repoRootOf(cwd)
	rows, _ := readAssignments()

	if active, ok := readActiveTask(root); !ok {
		fmt.Printf("no open task envelope in %s\n", root)
	} else if a, found := findAssignment(rows, cfg, active.TaskKey); found {
		fmt.Printf("open: %s · arm %s · stratum %s · opened %s\n",
			a.TaskKey, armLabel(a.Arm), a.Stratum, active.OpenedAt)
	} else {
		fmt.Printf("open: %s (no assignment row found)\n", active.TaskKey)
	}

	// Synced-ness comes from the receipt log, never from the row: the row is
	// immutable, so its `synced` field can never become true.
	receipts, _ := readReceipts()
	settled := foldReceipts(receipts).settled

	counts := map[string]int{}
	unsynced := 0
	for _, r := range rows {
		if r.OrgID != cfg.OrgID || r.ExperimentKey != cfg.ExperimentKey {
			continue
		}
		counts[armLabel(r.Arm)]++
		if r.Eligible && !settled["assignment|"+r.TaskKey] {
			unsynced++
		}
	}
	if len(counts) > 0 {
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		fmt.Printf("assignments (%s): %s\n", cfg.ExperimentKey, strings.Join(parts, " "))
		if unsynced > 0 {
			fmt.Printf("%d row(s) not yet synced to the backend assignment log — run `sync`\n", unsynced)
		}
	}
	return 0
}

func cmdLog(args []string) int {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	events := fs.Bool("events", false, "show the adherence event log instead of assignments")
	asJSON := fs.Bool("json", false, "raw jsonl")
	_ = fs.Parse(args)

	path := assignmentsPath()
	if *events {
		path = eventsPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("%s is empty\n", path)
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON || *events {
		os.Stdout.Write(data)
		return 0
	}
	fmt.Printf("%-28s %-8s %-30s %-4s %s\n", "TASK", "ARM", "STRATUM", "POS", "ASSIGNED")
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var a Assignment
		if json.Unmarshal([]byte(line), &a) != nil {
			continue
		}
		fmt.Printf("%-28s %-8s %-30s %-4d %s\n",
			truncate(a.TaskKey, 28), armLabel(a.Arm), truncate(a.Stratum, 30), a.StratumPosition, a.AssignedAt[:19])
	}
	return 0
}

// cmdSyncPayload emits one POST body per unsynced assignment row, for
// POST /v1/teams/experiments/assignment. Kept as a separate command rather than
// an automatic HTTP call: the route does not exist yet (openspec 0.3 in flight),
// and printing the exact bodies makes the offline rows reviewable before any of
// them is committed to the server's log.
func cmdSyncPayload(args []string) int {
	fs := flag.NewFlagSet("sync-payload", flag.ExitOnError)
	all := fs.Bool("all", false, "include rows already synced")
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	rows, err := readAssignments()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	receipts, err := readReceipts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	settled := foldReceipts(receipts).settled

	enc := json.NewEncoder(os.Stdout)
	for _, r := range rows {
		if r.OrgID != cfg.OrgID || r.ExperimentKey != cfg.ExperimentKey {
			continue
		}
		if settled["assignment|"+r.TaskKey] && !*all {
			continue
		}
		if err := enc.Encode(r.syncPayload()); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}
	return 0
}
