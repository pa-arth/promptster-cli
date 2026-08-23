package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	flag "github.com/spf13/pflag"
)

// startVerbose controls whether cmdStart and its helpers emit per-step
// debug output via verbosef. Set from the --verbose flag.
var startVerbose bool

// verbosef prints a dimmed, indented detail line when --verbose is active.
// Used to narrate paths being written, API calls, and other side effects
// that are otherwise invisible when startStep/endStep overwrite one line.
func verbosef(format string, args ...interface{}) {
	if !startVerbose {
		return
	}
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280"))
	line := fmt.Sprintf(format, args...)
	fmt.Printf("        %s\n", dim.Render(line))
}

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	acceptTos := fs.Bool("accept-tos", false, "Skip ToS prompt (for scripted/CI use)")
	workspaceFlag := fs.String("workspace", "", "Workspace path (skip interactive prompt)")
	restart := fs.Bool("restart", false, "Offer to kill running editors so they reload hooks")
	verbose := fs.Bool("verbose", false, "Print every step as it happens (for debugging setup)")
	toolsFlag := fs.String("tools", "", "AI tool(s) to instrument: claude|codex|all or a comma list (skips the interactive prompt)")
	// RETIRED, and still ACCEPTED rather than removed.
	//
	// The hiring team supplies model access now, so there is nothing for this to
	// turn on. But flag.ExitOnError means an unrecognised flag kills the process,
	// and the candidate who passes this is following an instruction someone gave
	// them — a hard exit at the start of a timed assessment is the worst possible
	// way to deliver news that is, for them, good. So it parses, it explains, and
	// startup continues on the employer's key.
	byoFlag := fs.Bool("byo-subscription", false, "Retired — assessments run on model access the hiring team supplies. Accepted and ignored.")
	// Cursor named here is the EDITOR, and that is the one Cursor claim this
	// change leaves standing: the extension installs into whatever editor the
	// candidate uses. What is retired is Cursor as an instrumented AGENT.
	noEditorExt := fs.Bool("no-editor-extension", false, "Skip installing the Promptster editor extension into VS Code/Cursor (the session records that editor attention capture was unavailable)")
	// --adopt: work in the checkout that is already here instead of cloning one.
	//
	// It DEFAULTS ON inside a Codespace (openspec §2.2). That default is the
	// point: the candidate's typed command has to be identical on both lanes —
	// `promptster start PST-XXXX-XXXX` — because the hosted lane exists to delete
	// setup steps, and "on this lane, add a flag" is a setup step. `--adopt=false`
	// still forces the clone path for anyone who needs it.
	adoptFlag := fs.Bool("adopt", false, "Adopt the checkout already present instead of cloning (default: on inside a GitHub Codespace)")
	fs.Parse(args) //nolint:errcheck
	startVerbose = *verbose

	adopt := *adoptFlag
	if !fs.Changed("adopt") {
		adopt = inCodespace()
	}
	// hostedLane is deliberately NOT `adopt && inCodespace()`. The fork hazard
	// (design.md §2) and the wrong-instructions hazard (§3.2) are properties of
	// being in a read-only codespace, not of how the workspace was resolved — so
	// `--adopt=false` inside a codespace must still suppress the commit and the
	// clone-and-checkout text. Tying the copy to the flag would let one override
	// silently switch off a privacy mechanism.
	hostedLane := inCodespace()
	if hostedLane {
		verbosef("hosted lane detected: CODESPACE_NAME=%s", codespaceName())
	}

	// Check for CLI updates (non-blocking, best-effort)
	checkForUpdate()

	// Preflight: check for required tools before doing anything else
	fmt.Println()
	verbosef("api=%s version=%s platform=%s/%s", apiURL(), version, runtime.GOOS, runtime.GOARCH)
	preflightChecks()
	if !hasRequiredTools() {
		os.Exit(1)
	}

	var session Session
	var taskBrief string
	var timeLimitMinutes int
	skipConsent := false
	taskRootDisplay := ""

	redeemKey := func(key string) {
		keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
		fmt.Printf("  Redeeming %s...\n", keyStyle.Render(key))
		var redeemArgs []string
		if *acceptTos {
			redeemArgs = append(redeemArgs, "--accept-tos")
		}
		redeemArgs = append(redeemArgs, key)
		cmdRedeem(redeemArgs)
		fmt.Println()
	}

	// If a key is passed as a positional arg, auto-redeem first
	if remaining := fs.Args(); len(remaining) > 0 && strings.HasPrefix(strings.ToUpper(remaining[0]), "PST-") {
		redeemKey(remaining[0])
	}

	startStepSection("Setup")

	startStep(1, 7, "Loading saved session...")
	verbosef("session file: %s", sessionPath())
	saved, err := loadSession()
	if err != nil {
		// No saved session and no key on the command line. ASK for it rather than
		// exiting (openspec §2.7): the hosted lane launches this from
		// postAttachCommand, so a hard exit here would greet the candidate with an
		// error telling them to run the command that just ran. Their only action
		// is a paste, and this is where the paste goes.
		//
		// Non-interactive callers keep the old exact behaviour — promptForAssessmentKey
		// returns "" when stdin is not a terminal, so scripts still fail fast.
		if key := promptForAssessmentKey(); key != "" {
			fmt.Println()
			redeemKey(key)
			startStep(1, 7, "Loading saved session...")
			saved, err = loadSession()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: no session found — run: promptster start PST-XXXX-XXXX")
		os.Exit(1)
	}
	verbosef("sessionId=%s assessmentId=%s", saved.SessionID, saved.AssessmentID)
	endStep(1, 7, "Loading saved session", "")
	session = saved
	// Recorded from the ENVIRONMENT, not from the assessment's hostingLane: the
	// assessment carries the recruiter's OFFER, and a candidate offered the
	// hosted lane can still clone locally. This is what keeps `done`, `abort` and
	// `doctor` on the right lane in a shell that never inherited the codespace env.
	session.HostedLane = hostedLane
	taskBrief = saved.TaskBrief
	timeLimitMinutes = saved.TimeLimitMinutes
	skipConsent = saved.ConsentAccepted

	// [2/7] Terms of Service ─────────────────────────────────────────────────
	if skipConsent {
		startStep(2, 7, "Terms of Service...")
		endStep(2, 7, "Terms of Service", "(already accepted)")
	} else {
		startStepBlock(2, 7, "Terms of Service...")
		// Rare path: the saved session predates consent (ConsentAccepted=false) —
		// redeem normally persists it true, so a normal start skips this block. We
		// deliberately do NOT re-confirm consent server-side here: the key was
		// already redeemed, and redemption is gated on consentConfirmedAt (a 403
		// otherwise), so consent is guaranteed on file by this point. ConsentAccepted
		// is persisted true in step 6 below, so this won't recur on restart.
		//
		// Best-effort fetch of consent state + canonical disclosure; zero value
		// falls back to the embedded disclosure and unconfirmed consent.
		info, _ := apiConsentInfo(session.Key)
		result := runConsent(info.Disclosure, info.AlreadyConfirmed, *acceptTos)
		if !result.Accepted {
			fmt.Println("\nAssessment declined. No data has been recorded.")
			os.Exit(0)
		}
		session.ConsentToIntegrity = result.IntegrityAccepted
		endStep(2, 7, "Terms of Service", result.Note)
	}

	// [3/7] Prepare workspace (select path + clone/init if needed) ───────────
	// Workspace selection is interactive when stdin is a terminal and no
	// --workspace flag was given; otherwise a sensible default is used silently.
	isInteractive := stdinIsTerminal() && *workspaceFlag == "" && !adopt
	if !isInteractive {
		startStep(3, 7, "Preparing workspace...")
	} else {
		startStepBlock(3, 7, "Preparing workspace...")
	}

	var chosenPath string
	if adopt {
		// ADOPT: the tree is already here. prepareWorkspaceCheckout is skipped
		// ENTIRELY — not pointed elsewhere, not made conditional inside — because
		// running it in a codespace materialises a second tree beside the mirror
		// the container was built from, and TaskRoot follows the new one while the
		// candidate keeps working in the old one.
		adopted, adoptErr := resolveAdoptWorkspace(*workspaceFlag)
		if adoptErr != nil {
			endStepWarn(3, 7, "Preparing workspace", adoptErr.Error())
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "error: --adopt could not find a checkout to adopt.")
			fmt.Fprintln(os.Stderr, "  Run promptster start from inside the assessment folder, or pass --workspace PATH.")
			os.Exit(1)
		}
		chosenPath = adopted
		verbosef("adopted checkout: %s", chosenPath)

		actualTree, treeErr := readTreeSha(chosenPath)
		outcome := evaluateAdoptedTree(session.ExpectedTreeSha, actualTree, treeErr)
		verbosef("tree verification: state=%s expected=%s actual=%s", outcome.State, outcome.Expected, outcome.Actual)
		if outcome.fatal() {
			endStepWarn(3, 7, "Preparing workspace", "tree check failed ("+outcome.State+")")
			printAdoptFailure(outcome, chosenPath)
			os.Exit(1)
		}
		session.TreeVerification = outcome.State
		// The subdirectory narrows the task boundary inside a monorepo, and that
		// is a property of the ASSESSMENT, not of how the checkout got here. The
		// clone path below applies it; adopting the repo root instead would put
		// snapshots, capture, the diff and the bundle over the whole repository.
		// Tree verification and DiffBaseCommit above deliberately stay on the repo
		// root — they are repo-level facts.
		adoptedTaskRoot := taskRootWithinRepo(chosenPath, session.RepoSubdir)
		session.TaskRoot = adoptedTaskRoot
		taskRootDisplay = adoptedTaskRoot
		if shaOut, err := exec.Command("git", "-C", chosenPath, "rev-parse", "--short", "HEAD").Output(); err == nil {
			session.WorkspaceCommit = strings.TrimSpace(string(shaOut))
		}
		// The adopted checkout's own HEAD is the diff base. session.RepoCommit is
		// the UPSTREAM brokenSha and names no object in a mirror clone, so leaving
		// it as the base makes every diff and every bundle scrub fail — silently,
		// into an empty submission. See Session.DiffBaseCommit.
		if fullSha, err := exec.Command("git", "-C", chosenPath, "rev-parse", "HEAD").Output(); err == nil {
			session.DiffBaseCommit = strings.TrimSpace(string(fullSha))
		}
		adoptedDisplay := chosenPath
		if adoptedTaskRoot != chosenPath {
			adoptedDisplay = fmt.Sprintf("%s (task root: %s)", chosenPath, adoptedTaskRoot)
		}
		note := adoptedDisplay + " (adopted, tree verified)"
		if outcome.State == hostedTreeUnverified {
			note = adoptedDisplay + " (adopted, tree NOT verified)"
		}
		endStep(3, 7, "Preparing workspace", note)
		if outcome.State == hostedTreeUnverified {
			printAdoptUnverified(outcome)
		}
	} else {
		chosenPath = resolveWorkspacePath(*workspaceFlag, session)
	}
	verbosef("workspace path: %s", chosenPath)
	if session.RepoURL != "" {
		verbosef("repo: %s", session.RepoURL)
		if session.RepoCommit != "" {
			verbosef("pinned commit: %s", session.RepoCommit)
		}
		if session.RepoSubdir != "" {
			verbosef("repo subdir: %s", session.RepoSubdir)
		}
	}

	if adopt {
		// Nothing to do: the adopt branch above already resolved TaskRoot from the
		// checkout that was here. This arm exists so the two paths are visibly
		// exclusive rather than exclusive by accident.
	} else if session.RepoURL == "" {
		// No repo to clone — just ensure the directory exists and set TaskRoot.
		if err := os.MkdirAll(chosenPath, 0o755); err != nil {
			endStepWarn(3, 7, "Preparing workspace", fmt.Sprintf("could not create directory: %v", err))
		} else {
			abs, _ := filepath.Abs(chosenPath)
			session.TaskRoot = abs
			taskRootDisplay = abs
			endStep(3, 7, "Preparing workspace", abs)
		}
	} else {
		workspacePath := chosenPath
		taskRootPath := workspacePath

		cloneErr := prepareWorkspaceCheckout(workspacePath, session.RepoURL, session.RepoCommit)

		if cloneErr != nil {
			msg := strings.TrimSpace(cloneErr.Error())
			if msg == "" {
				msg = "failed to prepare workspace"
			}
			endStepWarn(3, 7, "Preparing workspace", msg)
		} else {
			taskRootPath = taskRootWithinRepo(workspacePath, session.RepoSubdir)

			if shaOut, err := exec.Command("git", "-C", workspacePath, "rev-parse", "--short", "HEAD").Output(); err == nil {
				session.WorkspaceCommit = strings.TrimSpace(string(shaOut))
			}

			taskRootAbs, _ := filepath.Abs(taskRootPath)
			session.TaskRoot = taskRootAbs
			taskRootDisplay = taskRootAbs
			workspaceDisplay := taskRootAbs
			if session.RepoSubdir != "" {
				rootAbs, _ := filepath.Abs(workspacePath)
				workspaceDisplay = fmt.Sprintf("%s (task root: %s)", rootAbs, taskRootAbs)
			}
			if session.WorkspaceCommit != "" {
				endStep(3, 7, "Preparing workspace", workspaceDisplay+" @ "+session.WorkspaceCommit)
			} else {
				endStep(3, 7, "Preparing workspace", workspaceDisplay)
			}
		}
	}
	if session.WorkspaceCommit != "" && session.SessionID != "" && session.SessionToken != "" {
		if err := apiSaveWorkspaceCommit(session.SessionID, session.SessionToken, session.WorkspaceCommit); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save workspace commit to server: %v\n", err)
		}
	}

	// Write workspace pointer so hooks and state files resolve to <workspace>/.promptster/
	workspaceForState := chosenPath
	if session.TaskRoot != "" {
		workspaceForState = session.TaskRoot
	}
	if err := writeActiveWorkspace(workspaceForState); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write active workspace pointer: %v\n", err)
	}

	// Migrate session.json from global dir to workspace-local dir
	wsStateDir := filepath.Join(workspaceForState, ".promptster")
	globalSessionPath := filepath.Join(globalPromptsterDir(), "session.json")
	localSessionPath := filepath.Join(wsStateDir, "session.json")
	if globalSessionPath != localSessionPath {
		if err := os.MkdirAll(wsStateDir, 0o700); err == nil {
			if data, err := os.ReadFile(globalSessionPath); err == nil {
				if os.WriteFile(localSessionPath, data, 0o600) == nil {
					_ = os.Remove(globalSessionPath)
				}
			}
			// Also migrate the per-session signing key if it was written into
			// ~/.promptster during redeem (the workspace pointer wasn't known yet).
			globalKeyPath := filepath.Join(globalPromptsterDir(), "session.key")
			localKeyPath := filepath.Join(wsStateDir, "session.key")
			if keyData, err := os.ReadFile(globalKeyPath); err == nil {
				if os.WriteFile(localKeyPath, keyData, 0o600) == nil {
					_ = os.Remove(globalKeyPath)
				}
			}
		}
	}

	// Capture git author identity for commit attribution
	if name, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		session.GitAuthorName = strings.TrimSpace(string(name))
	}
	if email, err := exec.Command("git", "config", "user.email").Output(); err == nil {
		session.GitAuthorEmail = strings.TrimSpace(string(email))
	}

	// Write TASK.md so the candidate (and Claude Code) can reference the brief
	writeTaskFile(workspaceForState, session, taskBrief, timeLimitMinutes)

	// Ensure .promptster is gitignored in the workspace
	gitignorePath := filepath.Join(workspaceForState, ".gitignore")
	if data, err := os.ReadFile(gitignorePath); err != nil || !strings.Contains(string(data), ".promptster") {
		f, err := os.OpenFile(gitignorePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
				f.WriteString("\n") //nolint:errcheck
			}
			f.WriteString(".promptster/\n") //nolint:errcheck
			f.Close()                       //nolint:errcheck
		}
	}

	// Honeypot: drop a marker file inside .promptster/ when the candidate has
	// opted in to integrity checks. The token is random per-session; if it ever
	// appears in a prompt or command output, the backend flags a cross-session
	// paste (worth investigating, not an automatic fail).
	if session.ConsentToIntegrity {
		if session.HoneypotToken == "" {
			session.HoneypotToken = newHoneypotToken()
		}
		if session.HoneypotToken != "" {
			trapPath := filepath.Join(wsStateDir, "session-notes.md")
			content := fmt.Sprintf(
				"# Promptster session notes\n\n"+
					"Internal marker: %s\n\n"+
					"This file is gitignored and private to your local session. "+
					"Do not paste its contents into any other tool.\n",
				session.HoneypotToken,
			)
			_ = os.WriteFile(trapPath, []byte(content), 0o600)
		}
	}

	// [4/7] Configure hooks ───────────────────────────────────────────────────
	// Project path is now known, so Claude/Cursor project-level hooks can be
	// written into the active task root for this assessment.
	verbosef("installing promptster binary to %s", promptsterBin())
	if err := installSelfToBin(); err != nil {
		// Non-fatal: log a warning but continue; hooks may still work via PATH.
		fmt.Fprintf(os.Stderr, "warning: could not install binary to ~/.promptster/bin: %v\n", err)
	}
	hookRoot := chosenPath
	if session.TaskRoot != "" {
		hookRoot = session.TaskRoot
	}

	// Resolve which AI tool(s) to instrument. Claude Code is hooked into the
	// workspace settings; Codex is captured by tailing its rollout JSONL via a
	// background watcher (codex's hooks don't fire for `codex exec`).
	tools := selectTools(*toolsFlag, session.AllowedTools)
	if len(tools) == 0 {
		// selectTools already printed which tools are allowed; bail out.
		os.Exit(1)
	}
	// Narrow to the tools actually installed (early preflight only gated git, so
	// a Codex/Cursor-only assessment isn't blocked by a missing `claude`). A
	// missing tool is dropped with a warning; if NONE are installed the candidate
	// has no working AI tool and resolveUsableTools exits.
	tools = resolveUsableTools(tools)
	session.Tools = tools
	// Advise (don't block) if the assessment's run/test commands need a toolchain
	// binary that isn't installed — environment fights measure nothing.
	warnMissingProjectTools(resolveBrief(session))
	useClaude := hasTool(tools, toolClaude)
	useCodex := hasTool(tools, toolCodex)
	verbosef("instrumenting tools: %s", toolsLabel(tools))

	// Candidate-supplied model access is retired: the hiring team supplies the
	// key, it is metered at the proxy, and the candidate's own paid subscription
	// is never spent on being evaluated. See openspec
	// changes/employer-supplied-model-key.
	//
	// Transcript capture is NOT retired with it. It moved from being BYO's only
	// channel to being the fallback rail for everyone: it is armed below when the
	// proxy smoke test fails, because that is exactly the condition it exists to
	// cover — proxy traffic unavailable, so nothing would be captured at all.
	noteLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8")).Bold(true)
	noteText := lipgloss.NewStyle().Foreground(cBody)
	if *byoFlag {
		fmt.Println()
		fmt.Printf("  %s %s\n", noteLabel.Render("Note:"),
			noteText.Render("--byo-subscription is retired. This assessment runs on model access the hiring team supplies — your own Claude/OpenAI subscription is not used and not billed."))
	}

	// Claude Code and Codex share ONE proxy-backed AI budget, so using both
	// doesn't double it.
	if useClaude && useCodex {
		fmt.Println()
		fmt.Printf("  %s %s\n", noteLabel.Render("Note:"),
			noteText.Render("Claude Code and Codex draw from ONE shared AI budget — using both doesn't double it. Pick whichever you prefer."))
	}

	startStep(4, 7, "Configuring hooks...")
	var hookErr error
	if useClaude {
		verbosef("writing Claude hooks to %s", claudeProjectSettingsPath(hookRoot))
		verbosef("hook events: %s", strings.Join(claudeHookPointNames, ", "))
		hookErr = configureHooks(hookRoot)
	}
	// Cursor's hooks used to be written here. Cursor forces Agent/Edit model
	// traffic through its own backend, so a key the hiring team supplies can be
	// neither used nor metered on it — which made Cursor candidate-pays by
	// construction, the one thing this product no longer does. It is not an
	// instrumented tool any more. Candidates may still work in Cursor as their
	// editor; the assessment is instrumented through Claude Code or Codex.
	// See openspec changes/employer-supplied-model-key.
	if hookErr != nil {
		endStepWarn(4, 7, "Configuring hooks", hookErr.Error())
	} else {
		endStep(4, 7, "Configuring hooks", toolsLabel(tools))
	}

	// Install shell hook. As of 1.2.0 the hook only captures terminal commands
	// and fires TTL self-eviction — the Anthropic proxy is wired through Claude
	// Code's apiKeyHelper (configureProxyEnv below), not via shell env, so no
	// token-bearing block is ever written into this 0644 file.
	verbosef("writing shell hook to %s", shellHookPath())
	for _, rc := range shellRCPathsForInstall() {
		verbosef("will inject source line into %s", rc)
	}
	// On the hosted lane the image's own shell init sources the hook, so writing
	// it into ~/.bashrc as well would double it. Skip the injection only when the
	// system line is actually there — see systemShellInitSourcesHook.
	//
	// The failure is non-fatal on BOTH lanes and always was: `start` warns and
	// carries on. Worth stating rather than changing, because a hosted box has a
	// read-only-ish home in some configurations and a fatal shell-hook failure
	// there would kill an assessment over terminal capture, which is one signal
	// among many.
	injectRC := true
	if hostedLane && systemShellInitSourcesHook() {
		injectRC = false
		verbosef("hosted lane: system shell init already sources the hook — skipping RC injection")
	}
	_, shellErr := installShellHookWithRC(injectRC)
	if shellErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install shell hook: %v\n", shellErr)
	}

	// Configure API proxy — route Claude Code API calls through Promptster so
	// we can capture full conversations and supply the credential. Writes
	// ANTHROPIC_BASE_URL + an apiKeyHelper into the workspace settings.
	// (Codex BYOK proxy wiring is handled separately; see codex_proxy.go.)
	proxyURL := apiURL() + "/v1/proxy/anthropic"
	if useClaude {
		verbosef("setting ANTHROPIC_BASE_URL=%s + apiKeyHelper in %s", proxyURL, claudeProjectSettingsPath(hookRoot))
		configureProxyEnv(hookRoot, proxyURL, session.SessionToken)
	}

	// Codex BYOK proxy — route codex's Responses-API traffic through Promptster
	// for capture + metering. codex config is GLOBAL (no workspace scoping, no
	// apiKeyHelper), so we write a marker-fenced provider block into
	// ~/.codex/config.toml and select it via model_provider; configureCodexProxy
	// is idempotent and records prior state for an exact revert on done/abort.
	// The credential is NOT written to the file — it rides PROMPTSTER_PROXY_TOKEN,
	// exported PWD-gated by the shell hook from the 0600 session.json.
	codexProxyURL := apiURL() + "/v1/proxy/openai/v1"
	if useCodex {
		verbosef("configuring codex model_provider=%s base_url=%s in %s", codexProxyProviderID, codexProxyURL, codexConfigPath())
		if err := configureCodexProxy(codexProxyURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not configure Codex proxy: %v\n", err)
		}
	}

	// No subscription-logout step: the apiKeyHelper out-ranks a logged-in
	// subscription OAuth in Claude Code's credential precedence, so the proxy
	// wins regardless of whether the candidate is signed into Max/Pro. Only a
	// shell-exported ANTHROPIC_AUTH_TOKEN/API_KEY out-ranks the helper — that
	// case is flagged by `promptster doctor` (the smoke test below only checks
	// proxy reachability + token validity via a direct HTTP call, not Claude
	// Code's runtime auth resolution, so it cannot see a shell-exported token).

	// [5/7] Smoke-test every proxy this session will actually use, so auth,
	// network, and key-resolution failures surface here rather than on the
	// candidate's first prompt.
	//
	// EVERY INSTRUMENTED TOOL GETS CHECKED, not just Claude Code. Codex used to
	// print "skipped (no Claude Code in this session)" here and go unverified,
	// which inverted the point of the step — codex has no other preflight, so a
	// codex-only session was the one flying blind. It also kept a broken key
	// invisible: the proxy bumps org_openai_keys.auth_failure_count only when a
	// request reaches it, so a rejected hiring-team key read as healthy in the
	// dashboard until a candidate hit it mid-assessment. See codex_auth_check.go.
	//
	// A CLAUDE FAILURE HERE ARMS TRANSCRIPT CAPTURE, and that is the whole reason
	// transcript capture survives the retirement of BYO.
	//
	// It used to be BYO's private channel — the only way to see a session that
	// deliberately bypassed the proxy. With BYO gone, the condition it answers is
	// the accidental version of the same thing: the proxy is unreachable, or the
	// token is missing, so proxy traffic will not be captured and the reviewer
	// would get a session with nothing in it. Reading the transcript is strictly
	// better than reading nothing, and it carries the per-request token usage
	// too, so cost degrades to estimated rather than to absent.
	//
	// A CODEX FAILURE DOES NOT, because codex capture never rode the proxy in the
	// first place — the rollout-JSONL watcher records the session either way. The
	// thing that breaks is the candidate's model access, not our visibility.
	//
	// Set BEFORE saveSession below, so the watcher started later in this run and
	// the hook suppression in cmd_hook.go both see it.
	stepLabel := "Testing Claude API proxy"
	switch {
	case useClaude && useCodex:
		stepLabel = "Testing model proxies"
	case useCodex:
		stepLabel = "Testing Codex API proxy"
	}
	startStep(5, 7, stepLabel+"...")

	if session.SessionToken == "" {
		endStepWarn(5, 7, stepLabel, "no session token")
		// Only Claude capture rides the proxy. Codex is captured by tailing its
		// rollout JSONL, so it needs no fallback rail.
		if useClaude {
			session.CaptureMode = "transcript"
		}
	} else {
		var notes []string
		failed := false
		if useClaude {
			if smokeErr := smokeTestProxy(proxyURL, session.SessionToken); smokeErr != nil {
				failed = true
				notes = append(notes, "Claude: "+smokeErr.Error())
				printProxySmokeTestFailure(smokeErr)
				session.CaptureMode = "transcript"
			} else {
				notes = append(notes, "Claude ready")
			}
		}
		if useCodex {
			if smokeErr := smokeTestCodexProxy(codexProxyURL, session.SessionToken); smokeErr != nil {
				failed = true
				notes = append(notes, "Codex: "+smokeErr.Error())
				printCodexProxySmokeTestFailure(smokeErr)
				// Deliberately NOT arming transcript capture: codex capture does
				// not ride the proxy, so the session is still recorded. What is
				// broken here is the candidate's model access, not our visibility.
			} else {
				notes = append(notes, "Codex ready")
			}
		}
		if failed {
			endStepWarn(5, 7, stepLabel, strings.Join(notes, "; "))
		} else {
			endStep(5, 7, stepLabel, "ready")
		}
	}
	if session.CaptureMode == "transcript" {
		verbosef("proxy unavailable: arming transcript capture as the fallback rail")
	}

	// Detect running editors and prompt for restart if needed ─────────────────
	// Hooks live in <workspace>/.<tool>/ (e.g. .claude/settings.local.json), so a
	// tool window that was already open (anywhere) won't have them. Make that
	// loud — candidates previously assumed their existing session was being
	// captured and only found out later that nothing was recorded.
	running := runningEditors(tools)
	if len(running) > 0 {
		nouns := make([]string, len(running))
		for i, t := range running {
			nouns[i] = toolBaseName(t)
		}
		runningLabel := strings.Join(nouns, " and ")
		verb := "is"
		if len(nouns) > 1 {
			verb = "are"
		}
		warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f59e0b")).Bold(true)
		warnText := lipgloss.NewStyle().Foreground(cWarnText)
		fmt.Println()
		fmt.Printf("  %s %s\n", warnStyle.Render("!"), warnStyle.Render(runningLabel+" "+verb+" already running"))
		fmt.Printf("    %s\n", warnText.Render("Existing "+runningLabel+" windows will NOT capture this session."))
		fmt.Printf("    %s\n", warnText.Render("You must open "+runningLabel+" fresh from the workspace below."))
		if *restart {
			doRestart := !stdinIsTerminal() // auto-kill when non-interactive
			if stdinIsTerminal() {
				fmt.Printf("\n  Restart %s now? [y/N]: ", runningLabel)
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
					doRestart = ans == "y" || ans == "yes"
				}
			}
			if doRestart {
				killEditors(running)
				fmt.Printf("  Editors stopped. Reopen %s in your workspace to continue.\n", runningLabel)
			}
		}
	}

	// Editor attention capture ────────────────────────────────────────────────
	// Installing the extension that records which files the CANDIDATE opened, as
	// opposed to which files the agent read. Deliberately here, beside the editor
	// detection above, and deliberately NOT a numbered step: it is best-effort.
	//
	// Non-fatal, without exception. A candidate working in vim, on a locked-down
	// machine, or behind an editor CLI that will not cooperate finishes the
	// assessment exactly as before. What changes is that the session then says
	// capture was unavailable, so a reviewer never reads that silence as a
	// candidate who opened no files.
	editorCapture := installEditorExtension(!*noEditorExt)
	printEditorCaptureLine(editorCapture)

	// [6/7] Save session + verify setup ───────────────────────────────────────
	startStep(6, 7, "Verifying setup...")
	session.StartedAt = time.Now().UTC()
	session.ConsentAccepted = true
	session.ApiURL = apiURL()
	verbosef("saving session to %s", sessionPath())
	if err := saveSession(session); err != nil {
		fmt.Printf("\n")
		fmt.Fprintf(os.Stderr, "error: failed to save session: %v\n", err)
		os.Exit(1)
	}
	endStep(6, 7, "Verifying setup", "")

	// Record editor-capture availability on the session. Best-effort delivery,
	// but not optional information: absence-of-capture and absence-of-behaviour
	// are different states and only one of them is about the candidate.
	recordEditorCapture(&session, editorCapture)

	// Device continuity check — fire-and-forget
	if session.SessionToken != "" {
		verbosef("sending device check to %s/v1/candidate/device-check", apiURL())
		fp := collectDeviceFingerprint()
		if err := apiDeviceCheck(session.SessionToken, DeviceCheckRequest{
			Checkpoint:  "start",
			Fingerprint: fp,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: device check failed: %v\n", err)
		}
	}

	startStep(7, 7, "Enabling optional /explain...")
	initNudgeState()
	endStep(7, 7, "Optional /explain ready", "commentary on your decisions, only if you want")

	// Start background git diff watcher for capturing manual edits
	ensureGitWatcher()

	// Start background codex rollout watcher to capture codex CLI activity.
	if useCodex {
		verbosef("starting codex rollout watcher (sessions dir: %s)", codexSessionsDir())
		ensureCodexWatcher()
	}

	// Transcript-capture mode: start the Claude Code transcript watcher. It
	// owns prompt/response/tool capture (including per-request token usage —
	// the basis for estimated cost in BYO mode); hooks fall back when it dies.
	if useClaude && session.CaptureMode == "transcript" {
		verbosef("starting claude transcript watcher (projects dir: %s)", claudeProjectsDir())
		ensureClaudeWatcher()
	}

	// Task brief display ──────────────────────────────────────────────────────
	fmt.Println()
	printTaskBrief(session.AssessmentTitle, taskBrief, timeLimitMinutes)
	fmt.Println()

	infoLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	infoText := lipgloss.NewStyle().Foreground(cBody)
	dimText := lipgloss.NewStyle().Foreground(cMuted)
	codeText := lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8"))

	if taskRootDisplay == "" && session.TaskRoot != "" {
		taskRootDisplay = session.TaskRoot
	}
	if taskRootDisplay != "" {
		fmt.Printf("  %s %s\n", infoLabel.Render("Workspace:"), codeText.Render(taskRootDisplay))
		fmt.Println()
	}
	// setupInstructions is DISPLAY-ONLY on every lane — the CLI never executes it,
	// it just prints it — which is exactly why it is dangerous here. On the hosted
	// lane the stored text tells the candidate to clone and check out, and a
	// candidate who follows it lands in a second checkout that `done` does not
	// bundle. Backend #789 already serves lane-aware text (§3.2); this is the
	// defense in depth behind it, and it is reliable in a way it would not be on
	// the local lane because the hosted image bakes this binary in — no version skew.
	if strings.TrimSpace(session.SetupInstructions) != "" && !hostedLane {
		fmt.Printf("  %s\n", infoLabel.Render("Setup / How to run:"))
		for _, line := range strings.Split(session.SetupInstructions, "\n") {
			fmt.Printf("  %s\n", infoText.Render(line))
		}
		fmt.Println()
	}
	if hostedLane {
		fmt.Printf("  %s\n", infoLabel.Render("Your environment:"))
		for _, line := range hostedBriefLines() {
			fmt.Printf("  %s\n", infoText.Render(line))
		}
		fmt.Println()
	}

	// Next steps — explicit, numbered, copy-pasteable. The previous "Open
	// Claude Code and start working" line left candidates guessing; several
	// opened Claude Code against a different repo and never had any events
	// captured. Spell out the exact commands and why they matter.
	stepNumStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	fmt.Printf("  %s\n", infoLabel.Render("Next steps:"))
	if taskRootDisplay != "" {
		fmt.Printf("    %s %s\n", stepNumStyle.Render("1."), codeText.Render(fmt.Sprintf("cd %s", taskRootDisplay)))
	} else {
		fmt.Printf("    %s %s\n", stepNumStyle.Render("1."), infoText.Render("cd into your workspace (shown above)"))
	}
	// The launch command + reminder copy adapt to which tool(s) were chosen.
	launchCmd, launchDesc, toolNoun := nextStepLaunch(useClaude, useCodex)
	fmt.Printf("    %s %s  %s\n",
		stepNumStyle.Render("2."),
		codeText.Render(launchCmd),
		dimText.Render(launchDesc))
	fmt.Println()

	// Boxed reminder — the single most common candidate mistake is opening
	// Claude Code in a different directory (home, last project) and assuming
	// Promptster is capturing it. Hooks live in <workspace>/.claude/, so a
	// session started anywhere else writes zero events. Make this impossible
	// to skim past.
	warnBorderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#f59e0b")).
		Padding(0, 2).
		Width(70)
	warnHeading := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#f59e0b")).
		Bold(true)
	warnBody := lipgloss.NewStyle().
		Foreground(cWarnText)
	var warnContent strings.Builder
	if hostedLane {
		// The local-lane warning tells the candidate not to open their editor
		// somewhere else. In a codespace there IS nowhere else — the terminal they
		// are reading this in is already in the workspace — and the mistake that
		// actually happens here is the opposite one: making a second checkout.
		warnContent.WriteString(warnHeading.Render("⚠  Work here, in this codespace"))
		warnContent.WriteString("\n")
		warnContent.WriteString(warnBody.Render(strings.Join(wordWrap(
			"Do not clone the repository again. Promptster submits the working tree "+
				"at the path above; anything you write in a second copy will not be "+
				"captured and will not be submitted.",
			64), "\n")))
	} else {
		warnContent.WriteString(warnHeading.Render("⚠  Open " + toolNoun + " from this workspace"))
		warnContent.WriteString("\n")
		warnContent.WriteString(warnBody.Render(strings.Join(wordWrap(
			"Promptster only captures sessions started from the directory above. "+
				"A session opened anywhere else will not be recorded.",
			64), "\n")))
	}
	fmt.Println(warnBorderStyle.Render(warnContent.String()))
	fmt.Println()

	// Helpful commands reference
	hintLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	fmt.Printf("  %s\n", hintLabel.Render("Helpful commands:"))
	fmt.Printf("  %s  %s\n", codeText.Render("promptster brief"), dimText.Render("— review the task brief and time remaining"))
	fmt.Printf("  %s %s\n", codeText.Render("promptster explain"), dimText.Render("— document your decision rationale"))
	fmt.Printf("  %s   %s\n", codeText.Render("promptster done"), dimText.Render("— submit your assessment when finished"))
	fmt.Println()
	fmt.Printf("  %s\n", dimText.Render("The task brief is also saved in TASK.md in your workspace."))
	fmt.Println()
}

