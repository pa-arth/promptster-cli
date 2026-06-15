# CLI Changelog

## 1.5.0 - 2026-06-15

- feat(brief): `promptster brief` now opens a live, structured brief in its own terminal window by default, so the task stays on-screen beside the candidate's editor instead of scrolling away. The viewer is a scrollable Bubbletea TUI with a sticky header, a live countdown + color-coded progress bar, and per-phase cards — fed by a new structured brief (scenario, codebase orientation, ordered phases, evaluation dimensions, ground rules, deliverables) threaded through from the backend. `--here` keeps it in the current terminal, `--plain` prints once for piped/no-TTY use, `--json` emits machine-readable output, and `--demo` previews the viewer without a live session
- feat(brief): new-window spawn is cross-platform — AppleScript/iTerm2 on macOS with fallbacks across 8 Linux terminal emulators; when no window can be opened the viewer falls back to running inline
- compat(brief): assessments that still send the legacy flat `TaskBrief` string render unchanged — `resolveBrief()` falls back automatically, so older backends and in-flight sessions are unaffected

## 1.4.0 - 2026-06-12

- feat(capture): every Claude Code hook event now carries the per-process lane identity (`meta.ideSessionId` = Claude's `session_id`, `meta.cwd`) — previously only prompts did. Concurrent `claude` sessions in the same workspace are now fully distinguishable downstream, including which lane authored each file edit and command (feeds the worker's parallel-orchestration signals)
- feat(capture): the BYO transcript watcher stamps the same lane identity on every transcript-derived event (one transcript file = one Claude process)
- feat(capture): the transcript watcher now also tails sessions running in git worktrees registered to the workspace repo (`git worktree list` is consulted every poll) — candidates who parallelize with `git worktree add ../fix` are no longer invisible to BYO capture

## 1.3.4 - 2026-06-11

- feat(capture): the BYO transcript watcher now converts candidate-typed slash commands (`<command-name>` envelopes) into `tool_intent` events with `toolName: "SlashCommand"` instead of dropping them — pure-transcript capture now feeds the ecosystem_leverage rubric dimension the same way hook capture does. Claude Code built-ins with dedicated semantics (`/clear`, `/compact`, `/model`, etc.) stay excluded; Promptster's own `/explain` is emitted and filtered worker-side by input preview, matching the hook path

## 1.3.3 - 2026-06-01

- fix(consent): the CLI consent screen no longer re-shows the full disclosure to candidates who already accepted via the web flow — they now see "Terms already accepted on the web" plus only the cadence opt-in question, so no candidate is forced to accept twice
- fix(consent): `--accept-tos` now prints the full disclosure + ToS URL before auto-accepting so scripted and Claude Code-driven runs leave a displayed record of what was consented to (previously showed nothing)
- fix(consent): added "Anonymized device info for session continuity" to the consent disclosure — the CLI was already collecting a device fingerprint but the consent screen never mentioned it; the web page was the only surface that disclosed this
- feat(consent): the API now serves a canonical `disclosure` object from `GET /v1/candidate/consent`; the CLI fetches it so both surfaces always render the same capture/non-capture lists and can't drift apart. An embedded fallback list is used if the API is unreachable

## 1.3.2 - 2026-05-30

- fix(reset): `promptster reset` no longer reports success when files could not be removed. `removeGlobalConfigKeepBin` now surfaces per-entry removal failures and returns a count, and both the `--purge` and keep-bin paths warn on failure and print a "partially reset" message instead of a clean "configuration reset" — this is the recovery command, so a silent failure would strand the user with a half-broken setup

## 1.3.1 - 2026-05-30

- fix: `promptster start` no longer warns candidates to `claude /logout`, and the subscription-detection that drove it is removed. The premise was wrong: Claude Code's credential precedence puts the `apiKeyHelper` **above** a logged-in Max/Pro subscription (cloud creds > `ANTHROPIC_AUTH_TOKEN` > `ANTHROPIC_API_KEY` > `apiKeyHelper` > `CLAUDE_CODE_OAUTH_TOKEN` > subscription OAuth — confirmed 2026-05-30). The proxy wins over a candidate's subscription with no logout required; the old warning added pointless friction to every Max/Pro candidate's first run. The earlier 401s it was built against came from a shell-exported `ANTHROPIC_AUTH_TOKEN` (which genuinely out-ranks the helper), not from OAuth — that case is still caught by the proxy smoke test and `promptster doctor`

## 1.3.0 - 2026-05-23

