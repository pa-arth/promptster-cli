# promptster-experiment — task envelope + assignment harness

Batch-0 task **0.1** of the openspec change `practice-effect-experiment`. This is
the blocker for batch 1 starting.

**Not shipped to candidates.** `make build` and the release cross-compile build the
root package only; this is a separate `package main` under `experiment/`, built by
`make experiment`. It exists for the internal fleet running batch 1.

## What it does

1. **`open` bounds a task.** The unit of analysis is the task (design.md), not the
   drift-session. `open` writes the envelope's open marker.
2. **Assignment happens at open, before work, and is immutable.** Re-opening the
   same task never re-randomizes. Rows append to a log with no update path.
3. **Only the assigned artifact is shown.** C1 prints the one-artifact contract,
   C2 prints the re-anchor notice, control prints neither. Silence is the control
   condition — leaking C1's contract into control erases the contrast.
4. **Hooks enforce C2 and record adherence.** Adherence is a separate stream and
   can never overwrite an assignment row.

## Install

```bash
make experiment                                    # builds bin/promptster-experiment
bin/promptster-experiment init --org <orgId>       # engineer defaults to git user.email
bin/promptster-experiment install-hooks            # prints the settings.json snippet
bin/promptster-experiment install-hooks --write ~/.claude/settings.json
```

`install-hooks` registers three Claude Code hook points and is idempotent (it drops
its own prior entries before appending, and backs the file up first).

## Use

```bash
# in the worktree where the task will be done
promptster-experiment open --task promptster-cli/task-envelope --class feature \
    --size m --title "the one artifact this session ships"

promptster-experiment status
promptster-experiment close --task promptster-cli/task-envelope --outcome merged --pr 123
promptster-experiment log             # assignments
promptster-experiment log --events    # adherence
promptster-experiment sync-payload    # POST bodies for the backend, once 0.3 lands
```

`--class ops` and `--short` (expected under 30 min) record an **exclusion row**
rather than an assignment — C1's eligibility line excludes pure-ops sessions, and
an exclusion nobody wrote down is an exclusion nobody can audit. Exclusions never
consume a randomization slot.

Kill switch: set `"enabled": false` in `~/.promptster-experiment/config.json` and
every hook returns silently without uninstalling anything.

## The allocator

Mirrors the backend allocator (openspec 0.3) byte-for-byte, so an offline draw and
a server draw of the same slot are the same arm:

```
allocationKey = (experimentKey, orgId, engineerUserId, stratum)   // within-engineer
stratum       = "<repo>|<taskClass>"                              // NOT size band
position      = count of prior ELIGIBLE rows in that key          // UNIQUE per key
block         = position / 4;  slot = position % 4
seed          = sha256(experimentKey ‖ 0x00 ‖ orgId ‖ 0x00 ‖ engineerUserId ‖ 0x00
                       ‖ stratum ‖ 0x00 ‖ decimal(block))
stream        = sha256(seed ‖ uint32be(i)) for i = 0,1,2,… concatenated, 4 bytes at a time
perm          = [c1off_c2off, c1on_c2off, c1off_c2on, c1on_c2on]  // declaration order
                Fisher-Yates: for j = n-1 down to 1: k = next4 mod (j+1); swap perm[j], perm[k]
arm           = perm[slot]
```

Two properties this buys over design.md's original `hash(engineerId, taskId,
weekBlock) → arm`, both agreed with the backend half after review:

**Permuted blocks, not a bare hash.** A bare hash is unstratified simple
randomization. Over batch 1's expected 40–60 tasks it routinely lands 18/8-style
splits across the four cells, and this batch has no power to spare. Blocks keep
everything the design is actually after — deterministic, assigned before work,
replayable, zero experimenter discretion — and add balance by construction. The
design's task fingerprint is still computed and stored as `taskHash`.

**Stratified on repo × task class only.** design.md's batch-1 section already says
this; repo × class × size would be 27 cells for ~50 tasks, leaving most strata with
a single task — worse for balance than not stratifying on size at all. `sizeBand`
is recorded on every row as an analysis covariate.

The position state is *the log itself*, so no counter can drift away from the rows.

> **Cross-implementation agreement is UNVERIFIED.** The vectors in
> `assign_test.go` are self-generated: they prove this implementation is stable,
> not that it matches the backend's. The backend half is publishing shared test
> vectors; assert against that file before batch 1's first real assignment.

## Assignment row — the wire contract with backend task 0.3

Rows append to `~/.promptster-experiment/assignments.jsonl`. `sync-payload` emits
the exact `POST /v1/teams/experiments/assignment` body:

```json
{
  "experimentKey": "batch1-context-mechanics",
  "taskKey": "promptster-cli/task-envelope",
  "weekBlock": "2026-W33",
  "repo": "pa-arth/promptster-cli",
  "taskClass": "feature",
  "sizeBand": "M",
  "cliVersion": "0.1.0",
  "assignmentSource": "cli-offline",
  "arm": "c1off_c2on",
  "stratumPosition": 0,
  "assignedAt": "2026-08-12T00:56:36.752361Z"
}
```

**One authority per row.** `assignmentSource: "cli-offline"` means this CLI drew
the arm and the server stores it **verbatim** — it does not recompute and does not
reconcile. A task the engineer already worked under arm X can therefore never
become arm Y: the server answers `409 arm_conflict` (or `409 slot_taken`) instead.
Offline draws are gated per-experiment by the registry's `allowOfflineDraw`, true
for batch 1 (we are the lab, append-only local log) and **false for batch 2 at
ops.ai** — a customer trial is server-authoritative or it is not evidence.

Local-only fields never leave the machine, notably `envelope.repoRoot`: it is a raw
absolute path, under the same no-raw-path contract as teams ingest. `taskKey` is
validated at `open` against the backend's regex, so a key that works offline cannot
be rejected at sync time after the work is already done.

Adherence is a **separate** append-only resource
(`POST /v1/teams/experiments/adherence`) and never writes to the assignment row:
`task_open`, `task_reopen`, `artifact_shown`, `compaction`, `gate_armed`,
`reanchor_accepted`, `reanchor_rejected`, `gate_bypassed`, `task_close`.

## Why the C2 gate lives on UserPromptSubmit

C2 says assigned sessions "block until a ≥200-char re-anchor brief is provided
post-compaction". Against Claude Code's actual hook contract:

| hook | can block? |
|---|---|
| `SessionStart` (`source: compact`) | **no** — exit 2 is ignored, the session proceeds |
| `PreCompact` | yes, but only blocks *compaction* — the wrong thing |
| `UserPromptSubmit` | **yes** — the only place the requirement can be honored |

So `PreCompact` and `SessionStart(source=compact)` *arm* the gate, and
`UserPromptSubmit` enforces it. Blocking uses `decision: "block"` with a reason
rather than exit 2, because exit 2 **erases the prompt** — destroying an engineer's
typing is the fastest way to make an assigned arm stop cooperating. A refused
prompt is also saved to `rejected/` and its path shown.

`!noanchor` as a prompt prefix releases the gate and records `gate_bypassed`. That
hole is deliberate: ITT requires non-compliance to be *recordable*, not impossible,
and a harness that can hard-lock a session gets uninstalled. Every hook also fails
open on any error.

## Compliance events (the automated half of design.md's six-field spec)

- **C1**: zero topic pivots is hand-read on the audit sample. The automated proxies
  (churn ratio, first-turn cache-write size) are computed in analysis, not here.
- **C2**: `reanchor_accepted` in the same session as `gate_armed`, carrying
  `charLen` and the attempt count — so "complied on the 3rd try" stays visible
  instead of rounding to compliance.
