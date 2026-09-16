#!/usr/bin/env node
// run-eval — measure how good an agent actually is at verifying this CLI.
//
// The method: inject a real defect into a real .go file, REBUILD, hand a FRESH
// agent session the verify-cli skill and one instruction ("verify feature X"),
// and see whether it comes back FAIL. Control cases inject nothing and must come
// back PASS. Everything is scored against ground truth we planted, so the
// numbers are not self-reported.
//
// THE GO DIFFERENCE. The teams equivalent of this harness relies on Next's hot
// reload: edit a file, wait 4s, the running app is already broken. Nothing here
// reloads. A defect that is not compiled is not present, and an agent driving
// last build's binary scores a confident PASS on a case we thought we planted —
// which would silently measure nothing at all. So every injection is followed by
// a real `go build` here, and the run is ABORTED if that build fails. The agent
// then builds again inside its own sandbox; that second build is warm and cheap
// (measured: ~16s cold, ~4s warm on this repo).
//
// What it produces:
//   detection   — of the broken runs, how many the agent caught   (recall)
//   falseAlarm  — of the healthy runs, how many it wrongly failed (precision cost)
//   evidence    — how often it actually captured proof
//   costs       — wall time per case
//
// Usage:
//   node run-eval.mjs --agent claude
//   node run-eval.mjs --agent claude --case capture-wrong-source
//   node run-eval.mjs --validate-only        # check every anchor, run no agent
//   node run-eval.mjs --all-agents

import fs from "node:fs";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const SKILL = path.dirname(HERE);
const REPO = path.resolve(SKILL, "..", "..", "..");
const CASES = path.join(HERE, "cases");
const RESULTS = path.join(HERE, "results");
const CONTROL = path.join(SKILL, "control-cli.mjs");

const argv = process.argv.slice(2);

// An unrecognised flag used to fall through to a FULL run — which builds the
// repo and spawns real agent sessions. A typo should not cost ten minutes and a
// pile of nested agents, so unknown flags are a hard error. (Learned the hard
// way: `--validate` instead of `--validate-only` silently started a real run.)
const KNOWN_FLAGS = new Set(["validate", "validate-only", "agent", "all-agents", "case", "help"]);
for (const a of argv.filter((x) => x.startsWith("--"))) {
  const name = a.slice(2).split("=")[0];
  if (!KNOWN_FLAGS.has(name)) {
    console.error(JSON.stringify({
      ok: false,
      error: `unknown flag: --${name}`,
      hint: `Known flags: ${[...KNOWN_FLAGS].map((f) => "--" + f).join(", ")}. Refusing to start a full agent run on a typo.`,
    }, null, 2));
    process.exit(2);
  }
}
const flag = (n, d) => {
  const i = argv.indexOf(`--${n}`);
  if (i === -1) return d;
  const v = argv[i + 1];
  return !v || v.startsWith("--") ? true : v;
};

const AGENTS = {
  claude: (prompt) => ["claude", ["-p", prompt, "--permission-mode", "bypassPermissions"]],
  codex: (prompt) => ["codex", ["exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", prompt]],
  cursor: (prompt) => ["cursor-agent", ["-p", prompt, "--force"]],
};

const run = (cmd, args, opts = {}) =>
  new Promise((resolve) => {
    const t0 = Date.now();
    const child = spawn(cmd, args, { cwd: REPO, ...opts });
    let out = "";
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (out += d));
    child.on("error", (e) => resolve({ out: String(e.message), ms: Date.now() - t0, code: -1 }));
    child.on("close", (code) => resolve({ out, ms: Date.now() - t0, code }));
  });

// ------------------------------------------------------------ defect injection

// A killed runner must not leave a defect in the tree. This eval was itself
// killed mid-case once (a memory-pressured machine, several agent sessions), and
// it left normalize.go injected — a booby trap for the next person to build.
// So the pristine content is written to disk BEFORE the file is touched, and
// restored from there on exit, on a signal, and on the next startup.
const PENDING = path.join(HERE, ".eval-pending.json");

const LOCK = path.join(HERE, ".eval-lock");

/** Set once takeLock() succeeds. Recovery must never run without it. */
let holdsLock = false;

