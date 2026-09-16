# Promptster CLI — Feature Map

What a candidate can actually do with this binary, and how an agent drives it.
Read this before driving a command; it costs far fewer tokens than re-deriving
the dispatch table from `main.go`.

Every drive command below assumes, from the repo root:

```bash
CC=".agents/skills/verify-cli/control-cli.mjs"
```

## Before anything: three ways a proof lies here

**1. The binary is not the source.** Go does not rebuild on its own. Editing a
`.go` file and re-running the sandbox binary verifies the *previous* build.
`doctor` → `binaryCurrent` catches this; `build` fixes it. This is the most
common false pass in this repo.

**2. Exit 0 is not capture.** `promptster hook` deliberately fails open — it must
never block a candidate's editor — so it exits 0 whether it emitted an event or
silently dropped it. The watchers are detached daemons whose failure mode is
silence: exit 0, empty buffer, a line in a log nobody reads. The proof is
`state --what buffer` **plus** `state --what watchers`, never the exit code.

**3. The `source` is the assertion.** An event captured under the wrong `source`
is in the buffer and looks like success. The real values, from `normalize.go`
and `normalize_codex.go`:

| Origin | `source` |
|---|---|
| Claude Code (hook or transcript watcher) | `claude-code` — **not** `claude` |
| Codex (rollout watcher) | `codex` |
| Cursor (`promptster hook cursor`) | `cursor` |
| The CLI itself (e.g. `session_start` from `start`) | `cli` |

A buffer holding only `cli` events proves the CLI ran. It proves **nothing**
about editor capture.

## The candidate loop (this is the product)

| # | Feature | Command | File |
|---|---------|---------|------|
| 1 | [Start](start.md) — redeem, workspace, hooks, watchers | `promptster start` | `cmd_start.go` |
| 2 | [Capture](capture.md) — hooks and watchers filling the buffer | `promptster hook`, `*-watch` | `cmd_hook.go`, `cmd_claude_watch.go`, `cmd_codex_watch.go`, `git_watcher.go` |
| 3 | [Doctor](doctor.md) — diagnose a broken setup | `promptster doctor` | `cmd_doctor.go` |
| 4 | [Brief](brief.md) — the live task viewer (a TUI) | `promptster brief` | `cmd_brief.go`, `brief_tui.go` |
| 5 | [Status](status.md) — session info and capture health | `promptster status` | `cmd_status.go` |
| 6 | [Explain](explain.md) — decision rationale (a TUI) | `promptster explain` | `cmd_explain.go`, `decision_tui.go` |
| 7 | [Done](done.md) — submit | `promptster done` | `cmd_done.go`, `code_submit.go` |
| 8 | [Teardown](teardown.md) — abort and reset | `promptster abort`, `reset` | `cmd_cleanup.go`, `cmd_reset.go` |
| 9 | [Codex launch](codex-launch.md) — codex wired to the proxy | `promptster codex` | `cmd_codex.go`, `codex_proxy.go` |

Smaller surfaces, no file of their own: `promptster version`, `promptster help`,
`promptster env [--clear]` (shell self-eviction / legacy var clearing,
`cmd_env.go`), `promptster auth-token` (the Claude `apiKeyHelper`, `cmd_env.go`),
`promptster verify <sessionId|PST-…>` (fetches and checks the signed event log,
`cmd_verify.go` — needs a reachable backend, so it cannot be verified in the
sandbox), and the interactive menu shown when the binary is run with **no
arguments** (`main.go:interactiveMenu`, a numbered stdin prompt, not a TUI).

`promptster decide` is **retired** — it exits 1 pointing at `explain`. Do not
map it to anything.

## Aliases that are the same code path

- `task` → `brief`
- `cleanup` → `abort` (the legacy name; the shell hook's self-eviction calls it)

## Which flows this machine can verify at all

`start --tools X` only keeps a tool whose binary is present. `doctor` reports
what is installed under `checks.tools`. If `codex` is absent, the codex flow
**cannot** be verified here — say so, do not report a pass.

Cursor is no longer a selectable tool (`cmd_doctor.go` retired it: Cursor routes
model traffic through its own backend, so the hiring team's key cannot be
metered on it). It survives only as `promptster hook cursor`, and as a "this
session names a retired tool" notice in `doctor`.

## The TUI ceiling

`brief` and `explain` are bubbletea TUIs. `control-cli.mjs tui` captures their
**rendered screen** through a real PTY, and that part works. **Keystrokes do not
reach them** — measured, not assumed; see the SKILL.md section "TUIs: you can see
them, you cannot drive them". Anything behind a key press is unverifiable with
this harness. Report it as unverified rather than inferring it from the source.