// nextStepLaunch returns the launch command, its description, and the tool
// noun used in the "open from this workspace" reminder, adapting to whichever
// tool(s) the candidate selected.
func nextStepLaunch(useClaude, useCodex bool) (cmd, desc, noun string) {
	type launch struct{ cmd, noun string }
	var sel []launch
	if useClaude {
		sel = append(sel, launch{"claude", "Claude Code"})
	}
	if useCodex {
		sel = append(sel, launch{"codex", "Codex"})
	}
	if len(sel) == 0 {
		return "claude", "— opens Claude Code in this workspace", "Claude Code"
	}
	if len(sel) == 1 {
		return sel[0].cmd, "— opens " + sel[0].noun + " in this workspace", sel[0].noun
	}
	cmds := make([]string, len(sel))
	nouns := make([]string, len(sel))
	for i, s := range sel {
		cmds[i] = s.cmd
		nouns[i] = s.noun
	}
	return cmds[0] + "   # or: " + strings.Join(cmds[1:], " / "),
		"— open " + strings.Join(nouns, " or ") + " in this workspace",
		strings.Join(nouns, " / ")
}

// printTaskBrief displays the task brief in a styled lipgloss box.
func printTaskBrief(title string, brief string, timeLimitMinutes int) {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(cStrong).
		PaddingBottom(1)

	briefStyle := lipgloss.NewStyle().
		Foreground(cBody).
		Width(64)

	metaStyle := lipgloss.NewStyle().
		Foreground(cMuted)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#22c55e")).
		Padding(1, 2).
		Width(70)

	header := "ASSESSMENT TASK"
	if title != "" {
		header = title
	}

	var content strings.Builder
	content.WriteString(titleStyle.Render(header))
	content.WriteString("\n")

	if brief != "" {
		wrapped := strings.Join(wordWrap(brief, 64), "\n")
		content.WriteString(briefStyle.Render(wrapped))
		content.WriteString("\n")
	}

	if timeLimitMinutes > 0 {
		content.WriteString("\n")
		content.WriteString(metaStyle.Render(fmt.Sprintf("⏱  Time limit: %d minutes", timeLimitMinutes)))
	}

	fmt.Println(boxStyle.Render(content.String()))
}