/**
 * One evaluator per checkout, enforced.
 *
 * A run EDITS source in the shared working tree to inject a defect, and startup
 * recovery treats any pending file as a crashed run. Two evaluators in the same
 * checkout therefore corrupt each other: the second restores the first's source
 * out from under it and deletes its marker, so the first builds and scores the
 * wrong tree, and a later crash can leave the defect in place permanently. This
 * repo is routinely worked by several sessions at once, so that is a matter of
 * course rather than bad luck.
 *
 * `wx` is atomic: whoever creates the file owns the tree. A lock whose owner is
 * gone is stale and is taken over, so a killed run cannot wedge the next one.
 */
function takeLock() {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      fs.writeFileSync(LOCK, JSON.stringify({ pid: process.pid, at: new Date().toISOString() }), { flag: "wx" });
      holdsLock = true;
      return;
    } catch (e) {
      if (e.code !== "EEXIST") throw e;
      let owner = null;
      try { owner = JSON.parse(fs.readFileSync(LOCK, "utf8")); } catch { /* unreadable => stale */ }
      const live = owner?.pid && (() => { try { process.kill(owner.pid, 0); return true; } catch { return false; } })();
      if (live) {
        console.error(
          `REFUSING to start: another evaluation (pid ${owner.pid}, since ${owner.at}) holds this checkout.\n` +
          `It injects defects into shared source, so two at once corrupt each other's tree.\n` +
          `Wait for it, or run this one against a separate worktree.`,
        );
        process.exit(2);
      }
      console.error(`Clearing a stale eval lock from pid ${owner?.pid ?? "?"} (no longer running).`);
      try { fs.unlinkSync(LOCK); } catch { /* someone else cleared it; retry */ }
    }
  }
  console.error("Could not take the eval lock after clearing a stale one. Remove .eval-lock by hand if nothing is running.");
  process.exit(2);
}

const releaseLock = () => { if (holdsLock) { holdsLock = false; try { fs.unlinkSync(LOCK); } catch { /* already gone */ } } };


function restorePending() {
  if (!fs.existsSync(PENDING)) return null;
  try {
    const p = JSON.parse(fs.readFileSync(PENDING, "utf8"));
    fs.writeFileSync(path.join(REPO, p.file), p.original);
    fs.rmSync(PENDING, { force: true });
    return p.file;
  } catch { return null; }
}

// Guarded by holdsLock: a --validate run never owns the tree, and must not
// restore a pending file belonging to the evaluator that does.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => { if (holdsLock) restorePending(); releaseLock(); process.exit(130); });
}
process.on("exit", () => { if (holdsLock) restorePending(); });

/**
 * Resolve a case's target path and prove it stays inside the repo.
 *
 * Case files declare `defect.file` as repo-relative, but nothing checked it. A
 * typo or a future case containing `../` would make --validate read an outside
 * file and make a real run OVERWRITE it — injection writes before it restores,
 * so a crash in between leaves someone else's file holding sabotaged content.
 * Every caller routes through here so none can be the one that forgets.
 */
function caseTarget(relative) {
  const file = path.resolve(REPO, relative);
  const base = path.resolve(REPO);
  if (!file.startsWith(base + path.sep))
    throw new Error(`case targets a path outside the repository: ${relative}`);
  return file;
}

function inject(defect) {
  const file = caseTarget(defect.file);
  if (!fs.existsSync(file)) throw new Error(`case targets a file that does not exist: ${defect.file}`);
  const original = fs.readFileSync(file, "utf8");
  const hits = original.split(defect.find).length - 1;
  if (hits !== 1) {
    throw new Error(
      `case anchor must match exactly once in ${defect.file}, matched ${hits}. ` +
      `The CLI changed under the case — fix the case, do not loosen the anchor.`
    );
  }
  fs.writeFileSync(PENDING, JSON.stringify({ file: defect.file, original }));
  fs.writeFileSync(file, original.replace(defect.find, defect.replace));
  return () => { fs.writeFileSync(file, original); fs.rmSync(PENDING, { force: true }); };
}

// A defect that does not compile is not a behavioural defect — it is a build
// error, a far easier and different bug. Every case must still build.
function goBuild() {
  const t0 = Date.now();
  const r = spawnSync("go", ["build", "-o", path.join(HERE, ".eval-build-probe"), "."], {
    cwd: REPO, encoding: "utf8", timeout: 300_000,
  });
  return { ok: r.status === 0, ms: Date.now() - t0, stderr: (r.stderr || "").slice(-2000) };
}

