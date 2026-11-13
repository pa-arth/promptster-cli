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

`make experiment` only **builds** the binary. It does not touch `~/.claude` — wiring
the hooks is an explicit step you run:

```bash
make experiment-install                            # build + copy to a stable path
EXP=~/.promptster-experiment/bin/promptster-experiment

$EXP init --org <orgId>                            # engineer defaults to git user.email
$EXP install-hooks                                 # prints the settings.json snippet
$EXP install-hooks --write ~/.claude/settings.json # or writes it, with a .bak first
# restart Claude Code sessions — hooks load at session start
```

Use `make experiment-install`, not `bin/promptster-experiment`, before wiring hooks:
`install-hooks` writes the **running binary's resolved path** into settings.json, so
installing from a worktree's `bin/` leaves Claude Code pointing at a path that
disappears when the worktree is removed — mid-batch, silently. A silently dead C2
gate reads as perfect non-adherence.

`install-hooks` registers three hook points (`SessionStart`, `PreCompact`,
`UserPromptSubmit`) and is idempotent: it drops its own prior entries before
appending, and backs the file up to `settings.json.promptster-experiment.bak`.
Verify with `$EXP status`.

## Use

```bash
# from anywhere — the envelope is global, see below
promptster-experiment open --task promptster-cli/task-envelope --class feature \
    --size m --title "the one artifact this session ships"

promptster-experiment status
promptster-experiment close --task promptster-cli/task-envelope --outcome merged --pr 123
promptster-experiment log             # assignments
promptster-experiment log --events    # adherence
promptster-experiment sync-payload    # POST bodies for the backend, once 0.3 lands
```

**One envelope is open at a time, for every checkout at once.** It used to be one
per repo root, on the theory that one worktree means one task. Batch 1 falsified
that in two days: every envelope was opened from a single control checkout while
the work ran in worktrees elsewhere, so they all collided on one key. Each `open`
silently destroyed the previous pointer — one task was orphaned 43 minutes in and
still reads "open" with no close — and the hooks, which resolve by hashing the
SESSION's cwd, found nothing for the sessions actually doing the work: no C1
contract, no C2 gate, treatment delivery keyed to the wrong thing entirely.

So `open` now refuses to overwrite an open envelope — atomically, via `O_EXCL`,
because a check followed by a write is not a guard when 5-10 sessions share one
state directory. Close it first, or hand over with `--supersede`, which records a
`task_superseded` event naming the orphan — the handover is allowed, its
disappearance is not. Closing a task by `--task` while a different one is open
leaves the open one alone.

Global does **not** mean every session is in the experiment. A hook is served
only when its cwd is a checkout of the task's declared repo, or the checkout the
envelope was opened from. Serving everyone would swap under-delivery for
over-attribution: an unrelated session compacting elsewhere on the machine would
record a `compaction` against the open task and, under C2, have its prompts
gated for work the envelope never covered.

Upgrading across this change adopts a pointer left by the old binary (the newest
one, if several) and clears the rest, so an envelope open at upgrade time does
not vanish into exactly the bug being fixed.

`--repo` still decides the stratum, and the stratum decides which permuted block
the arm is drawn from. When it disagrees with the checkout — the normal case when
dispatching from a control checkout — `open` says so and the row keeps both, as
`repo` and `envelope.openedInRepo`. All seven of batch 1's rows had that
disagreement and nothing recorded it.

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

**`open` refuses a task key whose repo segment contradicts the checkout.** The
stratum is repo × class, so the repo decides which permuted-block sequence the
arm comes from — and the log is append-only, so nothing about a wrong one can be
fixed afterwards. It went wrong once for real before the guard existed: a
promptster-backend task opened from the promptster-teams checkout drew from
`pa-arth/promptster-teams|feature` (batch-1 prereg amendment A2). It refuses
rather than warns, because by the time anyone reads the log it is too late, and
both escapes produce a correct row rather than silencing the check: pass
`--repo <owner>/<name>` for the repo the work really lands in, or use a task key
with no `/`, which claims no repo. Re-opening a task that already drew its arm is
never blocked — that draw is immutable, so refusing would cost work and protect
nothing.