// wordWrap breaks text into lines of at most maxWidth characters, splitting on
// word boundaries. Existing newlines in the input are preserved.
func wordWrap(text string, maxWidth int) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		current := words[0]
		for _, w := range words[1:] {
			if len(current)+1+len(w) > maxWidth {
				lines = append(lines, current)
				current = w
			} else {
				current += " " + w
			}
		}
		lines = append(lines, current)
	}
	return lines
}

// taskRootWithinRepo applies session.RepoSubdir to a repository root, returning
// the directory the assessment is actually scoped to.
//
// Shared by the clone path and the adopt path on purpose: they used to compute
// this independently and the adopt path simply did not, so a hosted monorepo
// assessment captured, diffed and bundled the entire repository. A subdirectory
// that escapes the root (absolute, or climbing out with ..) is ignored rather
// than honoured — the root is the safe answer and the value comes from the
// server, not from the candidate.
func taskRootWithinRepo(repoRoot, subdir string) string {
	cleaned := strings.TrimSpace(subdir)
	if cleaned == "" {
		return repoRoot
	}
	cleaned = filepath.Clean(cleaned)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return repoRoot
	}
	cleaned = strings.TrimPrefix(cleaned, string(filepath.Separator))
	if cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return repoRoot
	}
	return filepath.Join(repoRoot, cleaned)
}

