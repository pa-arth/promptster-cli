---
name: verify-cli
description: Drive the real promptster CLI (Go binary — start, hooks, watchers, doctor, brief, done) inside a throwaway sandbox and capture proof. Use whenever a change to promptster-cli needs to be shown working — "verify this", "prove it works", "did the capture actually fire", "QA this end to end" — or when reproducing a bug report against the real binary. `go test ./...` proves the units behave; this proves the product does.
---

# Verify Promptster CLI

Prove a change works by running the real binary the way a candidate does, and
capturing what it wrote. Everything runs through one control CLI. Every command
returns JSON; every failure returns `{ok:false, error, hint}` where `hint` says
what to do next.

```bash
CC=".agents/skills/verify-cli/control-cli.mjs"   # from the repo root
node $CC help
```

No npm install. Node stdlib plus `python3` (for the PTY) — both already on the
machine.

## What already exists — read this before adding anything

This repo is **not** starting from zero, and this skill deliberately does not
replace what is here:

- **`scripts/qa-e2e.sh`** is the repo's own end-to-end harness. It makes its own
  sandbox per tool, runs the real `start`, feeds a live event, checks `doctor`
  and `brief`, and tears down. It is the reference implementation of the
  isolation model and the source of every gotcha below.
- **`promptster-qa-e2e`** (a global skill, `~/.claude/skills/promptster-qa-e2e/`)
  documents that harness: when to run it, what it asserts, why it is safe.

Reach for them like this:

| You want | Use |
|---|---|
| A fast regression check over the whole candidate flow | `node $CC qa claude` (wraps `scripts/qa-e2e.sh`, returns its PASS/FAIL as JSON) |
| To drive ONE command and look at what it produced | `node $CC run …` / `node $CC state …` |
| To see a TUI's rendered screen | `node $CC tui …` |
| To know what a flow is supposed to do | [`features/README.md`](features/README.md) |

`qa` is a thin wrapper that parses the harness's output — it does not
reimplement its assertions. When an assertion in `qa-e2e.sh` needs to change,
change it there, not here.

## 1. Launch

There is no server to keep alive. Launch means: build the binary once, then make
a sandbox to run it in.

```bash
node $CC up --tools claude          # build + fresh sandbox + seeded session
node $CC up --tools codex
node $CC up --tools claude,codex
```

`up` prints the sandbox path, the binary sha, and the seeded session path. It is
ready when it returns `{"ok":true}`. It is finished when you run `down`.

The seeded session is what makes this runnable with **no PST key and no
network**: `consentAccepted=true` plus a token the unreachable API never
validates. Same shape `scripts/qa-e2e.sh` writes.

### Why this is safe — the isolation model

Every spawn goes through `envFor()` in `control-cli.mjs`, which forces all five
redirects. They cannot be overridden from the command line.

| Var | Points at | Replaces |
|-----|-----------|----------|
| `HOME` | `$SBX/home` | shell RC, `~/.promptster`, `~/.claude` |
| `PROMPTSTER_STATE_DIR` | `$SBX/state` | `session.json`, `buffer.jsonl`, watcher state |
| `PROMPTSTER_BUFFER_PATH` | `$SBX/state/buffer.jsonl` | the captured-event buffer |
| `CODEX_HOME` | `$SBX/codexhome` | codex `config.toml` + rollouts |
| `PROMPTSTER_API_URL` | `http://127.0.0.1:9` | the backend (unreachable ⇒ events buffer locally) |

**Never drive the real `~/.promptster/`.** A live candidate session on this
machine runs a `promptster` daemon from `~/.promptster/bin` with the same
process name as the sandbox's. `down` kills only processes whose command line
contains *this sandbox's path*; it never matches on the name. Do not run
`pkill -f promptster` — that is the one command in this whole skill that can
destroy someone's real capture. `doctor` lists foreign processes under
`checks.processes.foreign` precisely so you can see them and leave them alone.

## 2. Doctor

```bash
node $CC doctor
```

Run this first whenever anything looks off. It answers the only question that
matters before driving: **is this sandbox worth driving?** It exits non-zero
when it is not, and every failed check adds an actionable line to `hints`.

The checks exist because each one has a failure mode where a proof looks fine
and measures nothing:

- **`binaryCurrent`** — a `.go` file is newer than the binary in the sandbox.
  You would be verifying code that is not the code on disk. This is the single
  most common false pass in a Go repo: **edit, forget to rebuild, "confirm" the
  old behavior.** Run `node $CC build`.
- **`sandboxIsolated`** — all five redirects resolve inside the sandbox. If this
  is false, **stop**: a drive from here mutates the user's real install.
- **`realStateUnchanged`** — `~/.promptster`'s contents are byte-for-byte the
  mtimes recorded at `up`. If this flips, something escaped the sandbox; nothing
  you measured afterwards is trustworthy.
- **`binaryUnmodified`** — the sandbox binary still hashes to what `up` copied.
- **`tools`** — `claude` / `codex` installed. `start --tools X` will not keep a
  tool whose binary is absent, so its flow **cannot** be verified on this
  machine. Say that; do not report a pass.
- **`processes.foreign`** — promptster processes belonging to the real install.

## 3. Drive

Read [`features/README.md`](features/README.md) first. It maps every
user-facing command to its real flags, how a candidate reaches it, and the
observable end state that proves it works.