**Cross-implementation agreement is VERIFIED** (2026-08-12). `vectors_test.go`
asserts this Go allocator against `testdata/experiment-block-vectors.json`, a
verbatim copy of the backend's
`apps/api/src/__tests__/__fixtures__/experiment-block-vectors.json`
(PR #699, branch `feat/experiment-assignment-log`): 3 allocation keys × 12
positions, all agreeing, plus a guard that the cell **declaration order** matches
— the subtlest way the two sides could diverge while every seed byte still agrees.

If that test goes red, stop assigning. It means the two halves disagree about what
arm an engineer was shown, which is the one failure an assignment log cannot
survive. Re-copy the fixture whenever the backend regenerates it.

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

## Sync — getting the log off this laptop

```
init --key PSE-…          store the engineer key (0600), or export PROMPTSTER_ENGINEER_KEY
sync --dry-run            show exactly what would be posted, post nothing
sync                      POST assignments, then the adherence derived from them
```

Until a row reaches the server, the only copy of batch 1's assignment log is one
JSONL file on one laptop. A pre-registered trial whose assignment record dies with
a disk is not a trial, so `sync` is the thing that makes "assignment preceded the
work" outlive this machine. It targets `POST /v1/teams/experiments/assignment` and
`/adherence` (backend #699), authenticated with the **engineer** key (`PSE-`) in
`X-API-Key` — a `PSO-` org capture key authenticates a *machine*, carries no
engineer identity, and is refused here by name rather than left to a generic 401.

**Sync state is not a flag on the row.** The assignment row is immutable, so its
`synced` field can never become true; it stays false forever and nothing reads it.
Receipts append to `sync-receipts.jsonl`, and "already synced" is derived from
them. That log is also what stops a second `sync` from double-posting adherence —
the adherence route is append-only with no idempotency key, and a double-posted
observation inflates the number the batch-2 gate is read off.

**A 4xx is a decision, not a hiccup.** `slot_taken`, `offline_draw_not_allowed`,
`experiment_closed`, a rejected body — retrying the same row cannot change any of
them, and mutating a row to get a different answer is the one thing that must
never happen, so they are recorded as `refused` and left alone. Only 5xx and
transport failures retry. An `arm_conflict` (or a 200 whose arm differs from the
local row, which the verbatim-storage contract says is impossible) prints a loud
block and stops: one authority per row, and reconciling by hand is the only
correct move.

**What the CLI is allowed to claim about adherence.** One observation per *armed
gate*, never per event:

| local events | posted |
|---|---|
| `gate_armed` → `reanchor_accepted` | `anchor_after_compact` = **followed** |
| `gate_armed` → `gate_bypassed` | `anchor_after_compact` = **violated** |
| `gate_armed`, unanswered, task closed | `anchor_after_compact` = **unknown** |
| `gate_armed`, unanswered, task still open | nothing yet — it can still be answered |
| `reanchor_rejected` | nothing, ever |

A rejected draft is a keystroke, not a verdict. Batch 1's first task rejected two
drafts before the third landed; posting the raw stream would have recorded three
violations against behaviour that was fully compliant, and adherence would have
read 40% for a 100% task. Dropping the unanswered-gate case would have been the
opposite error, quietly inflating the same number — hence `unknown`, which the
route treats as a first-class value.

**Everything here is scoped to the full assignment identity** (experiment, org,
task) rather than the task key, because the state directory outlives the
experiment: batch 2 runs under a different `experimentKey` against the same
`~/.promptster-experiment`, and a task key may legitimately repeat. Keyed on the
task alone, batch 1's receipt would mark batch 2's row already synced — silently,
with `status` reporting success — and a foreign accept, bypass or close would
resolve this batch's gates with another experiment's behaviour. For the same
reason a gate is only closed out by a `task_close` that comes **after** it armed:
a task can be closed and reopened, and yesterday's close must not answer today's
gate.

C1's `zero_topic_pivots` is **never** derived here. The pre-registration makes it
hand-read on every C1-arm task (regexes misfire on long machine notifications), so
it reaches the server from the audit pass with `source: hand-audit`.