- feat: `promptster reset [--purge]` — wipe local Promptster config to recover from a broken setup. Tears down the session, hooks, RC source lines, and workspace `.claude` config (like `abort`), then sweeps `~/.promptster` keeping only the installed binary so the next `start` rebuilds without a reinstall. Unlike `abort` it resolves the workspace from the active-workspace pointer when `session.json` is unreadable, so it works even when the session is corrupt. `--purge` removes the binary too (full uninstall)
- change: `promptster cleanup` is now surfaced as `promptster abort` ("discard the current session + tear down hooks without submitting"). `cleanup` still works as a hidden alias, so the shell-hook self-eviction path and existing muscle memory are unaffected
- fix(doctor): add an `apiKeyHelper resolves a token` check that fails on an **expired** session. Previously an expired session passed every doctor check yet emitted no token from the `apiKeyHelper` (it self-evicts), so Claude Code would silently 401 with a clean bill of health. The check mirrors `auth-token`'s exact conditions (token + workspace present, not past `expiresAt`)
- fix(discoverability): the interactive menu (bare `promptster`) now lists **Run doctor** and **Abort assessment**; `promptster help` documents `abort`, `reset`, the `cleanup` alias, and the `eval "$(promptster env --clear)"` escape hatch — all of which existed but were undiscoverable from the CLI itself

## 1.2.0 - 2026-05-23

- feat: route Claude Code through the Promptster proxy via an `apiKeyHelper` in the workspace's `.claude/settings.local.json` instead of exporting `ANTHROPIC_AUTH_TOKEN` into the shell. The credential is sourced on demand from the 0600 `session.json` by a new `promptster auth-token` subcommand and never lands in any shell env or 0644 file
- feat: the proxy is now scoped to the workspace by construction — `apiKeyHelper` only applies when Claude Code runs with the workspace as its project root, so personal `claude` runs and library audits (which run in a throwaway clone) use your own subscription with no escape hatch needed
- removed: the shell hook no longer exports/evicts Anthropic proxy env. It now only captures terminal commands (PWD-gated to the workspace) and fires TTL self-eviction. This deletes the per-prompt proxy sync, the cross-directory eviction, and the whole "PST token leaked into an unrelated terminal" class of bug
- change: `promptster env` no longer prints exports — it is a TTL self-eviction trigger only. `promptster env --clear` still emits `unset` lines to clean up creds left behind by a pre-1.2.0 session
- change: `promptster doctor` now flags an `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` exported in your shell (it out-ranks the helper and breaks capture), and verifies the workspace settings carry `apiKeyHelper` + `ANTHROPIC_BASE_URL` rather than a baked-in key
- note: a logged-in Claude subscription (OAuth) still out-ranks the helper, so `promptster start` continues to warn you to `/logout` before the first prompt

## 1.1.1 - 2026-05-23

- fix(security): remove the assessment title from `TASK.md` and `promptster brief`. Titles are auto-derived from the upstream issue title (e.g. "OR conditions silently converted to AND"), which handed candidates the answer in the first line of their task file. The task brief is now the single source of truth for what the candidate sees
- fix: exclude CLI-generated files (`TASK.md`, `.promptster/`) from the submission diff, periodic snapshots, and live `file_diff` events via git pathspec, so they no longer pollute the diff recruiters review
- fix: per-prompt proxy eviction — the shell hook now unsets all three `ANTHROPIC_*` vars on `cd` outside the workspace (including agent shells that never entered it), adds `promptster env --clear` and a `doctor` leak check so `claude -p` audit runs don't hit the candidate proxy

## 1.1.0 - 2026-05-21

- fix(security): stop inlining the candidate's `ANTHROPIC_AUTH_TOKEN` into `~/.promptster/shell-hook.sh` (mode 0644, sourced by every interactive shell). The token now lives only in the per-session `<workspace>/.promptster/session.json` (0600); the hook reads it dynamically via a new `promptster env` subcommand on each shell start
- fix(security): PWD-gate the shell hook so the Anthropic proxy env and command-capture hooks only activate when the shell starts inside the workspace tree. Previously the proxy creds were exported in every new terminal anywhere on the candidate's machine, hijacking unrelated Claude Code work
- feat: `promptster cleanup [--reason X]` — tear down hooks, RC source lines, and local state WITHOUT submitting. Fixes the "`promptster done` is the only exit" trap that left abandoned sessions with active proxy creds and stale shell hooks
- feat: shell hook self-evicts when the candidate key's local TTL has passed — `promptster env` fires `promptster cleanup --reason expired` in the background and prints no exports, so the next new terminal is back to a clean shell with zero candidate intervention
- feat: `/v1/candidate/redeem` now returns `expiresAt`, persisted into `session.json` so the CLI can do local staleness checks without an API round-trip per new shell
- internal: `appendProxyToShellHook`, `stripProxyBlock`, and the `# >>> promptster proxy >>>` marker mechanism are removed — the static hook template is complete on its own and no longer needs to be rewritten with a token-bearing block
- internal: regression tests guard against re-inlining tokens (`TestShellHookScriptStructure`) and shell-injection via the token value (`TestShellQuoteHandlesSingleQuote`)

