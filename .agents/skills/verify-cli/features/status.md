# Status — session info and capture health

`promptster status` is the candidate's at-a-glance panel. Source: `cmd_status.go`.

## Sub-features

- Human box: Session ID, Started, Elapsed, Remaining.
- `Claude hooks:` row — only when there is really a file, or when Claude is
  instrumented and the file is **missing** (the case worth shouting about).
- `Codex:` row — watcher **ownership**, not just liveness.
- `Last explain:` row when a decision note was recorded.
- `Events:` and `Status:` from the API when reachable.
- `--json` for all of the above.

## How to get to it (user POV)

`promptster status`, or "4. Check status" from the menu.

## Driving it with control-cli

```bash
node $CC run status --json
node $CC run status              # the human box; check the rows
```

## Proves it works

Verified `--json` shape from a sandbox:

```json
{"elapsedSeconds":708,"remainingSeconds":4692,"sessionId":"sess-verify-claude",
 "startedAt":"2026-09-16T02:57:45Z","taskRoot":"…/ws","timeLimitMinutes":90}
```

`eventCount` and `status` are absent because the API is unreachable — correct,
not a failure.

The human box's conditional rows are the real assertion. On a claude session
before `start` has written hooks, the box reads
`Claude hooks: not written (expected …/ws/.claude/settings.local.json)` —
verified. After `start`, it must show the path instead.

## Gotchas

- **`Claude hooks:` is stat-based on purpose.** It used to print the intended
  path unconditionally, so a Codex-only session announced Claude wiring that
  never existed, and a failed hook write was indistinguishable from success
  (`cmd_status.go` carries the whole history). A row that always appears is the
  regression.
- **`Codex:` reports ownership.** A watcher left running by a *previous* session
  satisfies a liveness check while matching every rollout against the old
  workspace — capturing nothing, saying nothing. The row must distinguish
  "watching (pid N, M events sent)" from "pid N belongs to another session".
  That distinction is the whole point of the row.
- An old `session.json` may carry no `tools` array. The code consults the file
  first and the tool list only to decide whether an *absent* file is worth
  reporting.
