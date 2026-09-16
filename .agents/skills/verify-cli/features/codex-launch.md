# Codex launch — codex wired to the proxy, for one process

`promptster codex [args...]` launches codex against this session's proxy for
exactly that one process. Source: `cmd_codex.go`, `codex_proxy.go`,
`cmd_codex_unix.go`, `cmd_codex_windows.go`.

## Sub-features

- Select the custom model provider via `-c` overrides on the launch.
- Put the session credential in that process's environment only.
- Purge a legacy managed block from the global `~/.codex/config.toml`
  (`purgeLegacyCodexProxyBlock`), healing machines that ran CLI ≤1.9.
- Refuse, with a named fix, when there is no session / no token / expired / codex
  was not selected for this session.
- Forward every argument to codex verbatim — no flag parsing.

## How to get to it (user POV)

A candidate on a codex assessment runs `promptster codex` instead of `codex`.

## Driving it with control-cli

```bash
node $CC up --tools codex
node $CC run codex --help --timeout 30000
node $CC state --what files     # confirm codexhome/config.toml is untouched
```

Refusal paths are the cheap, reliable assertions:

```bash
node $CC up --tools claude      # a session WITHOUT codex
node $CC run codex --help       # must refuse, naming the fix
```

## Proves it works

- On a claude-only session, the launch is **refused** with a message naming what
  to run instead. Refusing is correct: a session started without codex has no
  consent to meter codex traffic against the hiring team's key and its rollout
  watcher is not running, so the run would be billed to somebody and captured by
  nobody.
- `$CODEX_HOME/config.toml` either does not exist or contains no `promptster`
  string, **before and after**. `qa-e2e.sh` asserts exactly this.
- On a codex session with codex installed, the process launches with the
  provider override and the credential in its environment only.

## Gotchas

- **The global codex config is the regression to watch.** Writing the provider
  there hijacked every codex on the machine and survived any teardown that did
  not run. The whole design is "per launch, dies with the process". A
  `promptster` string appearing in `config.toml` is a serious regression even
  though nothing visibly breaks in the run that caused it.
- Needs the real `codex` binary. Absent ⇒ unverifiable here; say so.
- Capture for codex is the **rollout watcher**, not this command. Launching
  proves wiring, not capture. See [capture.md](capture.md).
