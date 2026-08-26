# CLI Changelog

## 1.10.0 - 2026-08-26

- fix(codex): Promptster no longer rewrites the machine's global codex config, so an assessment stops breaking every other codex on it. `start` used to write a marker-fenced provider block plus `model_provider = "promptster"` into `~/.codex/config.toml`, because codex will not scope a model provider to a workspace. That file is shared by every codex run on the machine, so for the length of a session the candidate's own codex — in their own project, in an unrelated terminal — resolved to our provider and died on `Missing environment variable: PROMPTSTER_PROXY_TOKEN`, a variable only the assessment workspace ever exported. Worse, it outlived the session: `done` and `abort` reverted it, but a crash, a closed laptop, a key that simply expired, or a wiped `~/.promptster` left `model_provider = "promptster"` live forever, and codex stayed broken on that machine until someone hand-edited TOML. The exact-revert sidecar could not help — it lived in the directory that gets wiped. `promptster codex` now passes the whole provider as `-c` overrides on the launch it performs, so the blast radius is one process: nothing is written, nothing needs reverting, and nothing a crash leaves behind can outlast the process it was set for. Anything the user's own config says other than the provider — their `model`, their tools, their project trust levels — still applies, and an explicit `-c model_provider=...` of their own still wins
- fix(codex): upgrading heals a machine an earlier version already broke. `start`, `done`, `abort`, `reset`, `doctor` and `promptster codex` each purge the block CLI ≤1.9 wrote into `~/.codex/config.toml` and restore the `model_provider` it displaced. The purge is unconditional, not session-gated, which is the point: the old reconciler only stripped the block when it judged the owning session stale, and it judged staleness from `~/.promptster` — so a wiped state directory read as "no session", which is also exactly what a healthy machine looks like. Nothing writes that block any more, so its presence is by definition a leftover. If the file cannot be rewritten, `doctor` says so and prints the two marker lines to delete by hand
- fix(hook): the shell hook installs a workspace-gated `codex` launcher instead of exporting the proxy credential into the shell. The export existed only because the provider was global: any shell that might run codex needed the token, so the token went into the environment and stayed there. Now the credential goes into one child process and nowhere else — typing plain `codex` inside the assessment workspace routes through `promptster codex`, and anywhere else it runs the real binary unchanged. Dispatch is re-evaluated per call rather than at source time, so the launcher goes inert the moment the session ends, in shells that are already open — something a config file could never do. It declines to wrap a `codex` that is already a function or an alias, and defines nothing at all when codex is not installed

## 1.9.0 - 2026-08-26

