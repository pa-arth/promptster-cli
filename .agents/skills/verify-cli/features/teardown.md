# Teardown — abort and reset

Two different recovery paths. Source: `cmd_cleanup.go`, `cmd_reset.go`,
`cleanup_lock.go`, `process_cleanup.go`, `shell_hook.go`.

## Sub-features

- **`promptster abort`** (alias `cleanup`) — discard the session, tear down
  hooks, kill the daemons. **No submit.** Flags: `--reason X` (default
  `manual`), `--verbose`.
- **`promptster reset`** — wipe local Promptster config to recover a broken
  setup. Flags: `--purge` (also remove `~/.promptster/bin`), `--verbose`.
- Both work when `session.json` is missing or corrupt — that is the case they
  exist for.
- `promptster env` (no args) is the shell hook's self-eviction trigger: on a new
  interactive shell, if the local `ExpiresAt` has passed it fires a background
  cleanup. `promptster env --clear` prints `unset` lines for legacy
  shell-exported proxy vars.

## How to get to it (user POV)

`promptster abort`, "7. Abort assessment" from the menu, or automatically via
the shell hook's TTL self-eviction on the next terminal.

## Driving it with control-cli

```bash
node $CC run abort --reason verification --verbose --evidence abort.txt
node $CC state --what files
node $CC state --what watchers
node $CC run env --clear
```

## Proves it works

After `abort`: `session.json` is gone, `home/.promptster/active-workspace` is
gone, the RC line and shell hook are removed, and `state --what watchers` shows
no live pids. Then `status` must fail with "no session".

**The workspace itself must survive.** `abort` never tears the workspace down
(`cmd_cleanup.go:37-46`): it is the path where nothing was uploaded, so the box
holds the only copy of the candidate's work, and one of its three callers is an
unattended TTL self-eviction. A teardown that removed the workspace would
destroy a running candidate's machine. **If a change makes `abort` remove the
workspace, that is the bug, not the test.**

`env --clear` prints exactly:

```
unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL 2>/dev/null || true
```

Idempotent, safe to `eval`, a no-op for shells that never had them.

## Gotchas

- `--delete-codespace` is **gone** along with the host. Box lifecycle belongs to
  the provisioner, not the CLI inside it.
- The proxy is wired through Claude Code's `apiKeyHelper`, **not** shell env
  vars, since 1.2.0. `env --clear` only cleans up pre-1.2.0 leftovers; its being
  a no-op is success.
- The `apiKeyHelper` lives in `<workspace>/.claude/settings.local.json` and
  applies only when Claude Code runs with that workspace as its project root — so
  a `claude` outside the workspace uses the user's own auth. Inside it, the
  helper out-ranks a logged-in subscription.