// resolveWorkspacePath determines the workspace directory. If --workspace is
// provided it is used directly. Otherwise an interactive prompt is shown (or the
// default is used when stdin is not a terminal).
func resolveWorkspacePath(flagValue string, session Session) string {
	if flagValue != "" {
		return expandPath(flagValue)
	}

	cwd, _ := os.Getwd()
	cwdIsGit := isGitRepository(cwd)
	freshPath := defaultFreshWorkspacePath(session)

	// Non-interactive: use sensible default without prompting.
	if !stdinIsTerminal() {
		if cwdIsGit && session.RepoURL == "" {
			return cwd
		}
		return freshPath
	}

	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	num := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")).Width(3).Align(lipgloss.Right)
	pathStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)

	// Pick a sensible default based on context.
	defaultChoice := "3"
	if cwdIsGit && session.RepoURL == "" {
		defaultChoice = "1"
	}

	fmt.Println()
	fmt.Println(heading.Render("  Workspace Setup"))
	fmt.Println()

	// Option 1
	gitNote := ""
	if cwdIsGit {
		gitNote = dim.Render("  (git repo detected)")
	}
	defaultTag1 := ""
	if defaultChoice == "1" {
		defaultTag1 = dim.Render("  ← default")
	}
	fmt.Printf("  %s  Current folder: %s%s%s\n", num.Render("1"), pathStyle.Render(cwd), gitNote, defaultTag1)

	// Option 2
	fmt.Printf("  %s  Enter a custom path\n", num.Render("2"))

	// Option 3
	defaultTag3 := ""
	if defaultChoice == "3" {
		defaultTag3 = dim.Render("  ← default")
	}
	fmt.Printf("  %s  Fresh workspace: %s%s\n", num.Render("3"), pathStyle.Render(freshPath), defaultTag3)

	fmt.Println()
	fmt.Printf("  %s ", prompt.Render("❯"))

	scanner := bufio.NewScanner(os.Stdin)
	choice := defaultChoice
	if scanner.Scan() {
		if input := strings.TrimSpace(scanner.Text()); input != "" {
			choice = input
		}
	}

	switch choice {
	case "1":
		return validateWorkspacePath(cwd, session)
	case "2":
		fmt.Printf("\n  %s Path: ", prompt.Render("❯"))
		if scanner.Scan() {
			if p := strings.TrimSpace(scanner.Text()); p != "" {
				return validateWorkspacePath(expandPath(p), session)
			}
		}
		return validateWorkspacePath(cwd, session)
	case "3":
		return validateWorkspacePath(freshPath, session)
	default:
		return validateWorkspacePath(cwd, session)
	}
}