```bash
node $CC run status --json
node $CC run start --tools claude --workspace "$WS" --accept-tos --no-editor-extension --timeout 180000
node $CC run doctor --timeout 20000
node $CC feed claude                     # inject a live event the way the editor would
node $CC state --what buffer             # what was actually captured
node $CC state --what watchers           # are the daemons ours, and alive
node $CC state --what files              # everything start wrote, by tree
```

Flags that are not control flags pass through verbatim, so `run status --json`
sends `--json` to promptster. The control flags are a closed set:
`--timeout --stdin --evidence --keys --settle --what --prompt --tools --no-build`.

`--restart` is **refused**. Combined with non-interactive stdin it auto-kills a
running editor — the user's real Cursor or Claude window, not the sandbox's.

### TUIs: you can see them, you cannot drive them

```bash
node $CC tui brief --here --timeout 10000 --evidence brief-screen.txt
```

This gives you the **rendered screen**: a real PTY (`pty-drive.py`, python3
stdlib), `TERM=xterm-256color`, 100x40, raw mode, ANSI stripped, last repainted
frame selected.

**Keystrokes do not reach the TUI. This is a hard, measured limitation.**

- The PTY write path itself works — verified by driving `cat`, which echoed the
  text sent to it.
- `promptster brief --here` renders its full alt-screen viewer and its live
  countdown keeps ticking, but `q`, `esc` and `ctrl+c` all have no effect, in
  both cooked and raw mode. The process only ends on the driver's timeout.
- Root cause not isolated. Both TUIs (`brief_tui.go:440`, `decision_tui.go:256`)
  pass `tea.WithInput` an independent `os.OpenFile("/dev/tty")` handle; that is
  the obvious suspect but it has not been proven.

What this means for a verdict, and it matters:

- **Can be proven:** that a TUI launches, that it renders, and exactly what it
  renders — headers, the countdown, the footer keymap, the workspace path, the
  tool list.
- **Cannot be proven here:** anything behind a keystroke — scrolling, `g`/`G`,
  quitting, and every branch of `explain`'s y/n/l decision prompt.

If you need an interactive branch verified, say it is unverified and why. Do not
infer it from the source. An agent that reports "the explain TUI captures a
decision" from a screen it never advanced has produced exactly the false pass
this skill exists to prevent.

## 4. Evidence and proof standards

```bash
node $CC run doctor --evidence doctor.txt
node $CC tui brief --here --evidence brief-screen.txt
node $CC evidence          # list what has been captured
```

Evidence lands in `~/.promptster-verify/cli/evidence/` (override with
`PROMPTSTER_VERIFY_EVIDENCE`). Report the paths in your summary.

A proof that does not meet these is not evidence:

- **Run the real path.** `start` writes the hooks, spawns the watchers, and
  installs the binary. Hand-writing a `session.json` and checking `status` skips
  everything likely to be broken.
- **Check the side effect, not the exit code.** `promptster hook` exits 0 when
  it silently drops an event. `state --what buffer` is the proof; `exitCode: 0`
  is not.
- **An empty buffer is the silent-failure state, never a pass.** A watcher that
  exits without capturing leaves exit code 0, an empty `buffer.jsonl`, and a log
  line nobody reads. Always read `state --what watchers` alongside the buffer.
- **Check the `source`, not just the count.** An event captured with the wrong
  `source` lands in the buffer and looks like success. Claude Code events must
  be `claude-code` (not `claude`), codex must be `codex`, cursor must be
  `cursor`, and `cli` means the CLI emitted it itself — a `cli` event is **not**
  proof that editor capture works.
- **Confirm the binary is current** (`doctor` → `binaryCurrent`) before
  believing any result. Go does not rebuild on its own.

## 5. Cleanup

```bash
node $CC down
```

`down` kills only processes whose command line contains this sandbox's path,
then removes the sandbox. It reports `killed` (ours), `spared` (the real
install's — untouched), and re-checks `realStateUnchanged`.

It does **not** delete evidence. Screenshots, transcripts and harness logs stay
in `~/.promptster-verify/cli/evidence/`. A verification whose artifacts were
torn down with the sandbox proved nothing.

Always run `down`. `start` leaves background daemons (`claude-watch`,
`codex-watch`, `diff-watch`) running, and this machine is memory-constrained.

## 6. Helpers

| File | What it is |
|---|---|
| `control-cli.mjs` | Everything above. `node control-cli.mjs help` for the command list. |
| `pty-drive.py` | The PTY driver. `python3 pty-drive.py --keys q --timeout 15000 -- <cmd>`. Standalone and runnable. |
| `evals/run-eval.mjs` | Injects a real defect into a real `.go` file, rebuilds, runs a fresh agent with this skill, scores its verdict against planted ground truth. See [`evals/`](evals/). |
| `features/` | The feature map. |

`script(1)` is deliberately **not** used: BSD `script` calls `tcgetattr` on its
own stdin and dies with *"Operation not supported on socket"* whenever an agent
runs it non-interactively. `pty-drive.py` has no such requirement.

## 7. Keeping this skill honest

The feature map is only useful while it is true. When you drive a command and
find the map wrong — a renamed flag, a changed output line, a new gotcha — fix
the map file in the same change. A stale map costs more than no map, because an
agent trusts it.