// ------------------------------------------------------------ verdict parsing

// The verdict is read from a FILE the agent writes, never scraped from its
// output. Scraping was actively wrong: these TUIs echo the prompt back, so the
// instruction lines "VERDICT: PASS / or / VERDICT: FAIL" appear in the
// transcript verbatim and the parser read MY OWN PROMPT as the agent's answer.
// That scored a confident verdict for runs where the agent sat at a permission
// dialog and executed nothing at all.
function readVerdict(verdictFile) {
  if (!fs.existsSync(verdictFile)) return { verdict: null, reason: "agent wrote no verdict file" };
  const raw = fs.readFileSync(verdictFile, "utf8").trim().toUpperCase();
  try { fs.unlinkSync(verdictFile); } catch { /* best effort */ }
  if (raw === "PASS" || raw === "FAIL") return { verdict: raw, reason: null };
  return { verdict: null, reason: `verdict file held ${JSON.stringify(raw.slice(0, 40))}, not PASS or FAIL` };
}

const sawEvidence = (out) => /promptster-verify|evidence\//.test(out);

// ------------------------------------------------------------ main

const PROMPT = (feature, verdictFile) => `Use the verify-cli skill to verify the "${feature}" feature of this CLI is working correctly right now.

Drive the real binary with .agents/skills/verify-cli/control-cli.mjs, in a sandbox. Do not read the Go source to decide your answer — decide from what the running binary actually does and what it wrote. Capture evidence.

Run \`down\` when you are finished.

When you have decided, write your verdict to this exact path, as a file whose
entire contents are one word, either PASS or FAIL:

${verdictFile}

For example: printf PASS > ${verdictFile}

PASS means the feature works as its feature-map file says it should. FAIL means it does not. Write the file as the last thing you do; a run with no file written is scored as no answer.`;