// validateWorkspacePath checks the chosen path and warns if it looks risky.
// For paths that are existing git repos when we need to clone, it asks for
// confirmation. Returns the path to use.
func validateWorkspacePath(chosen string, session Session) string {
	fi, err := os.Stat(chosen)
	if err != nil {
		// Doesn't exist yet — fine, we'll create it.
		return chosen
	}
	if !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "warning: %s is not a directory, using fresh workspace\n", chosen)
		return defaultFreshWorkspacePath(session)
	}
	// If we need to clone a repo and the target is already a git repo, warn.
	if session.RepoURL != "" && isGitRepository(chosen) {
		if !stdinIsTerminal() {
			// Non-interactive: honor explicit --workspace flag, proceed silently.
			return chosen
		}
		fmt.Printf("\n  Warning: %s is already a git repo.\n", chosen)
		fmt.Print("  Overwrite with assessment repo? [yes/no]: ")
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if ans == "yes" || ans == "y" {
				return chosen
			}
		}
		fresh := defaultFreshWorkspacePath(session)
		fmt.Printf("  Using fresh workspace instead: %s\n", fresh)
		return fresh
	}
	// Non-empty, non-git directory without a repo to clone — warn but allow.
	if session.RepoURL == "" && !isGitRepository(chosen) {
		entries, _ := os.ReadDir(chosen)
		if len(entries) > 0 {
			if !stdinIsTerminal() {
				// Non-interactive: honor explicit --workspace flag, proceed silently.
				return chosen
			}
			fmt.Printf("\n  Note: %s is not empty and not a git repo.\n", chosen)
			fmt.Print("  Continue anyway? [yes/no]: ")
			scanner := bufio.NewScanner(os.Stdin)
			if scanner.Scan() {
				ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
				if ans == "yes" || ans == "y" {
					return chosen
				}
			}
			fresh := defaultFreshWorkspacePath(session)
			fmt.Printf("  Using fresh workspace instead: %s\n", fresh)
			return fresh
		}
	}
	return chosen
}

