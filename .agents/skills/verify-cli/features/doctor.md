# Doctor — diagnose a broken setup

`promptster doctor` is the candidate's self-service diagnosis. Source:
`cmd_doctor.go` (~20k), `editor_extension_doctor.go`, `preflight.go`.

## Sub-features

- Header: CLI version, API URL, platform.
- `Environment` — git, and the binary for each **relevant** tool.
- `Session` — session id, expiry, workspace.
- `Claude hooks` — only when Claude is instrumented.
- `Codex` — only when codex is instrumented or a legacy proxy block is found.
- `Legacy cleanup` — a pre-1.10 provider block left in the global codex config.
- `Connectivity` — `GET <api>/v1/health`, then the session.
- Editor-extension status.

## How to get to it (user POV)

`promptster doctor`, or "6. Run doctor" from the no-argument menu. It is what a
candidate is told to run when capture looks wrong.

Takes **no flags** (`main.go` calls `cmdDoctor()` with no args).

## Driving it with control-cli

```bash
node $CC run doctor --timeout 20000 --evidence doctor.txt
```

Always pass a short `--timeout`. See the gotcha below.

## Proves it works

Tool-awareness is the assertion, and it is directional — `qa-e2e.sh` checks both
halves:

- A **claude** session: `claude binary` present AND a `Claude hooks` section present.
- A **codex** session: `codex binary` present AND **no** `Claude hooks` section.

A doctor that shows every section for every session is broken even though it
looks more informative. `cmd_status.go:claudeHooksStatus` documents the same
rule for `status`: a row is printed only when there is really a file at the path.

Connectivity failing against `127.0.0.1:9` is correct in the sandbox.

## Gotchas

- **`doctor` can hang forever.** `editor_extension_doctor.go:47` runs
  `cursor --list-extensions --show-versions` through `exec.Command(...).Output()`
  with **no timeout**. On this machine that call never returns; a `promptster
  doctor` was observed still running after 2m47s, and `scripts/qa-e2e.sh claude`
  stalled for over 3 minutes at the same point. Measured, not theorised.
  - Workaround used to get a clean harness run: put a `cursor` stub that exits 1
    first on `PATH`. With it, `qa-e2e.sh claude` completed with all 7 checks PASS.
  - control-cli's `run` applies a 60s default timeout and returns
    `{ok:false, …, stdoutSoFar}`, so the hang surfaces as a finding instead of a
    stalled agent.
  - **This is a real bug, not a test artifact.** A candidate with Cursor
    installed and a `doctor` that never returns has no way to diagnose anything.
- **Absent tools are not failures.** `doctor` only surfaces a tool's checks when
  the session instruments it. A missing section can be correct.
