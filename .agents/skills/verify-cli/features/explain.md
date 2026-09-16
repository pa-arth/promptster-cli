# Explain — decision rationale

`promptster explain` lets a candidate record why behind recent work. Source:
`cmd_explain.go`, `decision_tui.go`, `decision_capture.go`, `decision_watcher.go`,
`explain_command.go`.

`promptster decide` is **retired**: it exits 1 pointing here (`cmd_decide.go`).

## Sub-features

- Look back over a window, propose decision candidates with an impact bar.
- A TUI prompt per candidate: `y` capture, `n` skip, `l` later, `q` quit.
- A multi-line input view for the rationale (`ctrl+d` submit, `esc` cancel).
- `.claude/commands/explain.md` — the slash command `start` installs, which
  invokes `explain --quiet`.
- Queued notes surface as an optional notice in `done`; they never block it.

## How to get to it (user POV)

`promptster explain`, `promptster explain --last 30m`, "3. Explain a decision"
from the menu, or `/explain` inside Claude Code.

Real flags (`cmd_explain.go:58`): `--last` (default `20m`), `--quiet`.

## Driving it with control-cli

```bash
node $CC run explain --last 30m --quiet
node $CC tui explain --timeout 12000 --evidence explain-screen.txt
```

## Proves it works

With no qualifying activity, `explain` reports it has nothing to ask about and
exits cleanly — that is a pass, not a failure.

With candidates present, the TUI screen shows the candidate summary and the
impact bar (`renderImpactBar`, `decision_tui.go:64`) and the `y/n/l/q` prompt.

The durable proof of a **capture** is a recorded decision note plus a
`Last explain:` row appearing in `promptster status`.

## Gotchas

- **The capture path cannot be verified with this harness.** Every branch beyond
  the first frame is behind a keystroke, and keystrokes do not reach the TUI
  (`decision_tui.go:256` uses the same `tea.WithInput(/dev/tty)` shape as the
  brief viewer). You can prove the prompt **renders**. You cannot prove `y`
  captures, `l` defers, or that the input view submits on `ctrl+d`. Say so.
- **`/explain` is optional by design.** Nothing prompts for it and `done` never
  blocks on queued notes. A verification that treats a missing note as a defect
  is a false finding.
- `--quiet` exists so the slash command keeps Claude's context clean; it
  suppresses the activity box and ANSI styling, so assert on text, not layout.