// defaultFreshWorkspacePath returns ~/promptster-test (or a fallback).
// If the path already exists, it appends -2, -3, etc. to avoid conflicts.
func defaultFreshWorkspacePath(session Session) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	base := filepath.Join(home, "promptster-test")
	if _, err := os.Stat(base); err != nil {
		return base // doesn't exist yet — use it
	}
	// Path exists — find a unique suffix
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return base // fallback (shouldn't happen)
}

// expandPath resolves ~ and makes the path absolute.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// isGitRepository returns true if dir is inside a git work tree.
func isGitRepository(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// isHex returns true if every character in s is a hex digit (0-9, a-f, A-F).
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}

// stdinIsTerminal returns true when stdin is connected to a terminal.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// installSelfToBin copies the running promptster binary to ~/.promptster/bin/promptster
// so that hook commands written into Claude/Cursor settings resolve correctly
// regardless of how promptster was installed (npm global, brew, manual, etc.).
func installSelfToBin() error {
	dest := promptsterBin()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}

	// Find the currently running binary.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self: %w", err)
	}
	// Resolve symlinks so we get the real ELF/Mach-O binary.
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("eval symlinks %s: %w", self, err)
	}

	return installBinary(self, dest)
}

func installBinary(src, dest string) error {
	if filesEqual(src, dest) {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", dest, err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}

	if err := copyFile(src, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy %s → %s: %w", src, tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s → %s: %w", tmpPath, dest, err)
	}
	return nil
}