- fix(codex): `promptster codex` carries the proxy credential itself, because the shell that `start` tells you to use is the one shell that could never have had it. Codex has no `apiKeyHelper` equivalent, so its managed provider block reads the credential from `$PROMPTSTER_PROXY_TOKEN` — and the only thing that ever exported that variable was the shell hook, which is sourced from an RC file. `promptster start` runs inside an already-open shell, so the shell a candidate is standing in when they read the printed next steps was, by construction, the one that had registered nothing: no command capture, and no token. Following step 2 in that shell produced codex's "Missing environment variable: PROMPTSTER_PROXY_TOKEN" every time. It was never intermittent. The new command reads the 0600 `session.json` and hands the token to the child directly, so it depends on no RC file, no interactive shell and no supported shell family — which is why it is what `start` now prints. Claude Code was never affected: its credential rides an `apiKeyHelper` read at runtime. The "Codex ready" line came from a smoke test that authenticates with the session token over a direct HTTP call, so it proved the proxy and the org key were healthy and proved nothing about whether codex would find a credential; `start` now says so rather than letting step 2 look like an arbitrary detour
- fix(hook): the shell hook installs unconditionally and re-resolves everything per prompt. It used to read `~/.promptster/active-workspace` once at source time and return early when no session was live, which is what made the candidate's own shell blind for its entire life — terminal capture silently off, and a shell that *did* have the hook loaded held the workspace path captured before `start` ran. The workspace, the codex token and the TTL eviction are now all resolved from the per-prompt hooks of both shell families. The token in particular is re-read from `auth-token` every prompt rather than cached on "already exported": `auth-token` is what notices an expired session, so caching stranded a dead credential in the environment and skipped the only expiry check that shell would ever make
- fix(cleanup): teardown is single-flight, so the two AI rails cannot take each other down. Every caller of the expiry path is hot — Claude Code runs the `auth-token` helper once per API request, the shell hook once per prompt, `promptster codex` on launch — so the moment a session expires they all fire `cleanup` at once, and it is not safe to run twice. `revertCodexProxy` restores the user's own `model_provider` from a state file it then deletes, so a second runner finds no state, restores nothing, and leaves their personal codex pointed at a provider that no longer exists — globally, not just in the assessment, which is the exact failure that state file was written to prevent. Cleanup also removes both the Claude workspace settings and the codex block, so a storm triggered by one rail took the other with it. The lock lives in the global directory rather than the workspace, because cleanup deletes the workspace state directory partway through its own run; it goes stale after two minutes so a killed teardown cannot disable eviction forever
- fix(consent): the CLI renders the de-identified training-use clause it has been asking candidates to agree to. The consent disclosure has four lists and the Go struct had a field for three — `dataUse` was decoded by nothing and displayed by nothing, while the server stamped a corpus-eligible version and a sha256 anyway. The client now sends the disclosure attestation the backend has accepted since #783 and no client had ever produced, by echoing back the hash the server hands out on GET rather than trying to reproduce `JSON.stringify` key order from a decoded struct
- fix(extension): installing the bundled `.vsix` asks first. It ran `--force` with stdin closed, which deliberately suppresses the editor's own confirmation, into both VS Code and Cursor. It now prompts `[Y/n]`, declines when there is no TTY, and takes `--editor-extension` for scripted lanes
- feat(start): the hosted lane reports its boot, and says "unknown" for what the box cannot see rather than guessing (#18)
- feat(cli): the hosted lane adopts the codespace's existing checkout instead of cloning a second copy of the repo (#17)
- fix(experiment): a claim is a file with a body, and reading the body was a second syscall (#24)

## 1.8.1 - 2026-08-23

- fix(capture): the editor attention events that fail to send are the ones retried, and the ones already delivered are not sent again. The bundled editor extension spends one request per event against an ingest endpoint capped at 100 a minute per candidate key — a bucket it shares with this CLI's own hook binary — so a candidate working quickly can be refused, and that refusal is invisible to them. When it happened the transport requeued a positional slice of the batch chosen by a *count* of successes; sends complete out of order, so it retried events that had landed and discarded the ones that had not. Each half damages the same panel: the discarded events are a silent hole in the attention track, and the re-sent ones write a second timeline row, inflating the file-open count and dwell totals a reviewer reads as what the candidate actually looked at. That panel is the one thing separating what the person read from what the agent read, so a number that drifts either way is worse than no number. Carries promptster-vscode 0.3.1, which also holds the sender's concurrency limit across callers instead of resetting it per batch
- fix(npm): `npm install -g @promptster/cli` installs a `promptster` command. It has not since at least 1.5.0 — `package.json` declared `"bin": {"promptster": "./bin/promptster.js"}` and the tarball shipped no `bin/` at all, because the root `.gitignore`'s unanchored `bin/` pattern also matched `npm/bin/`, so the shim could never be committed. npm neither warns nor fails when a `bin` target is missing from a package — it links nothing and exits 0 — so four releases reported a successful install and left nothing on PATH. The shim is committed, the ignore pattern is anchored, the shim marks the platform binary executable at runtime rather than in a `postinstall` (which `--ignore-scripts` would skip), and the release now packs the tarball with the packer it publishes with, installs it into a throwaway prefix and *runs* `promptster --version` before publishing. The previous check read a file list, which is why it was green over a broken package

## 1.8.0 - 2026-08-23

- fix(done): `promptster done` refuses to submit an empty bundle when the work is somewhere else. An empty diff used to be ambiguous in the worst possible way — "the candidate wrote nothing" and "the candidate wrote a lot where we cannot see it" produced an identical silent submission, and those support opposite hiring decisions. Two paths put work outside `TaskRoot`, both observed: generated setup instructions told candidates to re-clone a repo the CLI had already prepared, and agents following a "always work in a git worktree" instruction create a linked worktree `git -C <taskRoot> diff` cannot see. Reproduced end to end on a 150-minute assessment: 1,090 insertions across 11 files, none of it in the bundle, the session looking perfect. `done` now walks linked worktrees and nested checkouts, names every path holding work, and stops. `done --auto` is the time-limit path, so it prints the same block but still submits — refusing there would leave the session open forever
- fix(done): only checkouts that are demonstrably the assessment repo can block a submission. A workspace is whatever directory the candidate picked, so it may hold repositories with nothing to do with the assessment, and blocking on someone's unrelated dirty side project would strand a valid submission. A nested checkout has to carry the pinned base commit or resolve its `origin` to the assessment repo; either signal alone is enough, because a shallow clone has the remote without the object and a clone made from the local workspace has the object without the remote. A failed base comparison is reported as an explicit unknown rather than as zero commits — treating it as zero is how a candidate who committed everything into a shallow clone gets a pristine workspace submitted on their behalf
- fix(start): every instrumented tool gets its proxy smoke-tested, not just Claude Code. Step 5 printed "skipped (no Claude Code in this session)" for a Codex-only assessment and verified nothing, which inverted the point of the step — Codex has no other preflight, so a Codex-only session was the one flying blind, and the first thing to find a broken model key was the candidate's first prompt, on the clock. It also kept the failure invisible on our side: the proxy records an org key's authentication failure only when a request actually reaches it, so a rejected hiring-team key read as healthy indefinitely. One sat dead for two months, never used, zero recorded failures. A Codex failure deliberately does *not* arm transcript capture the way a Claude failure does — Codex capture never rode the proxy, so what breaks is the candidate's model access, not our visibility
- fix(doctor): `doctor` asks whether a model is actually reachable, for both providers. Every Codex check in it read local state — provider block wired, token resolves, session not expired — so the command whose entire job is answering "why isn't this working" reported all-green on a session whose key had been dead for months. The probe is skipped, and says so, when the session is already known stale: the check above it has already named the real fix, and probing anyway either contradicts it with "contact the recruiter" or (because the proxies gate on key status, not expiry) prints a green "model reachable" directly beneath "session expired"

Internal only: repository documentation corrected to match the code after Cursor
was retired as an instrumented tool (#14).

## 1.7.0 - 2026-08-22

- feat(capture): `promptster start` installs the Promptster editor extension, so what the *candidate* read is recorded beside what the agent read. The `.vsix` is embedded in the binary — there is no download and no marketplace, because an assessment must not depend on a release host being reachable at the moment a candidate begins. Install is non-fatal by design: every failure is an availability state, bounded by a 45s timeout, and the candidate is told in both directions that their assessment is unaffected. `--no-editor-extension` declines, and declining is recorded too
- feat(capture): the *shortfall* is recorded, which is the half that matters. A session with no attention events because no instrumented editor was present must never read as a candidate who opened no files — those support opposite hiring decisions. `start` posts an `editorCapture` report (status, installed/failed/detected, extension version, vsix sha256) with `session_start`, and the review surface says which of the two happened
- feat(doctor): installed, activated and capturing are checked separately. "Installed" is the only one visible from outside and it proves the least — an installed extension that never activated produces a session that looks exactly like a candidate who opened no files. `doctor` also reports the retired-tool state
- feat(cli): `--byo-subscription` is retired as a configuration. The hiring team supplies model access, so proxy wiring is now unconditional for both Claude Code and Codex. The flag still parses and prints what changed rather than exiting — killing the process over a removed flag at the start of a timed assessment is the wrong trade. Transcript capture is *not* retired with it: it now triggers on the proxy smoke test failing, which is exactly when reading the transcript beats reading nothing
- feat(cli): Cursor is no longer a selectable tool. It routes Agent/Edit traffic through its own backend, so a key the hiring team supplies can be neither used nor metered on it — a Cursor assessment is candidate-pays by construction, which is the one arrangement this product no longer offers. `cursor` survives as a *recognised-but-retired* token and stops with an explanation naming who can fix it, rather than falling through the unknown-token path and silently running the assessment on Claude Code and Codex instead. Cursor remains a fine editor to work in; it is not an instrumented agent

## 1.6.0 - 2026-08-16

- feat(tools): Codex- and Cursor-only assessments now work end to end. Early preflight used to hard-require the `claude` binary before the assessment's tools were even known, so a candidate assigned Codex or Cursor was blocked by a tool they were never asked to use. Preflight now checks only the selected tools, warns and drops any that are missing, and hard-fails only when none are usable; Cursor is detected via a PATH shim or its macOS app bundle. Missing run/test toolchain binaries are advisory, never a gate
- feat(tools): warnings, `doctor`, and the brief all name the tool the candidate is actually using. The "already running — restart it" warning and the proxy-skip step follow the active tools instead of assuming Claude Code, and `promptster doctor` gained Codex-binary, Cursor-editor, and Cursor-hooks checks while scoping its Claude checks to Claude assessments. The brief's time bar carries a tool chip ("Claude Code · Codex CLI") with a matching Logistics row, and omits it cleanly on older sessions
- feat(redeem): when a recruiter turns on BYO-subscription for an assessment, the candidate no longer has to know to pass `--byo-subscription`. `promptster redeem` stores the recruiter-set auth mode with the session and `promptster start` activates the BYO path from it — the flag still works, and either source turns it on
- feat(capture): interrupts are captured. Pressing ESC or Ctrl+C to stop Claude mid-turn says something real about how a candidate steers, and it left no trace in the record before this
- fix(capture): the `planning` signal came back from the dead. Claude Code renamed TodoWrite/TodoRead to TaskCreate/TaskUpdate/TaskList, and because a rename doesn't throw, the normalizer went on matching only the old names and silently recorded nothing — TodoWrite=0 against TaskUpdate=43 and TaskCreate=25 across three days of real transcripts. The new tools aren't shape-compatible with the old ones and are now handled on their own terms, with `TaskList` recorded as a read

Internal only, and deliberately not part of the candidate CLI: the fleet
practice-effect experiment harness (`promptster-experiment` — its own package and
its own binary, excluded from `make build` and the release cross-compile) gained
task envelopes with pre-work arm assignment, backend sync for the assignment log,
and a fix for envelopes that resolved by checkout rather than by assignment.

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