## 1.0.0 - 2026-05-17

- feat: candidate workspace is uploaded as a single `bundle.tar.gz` to Supabase Storage on `promptster done` (signed PUT URL, SHA-256 verified, 5MB/file + 100MB total cap, symlinks skipped) — recruiter view can now traverse the candidate's full final-state repo, not just the diff
- fix: route Claude Code auth through `ANTHROPIC_AUTH_TOKEN` instead of `ANTHROPIC_API_KEY`; Claude Code 2.x validates `ANTHROPIC_API_KEY` against api.anthropic.com and blocks startup on custom-gateway tokens, but `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer` with no validation hop
- fix: install shell hook before writing the proxy env block so `installShellHook`'s file rewrite no longer wipes `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` exports — this was the actual production blocker that prevented the proxy from ever receiving real candidate prompts
- fix: shell-hook proxy block is now wrapped with `# >>> promptster proxy >>>` / `# <<< promptster proxy <<<` markers and rewritten on every `start`, so token rotation and env-var-name migrations are idempotent across CLI versions (legacy pre-marker blocks are stripped automatically)
- fix: API proxy accepts `Authorization: Bearer` in addition to `X-API-Key`, so the upgrade is forward-compatible with in-flight candidate sessions still on older CLI builds

## 0.9.0 - 2026-04-22

- feat: per-session Ed25519 signing of every event the CLI emits, with a hash chain linking each event to the previous one (`prevSig`) — any edit to an ingested record is detectable after the fact
- feat: `promptster verify [sessionId|PST-...]` — fetches the signed event log and walks the chain offline, reporting events signed / chain intact or the index of the first break
- feat: new session keypair is generated during `redeem` and persisted at `.promptster/session.key` (0600); public key is registered with the server so anyone can verify the session later with just the pubkey + event log
- internal: all event emission paths (Claude hooks, shell hook, decision capture, git watcher) now serialize through a single `flock`'d append so the chain stays consistent across concurrent hook processes
- removed: desktop notification system (notify.go, notify_icon.go, embedded icon, beeep dep) — older CLI versions spawned a `promptster decide service` daemon that kept firing notifications after `done`; queued-decision and explain reminders now print to the TTY instead. `promptster done` still sweeps any orphaned watcher daemons left by prior installs

## 0.8.1 - 2026-04-20

- feat: push a throttled workspace snapshot on every Claude Code hook so server-side auto-submit (on time-limit expiry) has candidate code to run tests against — file-edit hooks bypass the throttle so fresh code lands within seconds of any save
- fix: tests are no longer silently skipped when a session is auto-submitted or completes without an upload; the reviewer now sees a clear explanation instead of a blank test result

## 0.8.0 - 2026-04-19

- feat: optional cadence-based authorship signals — prompts are enriched with `timeSinceLastPromptMs`, `promptLengthChars`, and `likelyPaste` when the candidate opts in at consent time
- feat: per-session honeypot trap file written to `.promptster/session-notes.md` with a random token; the backend flags the token if it appears in prompts or command output
- feat: integrity opt-in question after ToS acceptance; all new signals are off by default and captured only with explicit consent
- feat: session metadata (integrity consent + honeypot token) carried through the `session_start` timeline event — no new API surface

## 0.7.4 - 2026-04-16

- fix: sweep orphaned watcher daemons on `promptster start` to stop duplicate notifications
- feat: use Promptster logo for Windows/Linux notifications (embedded PNG via `go:embed`); macOS still shows Script Editor icon (requires a signed `.app` helper, deferred)

## 0.7.3 - 2026-04-16

- feat: populate provenance on CLI events for AI/human attribution — `likely_ai` on IDE tool-hook file diffs and commands, `likely_human` on prompts and shell-hook terminal commands

## 0.7.1 - 2026-04-15

- feat: require Claude Code in preflight checks (hard fail if missing)
- feat: platform-specific install hints for Claude Code and Node.js
- remove: drop Cursor support — Claude Code is now the only supported editor
- remove: Cursor hook configuration, detection, and cleanup

## 0.7.0 - 2026-04-14

- feat: fully non-interactive CLI mode for scripted/CI use
- feat: `--json` flag on `promptster status` and `promptster brief`
- feat: `--accept-tos` flag on `promptster redeem`
- feat: pass `--accept-tos` through from `start` to `redeem`
- feat: guard workspace prompts with `stdinIsTerminal()` check
- feat: suppress OS notifications in non-interactive mode
- fix: `createdBy` crash when creating assessments via org API key
- fix: git-watcher state files not cleaned up on `promptster done`
- remove: GitHub submissions repo push (API upload is sufficient)