// filesEqual returns true if src and dst have identical bytes.
func filesEqual(src, dst string) bool {
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	di, err := os.Stat(dst)
	if err != nil || si.Size() != di.Size() {
		return false
	}

	sf, err := os.Open(src)
	if err != nil {
		return false
	}
	defer sf.Close()

	df, err := os.Open(dst)
	if err != nil {
		return false
	}
	defer df.Close()

	bufA := make([]byte, 32*1024)
	bufB := make([]byte, 32*1024)
	for {
		nA, errA := sf.Read(bufA)
		nB, errB := df.Read(bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false
		}
		if errA == io.EOF && errB == io.EOF {
			return true
		}
		if errA != nil || errB != nil {
			return false
		}
	}
}

// copyFile copies src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func runCommand(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func prepareWorkspaceCheckout(workspacePath, repoURL, repoCommit string) error {
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}

	if _, err := runCommand(workspacePath, "git", "init"); err != nil {
		return fmt.Errorf("git init failed: %w", err)
	}

	if out, err := runCommand(workspacePath, "git", "remote", "get-url", "origin"); err != nil {
		if _, addErr := runCommand(workspacePath, "git", "remote", "add", "origin", repoURL); addErr != nil {
			return fmt.Errorf("git remote add origin failed: %w", addErr)
		}
	} else if strings.TrimSpace(string(out)) != repoURL {
		if _, setErr := runCommand(workspacePath, "git", "remote", "set-url", "origin", repoURL); setErr != nil {
			return fmt.Errorf("git remote set-url origin failed: %w", setErr)
		}
	}

	if repoCommit != "" {
		if _, err := runCommand(workspacePath, "git", "fetch", "--depth", "1", "origin", repoCommit); err != nil {
			return fmt.Errorf("git fetch commit failed: %w", err)
		}
	} else {
		if _, err := runCommand(workspacePath, "git", "fetch", "--depth", "1", "origin", "HEAD"); err != nil {
			return fmt.Errorf("git fetch HEAD failed: %w", err)
		}
	}

	if _, err := runCommand(workspacePath, "git", "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("git checkout FETCH_HEAD failed: %w", err)
	}

	return nil
}