async function main() {
  fs.mkdirSync(RESULTS, { recursive: true });

  const only = flag("case");
  let cases = fs.readdirSync(CASES).filter((f) => f.endsWith(".json")).sort()
    .map((f) => JSON.parse(fs.readFileSync(path.join(CASES, f), "utf8")));
  if (typeof only === "string") cases = cases.filter((c) => c.id === only);
  if (!cases.length) { console.error("no cases matched"); process.exit(1); }

  // ---- anchor validation. Always, before anything is run or scored.
  const anchorProblems = [];
  for (const c of cases) {
    if (!c.defect) continue;
    let file;
    try { file = caseTarget(c.defect.file); }
    catch (e) { anchorProblems.push({ case: c.id, problem: String(e.message) }); continue; }
    if (!fs.existsSync(file)) { anchorProblems.push({ case: c.id, problem: `missing file ${c.defect.file}` }); continue; }
    const hits = fs.readFileSync(file, "utf8").split(c.defect.find).length - 1;
    if (hits !== 1) anchorProblems.push({ case: c.id, file: c.defect.file, matched: hits, problem: "anchor must match exactly once" });
  }
  if (anchorProblems.length) {
    console.error(JSON.stringify({ ok: false, error: "case anchors are stale", hint: "Fix the case files against the current source. Do not loosen an anchor to make it match — a two-hit anchor injects a defect somewhere you did not intend.", anchorProblems }, null, 2));
    process.exit(1);
  }
  if (flag("validate-only") || flag("validate")) {
    process.stdout.write(JSON.stringify({ ok: true, validated: cases.length, cases: cases.map((c) => c.id) }, null, 2) + "\n");
    return;
  }

  // Everything above only READS, so --validate needs no lock and stays usable in
  // CI while a run is in flight. Past this point the tree gets edited, so take
  // the lock BEFORE recovery — recovering without it would restore a live run's
  // source out from under it, which is the corruption this guards against.
  takeLock();
  process.on("exit", releaseLock);
  const stranded = restorePending();
  if (stranded) console.error(`restored ${stranded} — a previous run was killed mid-injection`);

  const agentNames = flag("all-agents") ? Object.keys(AGENTS) : [flag("agent", "claude")];
  for (const a of agentNames) if (!AGENTS[a]) { console.error(`unknown agent: ${a}. Known: ${Object.keys(AGENTS).join(", ")}`); process.exit(1); }

  // ---- fail fast rather than scoring an agent against a repo that does not build
  const base = goBuild();
  if (!base.ok) {
    console.error(JSON.stringify({ ok: false, error: "the repo does not build before any defect was injected", hint: "Fix the compile error first. Scoring an agent against a repo that does not build measures nothing.", stderr: base.stderr }, null, 2));
    process.exit(1);
  }
  console.error(`baseline build ok (${(base.ms / 1000).toFixed(0)}s)`);

  const runs = [];
  for (const agent of agentNames) {
    for (const c of cases) {
      let revert = () => {};
      try {
        if (c.defect) {
          revert = inject(c.defect);
          const b = goBuild();
          if (!b.ok) {
            revert();
            runs.push({ agent, case: c.id, error: "injected defect does not compile" });
            console.error(`ERR  ${agent}/${c.id}: does not compile — a build error is a different (easier) bug than the one this case means to plant`);
            continue;
          }
          console.error(`      rebuilt with defect (${(b.ms / 1000).toFixed(0)}s)`);
        }

        // Leave no sandbox from a previous case for this agent to inherit.
        await run("node", [CONTROL, "down"]);

        const verdictFile = path.join(HERE, `.verdict-${agent}-${c.id}.txt`);
        try { fs.unlinkSync(verdictFile); } catch { /* none from a previous run */ }
        const [cmd, args] = AGENTS[agent](PROMPT(c.feature, verdictFile));
        const res = await run(cmd, args);
        const { verdict, reason } = readVerdict(verdictFile);
        const correct = verdict === null ? null : verdict === c.expect;
        runs.push({
          agent, case: c.id, feature: c.feature, expect: c.expect, got: verdict,
          correct, invalid: reason, evidence: sawEvidence(res.out), ms: res.ms,
          detects: c.detects ?? null,
          transcriptTail: (reason || correct === false) ? String(res.out).slice(-6000) : undefined,
          exitCode: (reason || correct === false) ? res.code : undefined,
        });
        console.error(`${correct === true ? "HIT " : correct === false ? "MISS" : "INV "} ${agent}/${c.id} expect=${c.expect} got=${verdict ?? "-"} ${(res.ms / 1000).toFixed(0)}s`);
      } catch (e) {
        runs.push({ agent, case: c.id, error: String(e.message || e) });
        console.error(`ERR  ${agent}/${c.id}: ${e.message || e}`);
      } finally {
        revert();
        // Tear down whatever the agent left, then restore a clean binary so the
        // next case does not start from a poisoned build cache.
        await run("node", [CONTROL, "down"]);
        goBuild();
      }
    }
  }

  const score = (agent) => {
    const mine = runs.filter((r) => r.agent === agent && !r.error);
    const broken = mine.filter((r) => r.expect === "FAIL");
    const healthy = mine.filter((r) => r.expect === "PASS");
    const pct = (n, d) => (d ? Math.round((n / d) * 100) : null);
    return {
      agent,
      scored: mine.length,
      detection: { caught: broken.filter((r) => r.correct).length, of: broken.length, pct: pct(broken.filter((r) => r.correct).length, broken.length) },
      falseAlarm: { wrongFails: healthy.filter((r) => r.correct === false).length, of: healthy.length, pct: pct(healthy.filter((r) => r.correct === false).length, healthy.length) },
      invalidVerdicts: mine.filter((r) => r.invalid).length,
      evidenceCaptured: { n: mine.filter((r) => r.evidence).length, of: mine.length, pct: pct(mine.filter((r) => r.evidence).length, mine.length) },
      medianSeconds: mine.length ? Math.round(mine.map((r) => r.ms).sort((a, b) => a - b)[Math.floor(mine.length / 2)] / 1000) : null,
    };
  };

  const report = { ranAt: new Date().toISOString(), scores: agentNames.map(score), runs };
  const file = path.join(RESULTS, `${Date.now()}.json`);
  fs.writeFileSync(file, JSON.stringify(report, null, 2));
  fs.rmSync(path.join(HERE, ".eval-build-probe"), { force: true });
  process.stdout.write(JSON.stringify({ ...report, runs: undefined, reportFile: file }, null, 2) + "\n");
}

main().catch((e) => { console.error(e); process.exit(1); });
