# Capture — hooks and watchers filling the buffer

The product is the event log. Everything else is scaffolding. Source:
`cmd_hook.go`, `cmd_claude_watch.go`, `cmd_codex_watch.go`, `git_watcher.go`,
`normalize.go`, `normalize_codex.go`, `normalize_claude_jsonl.go`.

## Sub-features

- **`promptster hook`** — Claude Code hooks POST a JSON payload on stdin.
- **`promptster hook cursor`** — the Cursor route; forces
  `PROMPTSTER_HOOK_SOURCE=cursor` and always answers non-blocking
  (`{"continue":true}` for `beforeSubmitPrompt`, exit 0 otherwise).
- **`promptster hook shell-cmd`** — the shell hook's command capture.
- **`promptster claude-watch`** — daemon tailing Claude transcript JSONL.
- **`promptster codex-watch`** — daemon tailing codex rollout JSONL.
- **`promptster diff-watch`** — daemon polling git for file diffs (60s), and the
  ordinary time-limit detection path.
- Redaction (`redact.go`), path relativization, and cross-channel dedupe
  (`diff_dedup.go`) before anything is buffered or POSTed.

## How to get to it (user POV)

A candidate never runs these. They type in Claude Code or codex and the events
appear. That is exactly why this is the flow most likely to be silently broken.

## Driving it with control-cli

```bash
node $CC feed claude          # posts a UserPromptSubmit payload on stdin
node $CC feed codex           # writes a rollout JSONL the watcher tails
node $CC state --what buffer  # parsed events + a bySource histogram
node $CC state --what watchers
```

`feed codex` returns immediately; the watcher polls. Re-read the buffer for up
to ~12s before concluding nothing was captured (`qa-e2e.sh` loops 12x1s).

## Proves it works

`state --what buffer` shows an event with the **right `source`**:

| Origin | `source` |
|---|---|
| Claude Code | `claude-code` |
| Codex | `codex` |
| Cursor | `cursor` |
| The CLI itself | `cli` |

Verified in a sandbox: after `start` + `feed claude`,
`bySource == {"cli": 1, "claude-code": 1}`. The `cli` event is `start`'s own
`session_start` — on its own it proves **nothing** about editor capture.

`state --what watchers` must show the pid alive AND the state file present. A
watcher whose state file names a pid that is dead, or a pid belonging to another
session, is capturing nothing while looking alive — `cmd_status.go:codexCaptureStatus`
exists solely for that distinction.

## Gotchas

- **Exit 0 means nothing here.** The hook fails open by contract so it can never
  block an editor. It exits 0 when it drops the event too. Read the buffer.
- **`claude-code`, not `claude`.** `normalize.go:1286`. A case that changes it to
  `claude` produces a full-looking buffer that the backend cannot attribute.
- **macOS `/tmp` is a symlink to `/private/tmp`.** The codex watcher resolves the
  workspace through symlinks, so a rollout's `cwd` must be the **resolved** path
  or the "cwd within workspace" filter silently rejects every event. `feed` uses
  `fs.realpathSync`. Not a bug.
- **Codex rollout timestamps must be ≥ `session.StartedAt − 2min`.** Older
  rollouts are skipped so a candidate's unrelated prior codex sessions are never
  replayed. `feed codex` uses the current UTC time.
- **`claude-hook-takeover`** appears in the state dir when the hook emitted
  because the transcript watcher was not healthy. Its presence is normal right
  after `start`; a buffer that is empty *and* has no takeover marker means
  neither channel ran.
- **Events buffer locally because the API is unreachable.** That is the design:
  the hook buffers *before* it tries to POST, which is what makes an offline
  sandbox verifiable at all.