// Step styling ──────────────────────────────────────────────────────────────
// Numbered-step output lines look like:
//
//	1/7  Loading saved session                   ✓  /path/to/workspace
//
// The step number + total is dim, the label is light grey, and the suffix is
// either a green ✓ (success) or a yellow ! + yellow detail (warning). Labels
// are padded to a fixed width so checkmarks align across rows.
var (
	stepNum = lipgloss.NewStyle().
		Foreground(cDim)
	stepLabel = lipgloss.NewStyle().
			Foreground(cBody)
	stepCheck = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#22c55e")).
			Bold(true).
			Render("✓")
	stepWarnIcon = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f59e0b")).
			Bold(true).
			Render("!")
	stepNote = lipgloss.NewStyle().
			Foreground(cMuted)
	stepWarnNote = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f59e0b"))
	stepSection = lipgloss.NewStyle().
			Foreground(cMuted).
			Bold(true)
)

const stepLabelWidth = 34

// startStepSection prints a small section heading above the first step.
func startStepSection(title string) {
	fmt.Println()
	fmt.Printf("  %s\n", stepSection.Render(title))
	fmt.Println()
}

// stepPrefix renders "  N/M  " with the number/total dimmed.
func stepPrefix(n, total int) string {
	return "  " + stepNum.Render(fmt.Sprintf("%d/%d", n, total)) + "  "
}

// startStep prints the step line without a trailing newline so endStep can
// overwrite it on completion. In verbose mode the line terminates with \n so
// sub-step detail (verbosef) can be interleaved. Use startStepBlock when a
// sub-UI (TUI, prompt) will print between start and end.
func startStep(n, total int, label string) {
	if startVerbose {
		fmt.Printf("%s%s\n", stepPrefix(n, total), stepLabel.Render(label))
		return
	}
	fmt.Printf("%s%s", stepPrefix(n, total), stepLabel.Render(label))
}

// startStepBlock prints the step line with a trailing newline. For cases
// where an interactive sub-UI follows (e.g. consent TUI, workspace selector).
// endStep called later prints a separate confirmation line below.
func startStepBlock(n, total int, label string) {
	fmt.Printf("%s%s\n", stepPrefix(n, total), stepLabel.Render(label))
}

// endStep overwrites the current line with a success checkmark. In verbose
// mode it prints on a new line without overwriting so sub-step detail stays
// visible. Note, if startStepBlock was used, the \r still works — it just
// moves to the start of a new empty line and prints the confirmation there.
func endStep(n, total int, label, note string) {
	padded := fmt.Sprintf("%-*s", stepLabelWidth, label)
	suffix := stepCheck
	if note != "" {
		suffix += "  " + stepNote.Render(note)
	}
	if startVerbose {
		fmt.Printf("%s%s %s\n", stepPrefix(n, total), stepLabel.Render(padded), suffix)
		return
	}
	fmt.Printf("\r%s%s %s\n", stepPrefix(n, total), stepLabel.Render(padded), suffix)
}

// endStepWarn prints the step as a warning (yellow !) instead of a checkmark.
func endStepWarn(n, total int, label, detail string) {
	padded := fmt.Sprintf("%-*s", stepLabelWidth, label)
	if startVerbose {
		fmt.Printf("%s%s %s  %s\n", stepPrefix(n, total), stepLabel.Render(padded), stepWarnIcon, stepWarnNote.Render(detail))
		return
	}
	fmt.Printf("\r%s%s %s  %s\n", stepPrefix(n, total), stepLabel.Render(padded), stepWarnIcon, stepWarnNote.Render(detail))
}