## 0.6.1 - 2026-04-08

- fix: org branding table, idempotent migrations, and SQL query fixes
- fix: compute total_cost_usd from usage_records instead of sessions table

## 0.6.0 - 2026-04-02

- feat: remove local test execution — tests now run server-side in E2B sandboxes
- feat: `--auto` flag on `promptster done` for time-expiry auto-submit
- feat: time limit warnings + OS notifications at 30/15/10/5/2/1 min
- feat: interactive menu + `promptster explain` accepts inline rationale
- feat: direct file upload on `promptster done` (submit-code endpoint)

## 0.5.0 - 2026-03-26

- feat: server-side test execution via GitHub Actions (tests no longer run locally)
- feat: `promptster brief` command — show task brief + time remaining
- feat: preflight checks on start (git, node, claude code)
- feat: `.promptster-meta.json` written for CI test lookup
- feat: issueId tracked in session for CI pipeline
- feat: high-contrast lipgloss colors for all CLI output
- fix: remove degit, always use git shallow clone
- fix: co-author attribution on submission commits

## 0.4.2 - 2026-03-25

- feat: workspace-local state — session files now live in `<workspace>/.promptster/`
- feat: lipgloss-styled `promptster start` output (step progress, task brief box)
- feat: CLI version header + update check on startup
- feat: nudge interval changed to 15 minutes
- fix: installer message says `source ~/.zshrc` instead of raw export
- fix: workspace cleanup message on `promptster done`

## 0.4.1 - 2026-03-25

- feat: API proxy — candidates no longer need their own Anthropic key
- feat: `promptster start` configures ANTHROPIC_BASE_URL to route through proxy
- feat: `promptster explain` replaces automated decision watcher with user-initiated captures
- feat: send oldString/newString for diffs (server-side diff computation)
- feat: show assessment title in CLI watcher window
- fix: remove manual LCS diff computation from CLI

## 0.3.3 - 2026-03-21

- feat: push candidate code to GitHub submissions repo on `promptster done`
- fix: read prompt text from correct field (`prompt` not `transcript`)
- fix: align Stop, SessionStart, SessionEnd with current Claude Code hook docs

## 0.3.2 - 2026-03-20

- feat: `promptster start <key>` auto-redeems (single command flow)
- feat: register all 5 Claude Code hook points (UserPromptSubmit, Stop, Notification were missing)
- feat: register postToolUse + postToolUseFailure hooks for Cursor
- feat: lipgloss-styled consent screen, workspace picker, decision watcher
- feat: task brief displayed on decision watcher screen
- feat: auto-copy replay URL to clipboard on `promptster done`
- fix: stop decision watchers on `done`
- fix: pre-fix SHAs for jest and got issues

## 0.3.1 - 2026-03-18

- fix: run decision detection on human terminal commands (shell hook)
- fix: align decision detection keywords with reviewer-side enrichment
- fix: clean up all promptster state files on `done` (buffer, hooks, decision queue, workspace configs)
- fix: remove Promptster-specific path patterns, use generic framework-agnostic detection

## 0.3.0 - 2026-03-18

- feat: shell hook captures human terminal commands (bash DEBUG trap + zsh preexec)
- feat: auto-inject shell hook into .zshrc/.bashrc on `promptster start`
- feat: clean removal of shell hook on `promptster done`
- feat: run assessment test suites on `promptster done` with pass/fail display
- feat: show results URL with candidate key after completion
- feat: updated consent disclosures (file reads, search, IDE extension timing)

## 0.2.12 - 2026-03-16

- feat: decision capture TUI, architectural detection, and CI workflow (8eef843)
## 0.2.11 - 2026-03-13

- feat(cli): switch default workspace setup from git to degit
- fix(ci): authenticate git push to public releases repo

## 0.2.9 - 2026-03-11

- fix: one session per candidate key, add lastActivityAt tracking, source detection (71bea44)
- feat: CLI decision watcher, hooks improvements, updated binaries & docs (70957ad)
- Release/cli v0.2.8 (#5) (37d562a)
- fix: standardize API URL defaults to api.promptster.ai, add session URL fallback for hooks (0645088)
## 0.2.8 - 2026-03-10

- chore(cli): isolate release workflow and self-install fix (0fafd79)
- Improve hook handling and Cursor tests (#4) (4c92e29)
- chore: checkpoint unrelated local changes (#3) (c007f30)
- feat: Clerk webhook route, hook normalizer updates, candidate guide (d9e33c3)
## 0.2.7 - 2026-03-10

- Existing CLI release baseline before the isolated monorepo release workflow.
