# Start — redeem, workspace, hooks, watchers

`promptster start` is the whole setup flow. It is the single most important
thing to verify, because everything downstream silently does nothing if it
half-ran. Source: `cmd_start.go` (~61k), `preflight.go`, `hooks.go`,
`proxy_config.go`, `seeded_start.go`.

## Sub-features

- Redeem a `PST-XXXX-XXXX` key, accept ToS, persist `session.json`.
- Choose the workspace: clone a repo, or adopt the checkout already present.
- Self-install the binary to `~/.promptster/bin/promptster`.
- Write the tool's instrumentation: `.claude/settings.local.json` (an
  `apiKeyHelper` pointing at `promptster auth-token`, plus hooks) for Claude;
  nothing global for Codex (see gotchas).
- Install the shell hook into the RC file, write the `active-workspace` pointer.
- Spawn the background daemons: `diff-watch` always, plus `claude-watch` and/or
  `codex-watch` for the selected tools.
- Write `TASK.md` and `.claude/commands/explain.md` into the workspace.
- Offer the editor extension (VS Code / Cursor).

## How to get to it (user POV)

A candidate runs `promptster start PST-ABCD-1234`, or picks "1. Start
assessment" from the no-argument interactive menu. A provisioned box uses
`--seeded`, where the worker already wrote the session and no key is needed.

Real flags (`cmd_start.go:37-86`) — note the last four are **not** in
`printUsage`:

```
--accept-tos   --workspace PATH   --restart   --verbose
--tools claude|codex|all|<comma list>
--adopt   --seeded   --no-editor-extension   --editor-extension
--byo-subscription   (retired; accepted and ignored)
```

## Driving it with control-cli

```bash
node $CC up --tools claude
WS=$(node $CC state --what session | python3 -c "import json,sys;print(json.load(sys.stdin)['session']['taskRoot'])")
node $CC run start --tools claude --workspace "$WS" --accept-tos \
     --no-editor-extension --timeout 180000 --evidence start.txt
node $CC state --what files
node $CC state --what watchers
```

`--no-editor-extension` keeps the run non-interactive and off the editor-CLI
path that hangs (see gotchas). `--accept-tos` and `--workspace` skip the two
interactive prompts. **Never pass `--restart`** — control-cli refuses it.

## Proves it works

Verified output from a real run in a sandbox:

- stdout ends with a `Next steps:` block naming the workspace — this is the
  string `qa-e2e.sh` asserts on, and the cheapest "did the flow complete" check.
- `state --what files` shows, under `ws/`: `.claude/settings.local.json` (~1.7k),
  `.claude/commands/explain.md`, `TASK.md`, `.gitignore`.
- under `home/`: `.promptster/bin/promptster`, `.promptster/active-workspace`,
  `.promptster/shell-hook.sh`, `.zshrc`.
- `state --what watchers` shows `claude-watcher` and `git-watcher` with live
  pids inside the sandbox.
- `state --what buffer` holds one `session_start` event with `source: "cli"`.

A run that prints `Next steps:` but leaves an empty `ws/.claude/` did **not**
work, whatever the exit code says.

## Gotchas

- **The proxy check fails by design in the sandbox.** Step 5/7 prints
  `Claude: could not reach proxy: … 127.0.0.1:9: connection refused`, and stderr
  carries `warning: device check failed`. `PROMPTSTER_API_URL` points at a dead
  port on purpose. Not a regression. Do not "fix" it.
- **`--restart` kills the user's real editor.** With non-interactive stdin it
  takes its own offer. Refused by control-cli; never add it back.
- **Codex's global config must stay untouched.** `start` writes no provider
  block into `$CODEX_HOME/config.toml`; the provider is passed per launch by
  `promptster codex`. A block there is the leak that broke every codex on the
  machine. `qa-e2e.sh` asserts its absence — keep it that way.
- **Only installed tools survive.** `start --tools codex` on a machine without
  the codex binary does not instrument codex. Check `doctor` → `checks.tools`
  before believing a tool-selection result.
- **`start` rewrites `startedAt`.** The deadline is `StartedAt + TimeLimitMinutes`
  (`time_limit.go:14`). A sandbox left idle long enough crosses it, and then the
  watcher auto-submits and tears the session down mid-verification — observed.
  Feed events promptly after `start`, or re-`up`.
