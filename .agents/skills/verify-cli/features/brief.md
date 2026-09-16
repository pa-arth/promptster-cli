# Brief — the live task viewer

`promptster brief` (alias `task`) shows the assessment brief with a live
countdown. Source: `cmd_brief.go`, `brief_tui.go`, `brief.go`, `terminal_window.go`.

## Sub-features

- **Default**: opens the viewer in a **brand-new terminal window** so it sits
  next to the editor (`openBriefWindow`).
- `--here` / `--inline`: run the TUI in this terminal.
- `--plain`: print once, no interactivity. Also the automatic no-TTY path.
- `--json`: machine-readable.
- `--demo`: sample content, no session needed.
- Fallback chain: window → inline TUI → static print.
- `writeTaskFile` puts the same brief in `TASK.md` in the workspace.

## How to get to it (user POV)

`promptster brief`, or "2. View task brief" from the menu. `start` also tells
the candidate about it in its Next-steps block.

## Driving it with control-cli

```bash
node $CC run brief --json
node $CC run brief --plain
node $CC tui brief --here --timeout 10000 --evidence brief-screen.txt
```

Never run bare `brief` — it tries to spawn a real terminal window.

## Proves it works

`--json` returns `title`, `taskBrief`, `brief`, `timeLimitMinutes`, `workspace`,
`setupInstructions`, `remainingMinutes`. Verified: `remainingMinutes` is 78 on a
90-minute session started 12 minutes earlier — it is computed, not echoed.

The TUI screen, captured through the PTY, shows: the `▍PROMPTSTER · Assessment
Brief` header with the org name, a `⏳ … left` bar with a progress meter, a
`Tools:` label, `◆ THE SITUATION` and `◆ LOGISTICS` sections, the workspace
path, and the footer `↑/↓ scroll · g/G top/end · q close`.

The countdown ticking across frames is the proof it is live rather than a static
render — `qa-e2e.sh` settles for `brief | grep Tools`, which a frozen render
would also pass.

## Gotchas

- **Keystrokes do not reach this TUI.** `q`, `esc` and `ctrl+c` all do nothing
  under the PTY driver, in cooked and raw mode. The capture ends on the driver's
  timeout. Scrolling and `g`/`G` are therefore **unverifiable** here — report
  them as unverified, do not infer them from `brief_tui.go:411`.
- **Piped output is a different code path.** `stdoutIsTerminal()` false ⇒
  `printBriefStatic`. `run brief` verifies the static renderer; only `tui brief
  --here` verifies the viewer. Confusing the two is a false pass.
- `writeTaskFile` **never overwrites** an existing `TASK.md`. A stale TASK.md in
  an adopted checkout will not be refreshed.
