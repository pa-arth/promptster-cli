# Done — submit the assessment

`promptster done` ends the session and ships the work. Source: `cmd_done.go`,
`code_submit.go`, `code_bundle.go`, `code_snapshot.go`, `stranded_work.go`,
`signing.go`.

## Sub-features

- Optional notice when decision notes are queued (suppressed by `--auto`).
- Bundle the workspace code, detect work stranded outside the bundle, submit.
- Flush the local event buffer, sign and close the event log.
- Tear down local state (`cleanupPromptsterState`).
- Print the submitted confirmation box.

## How to get to it (user POV)

`promptster done`, or "5. Submit assessment" from the menu. `start`'s
Next-steps block names it. The time-limit auto-submit path spawns
`promptster done --auto` on the candidate's behalf.

Real flag (`cmd_done.go:17`): `--auto`.

## Driving it with control-cli

```bash
node $CC run done --auto --timeout 120000 --evidence done.txt
node $CC state --what files     # what survived teardown
```

## Proves it works

**This flow cannot be fully verified in the sandbox.** Submission needs a
reachable backend and `PROMPTSTER_API_URL` points at a dead port by design. What
you can verify locally:

- the queued-decision notice appears without `--auto` and is suppressed with it;
- bundling and stranded-work detection run and report;
- the submit attempt fails against the unreachable API with a clear error;
- local teardown behaviour after the attempt.

Verifying an actual successful submit needs a real backend. Say that explicitly
rather than reporting a pass from a sandbox run.

## Gotchas

- **`--auto` is not cosmetic.** It suppresses the interactive notice *and* tells
  `submitWorkspaceCode` to submit rather than abort when work is stranded outside
  the bundle. Aborting there would strand the session open forever. Do not treat
  `--auto` as merely "non-interactive".
- **`done` deletes the session.** After it runs, `status`, `brief` and `explain`
  all fail with "no session" — that is correct, not a regression.
- The expiry notice lives in the **global** dir on purpose, because `done` wipes
  the workspace state dir and the notice has to outlive that
  (`time_limit.go`).
