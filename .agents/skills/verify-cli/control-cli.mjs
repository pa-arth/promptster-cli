#!/usr/bin/env node
// control-cli — drive the REAL promptster CLI the way a candidate does, inside a
// sandbox, and capture proof.
//
// Every command prints one JSON object on stdout. Success exits 0; failure exits
// non-zero with {ok:false, error, hint} where `hint` says what to do next.
//
// No npm dependencies. Node stdlib plus `script(1)` for the PTY.
//
// SESSION MODEL. Unlike a web app there is no server to keep alive. `up` makes a
// sandbox: a throwaway directory tree plus a copy of the binary you just built.
// Every later command re-reads that sandbox from state.json, so `run` then
// `state` in two separate shell calls see the same files. `down` removes it.
//
// SAFETY — read this before changing anything below.
// The whole point of the sandbox is that the real machine is never mutated:
//   HOME                   -> $SBX/home       (shell RC, ~/.promptster, ~/.claude)
//   PROMPTSTER_STATE_DIR   -> $SBX/state      (session.json, buffer.jsonl, watchers)
//   PROMPTSTER_BUFFER_PATH -> $SBX/state/buffer.jsonl
//   CODEX_HOME             -> $SBX/codexhome  (codex config.toml + rollouts)
//   PROMPTSTER_API_URL     -> http://127.0.0.1:9   (unreachable ⇒ events buffer locally)
// Those five are applied by envFor() on EVERY spawn and cannot be overridden from
// the command line. `doctor` re-asserts them and refuses a sandbox that leaks.
//
// Cleanup kills by recorded pid and by the SANDBOX binary path only. It never
// pkills "promptster": a live candidate session on this machine runs the same
// process name from ~/.promptster/bin and killing it destroys real capture.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync, spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "..", "..", ".."); // .agents/skills/verify-cli -> repo root
const VHOME = process.env.PROMPTSTER_VERIFY_HOME || path.join(os.homedir(), ".promptster-verify", "cli");
const STATE = path.join(VHOME, "state.json");
const EVIDENCE = process.env.PROMPTSTER_VERIFY_EVIDENCE || path.join(VHOME, "evidence");
const BINDIR = path.join(VHOME, "bin");

// The real, user-owned Promptster install. Nothing here is ever written or killed.
const REAL_PROMPTSTER = path.join(os.homedir(), ".promptster");

const DEFAULT_TIMEOUT_MS = Number(process.env.PROMPTSTER_VERIFY_TIMEOUT_MS || 60_000);

const out = (o) => { process.stdout.write(JSON.stringify({ ok: true, ...o }, null, 2) + "\n"); };
const die = (error, hint, extra = {}) => {
  process.stdout.write(JSON.stringify({ ok: false, error, hint, ...extra }, null, 2) + "\n");
  process.exit(1);
};

// ---------------------------------------------------------------- args

// Control flags are a CLOSED set, deliberately. `run status --json` must send
// --json to promptster, not consume it here; a generic parser silently ate it
// and the run "passed" against the human-readable output instead. Anything not
// on this list is passed through verbatim to the CLI under test.
const VALUE_FLAGS = new Set(["timeout", "stdin", "evidence", "keys", "settle", "what", "prompt", "tools"]);
const BOOL_FLAGS = new Set(["no-build", "help"]);

const argv = process.argv.slice(2);
const cmd = argv[0];
const flags = {};
const positional = [];
for (let i = 1; i < argv.length; i++) {
  const a = argv[i];
  if (a === "--") { positional.push(...argv.slice(i + 1)); break; }
  if (a.startsWith("--")) {
    const eq = a.indexOf("=");
    const name = eq > -1 ? a.slice(2, eq) : a.slice(2);
    if (VALUE_FLAGS.has(name)) {
      flags[name] = eq > -1 ? a.slice(eq + 1) : argv[++i];
      continue;
    }
    if (BOOL_FLAGS.has(name)) { flags[name] = true; continue; }
    positional.push(a); // not ours — the CLI under test gets it
    continue;
  }
  positional.push(a);
}

// ---------------------------------------------------------------- state

const readState = () => {
  if (!fs.existsSync(STATE)) return null;
  try { return JSON.parse(fs.readFileSync(STATE, "utf8")); } catch { return null; }
};
const writeState = (s) => {
  fs.mkdirSync(VHOME, { recursive: true });
  fs.writeFileSync(STATE, JSON.stringify(s, null, 2));
};
const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };

function requireSandbox() {
  const st = readState();
  if (!st?.sbx) die("no sandbox", "Run `control-cli.mjs up` first. Nothing runs outside a sandbox — driving the real CLI would mutate ~/.promptster.");
  if (!fs.existsSync(st.sbx)) die("the recorded sandbox is gone", "Run `up` again. Something removed " + st.sbx, { state: st });
  return st;
}

// ---------------------------------------------------------------- the isolation contract

// envFor is the ONLY place the child environment is built. Every redirect is
// forced here, after the caller's extras, so nothing can opt out of isolation.
function envFor(st, extra = {}) {
  return {
    ...process.env,
    ...extra,
    HOME: path.join(st.sbx, "home"),
    SHELL: "/bin/zsh",
    PROMPTSTER_STATE_DIR: path.join(st.sbx, "state"),
    PROMPTSTER_BUFFER_PATH: path.join(st.sbx, "state", "buffer.jsonl"),
    CODEX_HOME: path.join(st.sbx, "codexhome"),
    PROMPTSTER_API_URL: st.apiUrl,
  };
}

// `promptster start --restart` offers to kill running editors. Under a
// non-interactive stdin it takes the offer, and the editor it kills is the
// user's real Cursor/Claude window. There is no verification this flag enables.
const FORBIDDEN_ARGS = new Set(["--restart"]);

function guardArgs(args) {
  for (const a of args) {
    if (FORBIDDEN_ARGS.has(a)) {
      die(`refusing to pass ${a}`,
        "--restart kills running editors, and with non-interactive stdin it does so without asking. That is the user's real editor window, not the sandbox's. Drop the flag.");
    }
  }
}

// ---------------------------------------------------------------- build

const goSources = () =>
  fs.readdirSync(REPO).filter((f) => f.endsWith(".go")).map((f) => path.join(REPO, f));

const newestSourceMtime = () =>
  goSources().reduce((m, f) => Math.max(m, fs.statSync(f).mtimeMs), 0);

const sha = (file) => createHash("sha256").update(fs.readFileSync(file)).digest("hex").slice(0, 16);

function build() {
  fs.mkdirSync(BINDIR, { recursive: true });
  const bin = path.join(BINDIR, "promptster");
  const t0 = Date.now();
  const r = spawnSync("go", ["build", "-o", bin, "."], { cwd: REPO, encoding: "utf8" });
  if (r.status !== 0) {
    die("go build failed", "Fix the compile error before verifying anything. A stale binary from a previous build is still on disk and driving it would prove nothing about the current source.",
      { stderr: (r.stderr || "").slice(-4000), repo: REPO });
  }
  return { bin, buildMs: Date.now() - t0, sha: sha(bin), builtFrom: REPO, sourceMtime: newestSourceMtime() };
}

// ---------------------------------------------------------------- running

function runSync(st, args, { timeoutMs = DEFAULT_TIMEOUT_MS, stdin = "", cwd = null } = {}) {
  guardArgs(args);
  const t0 = Date.now();
  const r = spawnSync(st.bin, args, {
    env: envFor(st),
    input: stdin,
    encoding: "utf8",
    timeout: timeoutMs,
    killSignal: "SIGKILL",
    cwd: cwd || path.join(st.sbx, "ws"),
  });
  return {
    argv: args,
    exitCode: r.status,
    signal: r.signal,
    timedOut: r.error?.code === "ETIMEDOUT" || r.signal === "SIGKILL",
    ms: Date.now() - t0,
    stdout: r.stdout || "",
    stderr: r.stderr || "",
  };
}

// ---------------------------------------------------------------- PTY

const stripAnsi = (s) =>
  s.replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, "")   // OSC
   .replace(/\x1b[@-Z\\-_]/g, "")                      // single-char escapes
   .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")            // CSI
   .replace(/\r/g, "");

// ponytail: frame-splitting, not terminal emulation. Bubbletea repaints the
// alt-screen, so the raw capture holds every frame ever drawn. We return the
// last one, which is what a human would be looking at. It is wrong for a TUI
// that paints incrementally without a clear — none of this repo's TUIs do
// (brief_tui.go and decision_tui.go are both full-repaint bubbletea models).
// Upgrade path if that changes: run the capture through a real vt100 emulator.
function lastFrame(raw) {
  const frames = raw.split(/\x1b\[(?:2J|H\x1b\[2J|3J)/);
  const pick = frames.reverse().find((f) => stripAnsi(f).trim().length > 0) ?? raw;
  return stripAnsi(pick).split("\n").map((l) => l.replace(/\s+$/, "")).join("\n").trim();
}

// ptyRun drives a command under a real PTY so TUIs believe they have a terminal.
// `script` is the stdlib answer here: no node-pty, no native build step.
function ptyRun(st, args, { keys = [], timeoutMs = 15_000, settleMs = 1200 }) {
  return new Promise((resolve) => {
    guardArgs(args);
    const quoted = [st.bin, ...args].map((a) => `'${String(a).replace(/'/g, `'\\''`)}'`).join(" ");
    const [c, a] = process.platform === "darwin"
      ? ["script", ["-q", "/dev/null", "/bin/sh", "-c", quoted]]   // BSD
      : ["script", ["-qfec", quoted, "/dev/null"]];                // util-linux

    const t0 = Date.now();
    let raw = "";
    let done = false;
    const child = spawn(c, a, { env: envFor(st), cwd: path.join(st.sbx, "ws"), stdio: ["pipe", "pipe", "pipe"] });
    child.stdout.on("data", (d) => (raw += d.toString("utf8")));
    child.stderr.on("data", (d) => (raw += d.toString("utf8")));

    const finish = (why) => {
      if (done) return;
      done = true;
      clearTimeout(hard);
      try { child.kill("SIGKILL"); } catch {}
      resolve({ argv: args, raw, screen: lastFrame(raw), ms: Date.now() - t0, endedBy: why });
    };
    const hard = setTimeout(() => finish("timeout"), timeoutMs);
    child.on("close", () => finish("exit"));
    child.on("error", (e) => { raw += `\n[spawn error] ${e.message}\n`; finish("error"); });

    // Let the first frame paint, then send keystrokes with a beat between them.
    setTimeout(async () => {
      for (const k of keys) {
        if (done) break;
        try { child.stdin.write(k === "\\n" ? "\n" : k); } catch {}
        await new Promise((r) => setTimeout(r, 300));
      }
    }, settleMs);
  });
}

// ---------------------------------------------------------------- evidence

function saveEvidence(name, body) {
  fs.mkdirSync(EVIDENCE, { recursive: true });
  const file = path.join(EVIDENCE, `${Date.now()}-${name.replace(/[^\w.-]/g, "_")}`);
  fs.writeFileSync(file, body);
  return file;
}

// ---------------------------------------------------------------- doctor helpers

const which = (b) => {
  const r = spawnSync("command", ["-v", b], { shell: true, encoding: "utf8" });
  return r.status === 0 ? r.stdout.trim().split("\n")[0] : null;
};

// Every promptster process on this machine, split into ours and NOT ours.
// "not ours" is the near-miss: a candidate's live capture daemon runs the same
// name from ~/.promptster/bin. It is reported so it is visible, and never killed.
function promptsterProcesses(st) {
  const r = spawnSync("ps", ["-ax", "-o", "pid=,command="], { encoding: "utf8" });
  const lines = (r.stdout || "").split("\n").filter((l) => /promptster/.test(l));
  const ours = [], foreign = [];
  for (const l of lines) {
    const m = l.trim().match(/^(\d+)\s+(.*)$/);
    if (!m) continue;
    const rec = { pid: Number(m[1]), command: m[2] };
    if (/control-cli\.mjs|\bps\b/.test(rec.command)) continue;
    if (rec.command.includes(VHOME)) continue; // our own verification tooling, not the app
    if (st?.sbx && rec.command.includes(st.sbx)) ours.push(rec);
    // The real install's daemons run from ~/.promptster/bin/promptster. Match
    // that exact executable, not the directory: ~/.promptster-verify shares the
    // prefix and a substring match swept in unrelated processes.
    else if (rec.command.includes(path.join(REAL_PROMPTSTER, "bin", "promptster"))) foreign.push(rec);
  }
  return { ours, foreign };
}

const realStateFingerprint = () => {
  try {
    const entries = fs.readdirSync(REAL_PROMPTSTER).sort();
    return entries.map((e) => {
      const s = fs.statSync(path.join(REAL_PROMPTSTER, e));
      return `${e}:${Math.round(s.mtimeMs)}`;
    }).join("|");
  } catch { return "absent"; }
};

// ---------------------------------------------------------------- session seeding

// The crafted session is what makes this runnable with no key and no network:
// consentAccepted=true and a token the unreachable API never validates. Same
// shape scripts/qa-e2e.sh writes — keep them in step.
function seedSession(st, { tools, timeLimitMinutes = 90, startedAt = new Date().toISOString() }) {
  const ws = fs.realpathSync(path.join(st.sbx, "ws"));
  const session = {
    sessionId: `sess-verify-${tools.join("-")}`,
    sessionToken: "tok-verify",
    key: "PST-VERIFY",
    assessmentTitle: "Verification sandbox",
    orgName: "Acme",
    taskBrief: "Do the task.",
    taskRoot: ws,
    timeLimitMinutes,
    startedAt,
    consentAccepted: true,
    allowedTools: tools,
    tools,
    apiUrl: st.apiUrl,
  };
  fs.writeFileSync(path.join(st.sbx, "state", "session.json"), JSON.stringify(session, null, 2), { mode: 0o600 });
  return session;
}

// ---------------------------------------------------------------- main

const HELP = `control-cli — drive the real promptster CLI in a sandbox, and prove what it did.

  up [--tools claude|codex|claude,codex] [--no-build]
                        Build the binary and make a fresh sandbox. Prints the paths.
  doctor                Is this sandbox worth driving? Build currency, isolation,
                        installed tools, foreign promptster processes. Run it first.
  build                 Rebuild the binary into the sandbox. Use after editing Go.
  run <sub> [args...]   Run a promptster subcommand in the sandbox.
                        --stdin TEXT  --timeout MS  --evidence NAME
  tui <sub> [args...]   Drive a TUI through a PTY; returns the rendered screen.
                        --keys "q"  --timeout MS  --settle MS  --evidence NAME
  feed claude|codex     Inject one realistic live event for that tool, the way the
                        editor would, so capture can be proven end to end.
  state [--what session|buffer|watchers|files|all]
                        Read back what the CLI wrote. Buffer events are parsed.
  qa [claude|codex|all] Run the repo's own scripts/qa-e2e.sh and return its
                        PASS/FAIL checks as JSON.
  evidence              List captured evidence files (these survive cleanup).
  down                  Kill only what this sandbox started, remove the sandbox.
                        Evidence is kept.
  help

Isolation is not optional and is applied to every spawn:
  HOME, PROMPTSTER_STATE_DIR, PROMPTSTER_BUFFER_PATH, CODEX_HOME are redirected
  into the sandbox and PROMPTSTER_API_URL points at an unreachable port.
`;

async function main() {
  switch (cmd) {
    case undefined:
    case "help":
    case "--help":
      process.stdout.write(HELP);
      return;

    // ------------------------------------------------------------------ up
    case "up": {
      const prev = readState();
      if (prev?.sbx && fs.existsSync(prev.sbx)) {
        die("a sandbox is already up", "Run `down` first, then `up`. Two sandboxes at once means the next command is ambiguous about which one it drove.", { existing: prev.sbx });
      }
      const tools = String(flags.tools || "claude").split(",").map((s) => s.trim()).filter(Boolean);
      const known = ["claude", "codex"];
      for (const t of tools) if (!known.includes(t)) die(`unknown tool: ${t}`, `Instrumented tools are ${known.join(", ")}. Cursor was retired as a selectable tool (see cmd_doctor.go).`);

      const built = flags["no-build"] ? null : build();
      const binSrc = built?.bin || path.join(BINDIR, "promptster");
      if (!fs.existsSync(binSrc)) die("no binary to run", "Run `up` without --no-build, or `build`.");

      const sbx = fs.mkdtempSync(path.join(os.tmpdir(), "pst-verify-"));
      for (const d of ["home", "state", "ws", "codexhome"]) fs.mkdirSync(path.join(sbx, d), { recursive: true });
      const bin = path.join(sbx, "promptster");
      fs.copyFileSync(binSrc, bin);
      fs.chmodSync(bin, 0o755);

      const st = {
        sbx, bin, tools,
        apiUrl: "http://127.0.0.1:9",
        startedAt: new Date().toISOString(),
        binSha: sha(bin),
        sourceMtimeAtBuild: built?.sourceMtime ?? newestSourceMtime(),
        realStateFingerprint: realStateFingerprint(),
        spawned: [],
      };
      writeState(st);
      const session = seedSession(st, { tools });
      out({
        sandbox: sbx, binary: bin, binSha: st.binSha, tools,
        buildMs: built?.buildMs ?? null,
        session: { path: path.join(sbx, "state", "session.json"), taskRoot: session.taskRoot },
        evidence: EVIDENCE,
        next: "control-cli.mjs doctor",
      });
      return;
    }

    // -------------------------------------------------------------- doctor
    case "doctor": {
      const st = readState();
      const hints = [];
      const checks = {};

      checks.sandboxExists = Boolean(st?.sbx && fs.existsSync(st.sbx));
      if (!checks.sandboxExists) hints.push("No sandbox. Run `up`.");

      // 1. Are we driving the binary we think we are?
      const srcMtime = newestSourceMtime();
      checks.binaryExists = Boolean(st?.bin && fs.existsSync(st.bin));
      checks.binaryCurrent = checks.binaryExists && srcMtime <= (st.sourceMtimeAtBuild ?? 0);
      if (checks.binaryExists && !checks.binaryCurrent) {
        hints.push("A .go file is newer than the binary in the sandbox. You are about to verify code that is not the code on disk — run `build`.");
      }
      checks.binaryUnmodified = checks.binaryExists && sha(st.bin) === st.binSha;
      if (checks.binaryExists && !checks.binaryUnmodified) hints.push("The sandbox binary changed underneath us. Run `down` then `up`.");

      // 2. Is the isolation actually in force?
      const env = st ? envFor(st) : {};
      const under = (p) => Boolean(st?.sbx && p && path.resolve(p).startsWith(path.resolve(st.sbx)));
      checks.isolation = st ? {
        HOME: under(env.HOME),
        PROMPTSTER_STATE_DIR: under(env.PROMPTSTER_STATE_DIR),
        PROMPTSTER_BUFFER_PATH: under(env.PROMPTSTER_BUFFER_PATH),
        CODEX_HOME: under(env.CODEX_HOME),
        apiUnreachable: env.PROMPTSTER_API_URL === "http://127.0.0.1:9",
      } : null;
      checks.sandboxIsolated = Boolean(checks.isolation && Object.values(checks.isolation).every(Boolean));
      if (st && !checks.sandboxIsolated) {
        hints.push("Isolation is NOT in force — a redirect points outside the sandbox. Do not run anything. This is the state where a drive mutates the user's real ~/.promptster.");
      }

      // 3. Has the real install been touched? (the near-miss check)
      checks.realStateUnchanged = !st || realStateFingerprint() === st.realStateFingerprint;
      if (!checks.realStateUnchanged) {
        hints.push(`~/.promptster changed since this sandbox was made. Something escaped the sandbox. Stop and investigate before trusting any result.`);
      }
      const realSession = fs.existsSync(path.join(REAL_PROMPTSTER, "active-workspace"));
      checks.realSessionPresent = realSession;
      if (realSession) hints.push("This machine has a real active-workspace pointer. Never kill a promptster process outside the sandbox — it may be a live candidate's capture daemon.");

      // 4. Which flows are verifiable at all on this machine?
      checks.tools = { claude: which("claude"), codex: which("codex"), cursor: which("cursor"), go: which("go"), git: which("git"), script: which("script") };
      for (const [t, p] of Object.entries(checks.tools)) {
        if (!p && ["go", "git", "script"].includes(t)) hints.push(`${t} is not installed and is required. Install it.`);
        if (!p && ["claude", "codex"].includes(t)) hints.push(`${t} is not installed — \`start --tools ${t}\` will not keep that tool, so its flow cannot be verified here. Say so rather than reporting a pass.`);
      }

      // 5. Processes
      const procs = st ? promptsterProcesses(st) : { ours: [], foreign: [] };
      checks.processes = procs;
      if (procs.foreign.length) hints.push(`${procs.foreign.length} promptster process(es) belong to the REAL install. They are not ours. \`down\` will not touch them and neither should you.`);

      const healthy = checks.sandboxExists && checks.binaryCurrent && checks.binaryUnmodified && checks.sandboxIsolated && checks.realStateUnchanged;
      out({ healthy, sandbox: st?.sbx ?? null, evidence: EVIDENCE, checks, hints });
      if (!healthy) process.exit(1);
      return;
    }

    // --------------------------------------------------------------- build
    case "build": {
      const st = requireSandbox();
      const built = build();
      fs.copyFileSync(built.bin, st.bin);
      fs.chmodSync(st.bin, 0o755);
      st.binSha = sha(st.bin);
      st.sourceMtimeAtBuild = built.sourceMtime;
      writeState(st);
      out({ rebuilt: true, binary: st.bin, binSha: st.binSha, buildMs: built.buildMs });
      return;
    }

    // ----------------------------------------------------------------- run
    case "run": {
      const st = requireSandbox();
      if (!positional.length) die("run needs a subcommand", "e.g. `run doctor`, `run status --json`, `run start --tools claude --workspace WS --accept-tos`");
      const res = runSync(st, positional, {
        timeoutMs: Number(flags.timeout || DEFAULT_TIMEOUT_MS),
        stdin: typeof flags.stdin === "string" ? flags.stdin : "",
      });
      const evidence = flags.evidence
        ? saveEvidence(String(flags.evidence), `$ promptster ${positional.join(" ")}\nexit=${res.exitCode}\n\n--- stdout ---\n${res.stdout}\n--- stderr ---\n${res.stderr}`)
        : null;
      if (res.timedOut) {
        return die(`\`promptster ${positional.join(" ")}\` did not finish in ${flags.timeout || DEFAULT_TIMEOUT_MS}ms`,
          "A hang is a finding, not an infrastructure problem — report it. `doctor` in particular shells out to `cursor --list-extensions` with no timeout of its own. Re-run with a larger --timeout only if you have a reason to believe it would finish.",
          { ...res, stdoutSoFar: res.stdout, evidence });
      }
      out({ ...res, evidence });
      if (res.exitCode !== 0) process.exit(0); // a non-zero CLI exit is data, not a control-cli failure
      return;
    }

    // ----------------------------------------------------------------- tui
    case "tui": {
      const st = requireSandbox();
      if (!positional.length) die("tui needs a subcommand", "e.g. `tui brief --here --keys q` or `tui explain`");
      if (!which("script")) die("script(1) is not available", "The PTY driver needs `script`. Without it a TUI sees a pipe, takes its non-interactive branch, and you verify the fallback instead of the TUI.");
      const keys = flags.keys ? String(flags.keys).split(",") : [];
      const res = await ptyRun(st, positional, {
        keys,
        timeoutMs: Number(flags.timeout || 15_000),
        settleMs: Number(flags.settle || 1200),
      });
      const evidence = flags.evidence ? saveEvidence(String(flags.evidence), res.screen) : null;
      out({ ...res, raw: undefined, rawBytes: res.raw.length, evidence });
      return;
    }

    // ---------------------------------------------------------------- feed
    case "feed": {
      const st = requireSandbox();
      const tool = positional[0];
      const ws = fs.realpathSync(path.join(st.sbx, "ws"));
      if (tool === "claude") {
        // Exactly the shape Claude Code posts to the hook. cwd must be the
        // RESOLVED workspace path (macOS /tmp is a symlink to /private/tmp).
        const payload = JSON.stringify({
          hook_event_name: "UserPromptSubmit",
          session_id: "verify-claude-1",
          cwd: ws,
          prompt: flags.prompt || "verify prompt",
        });
        const res = runSync(st, ["hook"], { stdin: payload, timeoutMs: 20_000 });
        out({ fed: "claude", via: "promptster hook (stdin)", expectSource: "claude-code", result: res });
        return;
      }
      if (tool === "codex") {
        // Codex capture is a background watcher tailing rollout JSONL, so the
        // event is a file, not a pipe. Two constraints that silently produce
        // zero events if you get them wrong, both learned the hard way:
        //   - cwd must be the symlink-RESOLVED workspace
        //   - the timestamp must be >= session.StartedAt - 2min
        const now = new Date().toISOString().replace(/\.\d+Z$/, ".000Z");
        const d = new Date();
        const dir = path.join(st.sbx, "codexhome", "sessions",
          String(d.getUTCFullYear()),
          String(d.getUTCMonth() + 1).padStart(2, "0"),
          String(d.getUTCDate()).padStart(2, "0"));
        fs.mkdirSync(dir, { recursive: true });
        const file = path.join(dir, `rollout-verify-${Date.now()}.jsonl`);
        fs.writeFileSync(file,
          JSON.stringify({ timestamp: now, type: "session_meta", payload: { id: "cx-verify", cwd: ws, originator: "codex_cli" } }) + "\n" +
          JSON.stringify({ timestamp: now, type: "event_msg", payload: { type: "user_message", message: flags.prompt || "verify prompt" } }) + "\n");
        out({ fed: "codex", via: "rollout jsonl", rollout: file, expectSource: "codex",
              note: "The watcher polls. Poll `state --what buffer` for up to ~12s before concluding nothing was captured." });
        return;
      }
      return die("feed needs a tool", "`feed claude` (posts a hook payload on stdin) or `feed codex` (writes a rollout JSONL the watcher tails).");
    }

    // --------------------------------------------------------------- state
    case "state": {
      const st = requireSandbox();
      const what = String(flags.what || "all");
      const sd = path.join(st.sbx, "state");
      const res = {};
      const readJson = (p) => { try { return JSON.parse(fs.readFileSync(p, "utf8")); } catch (e) { return { unreadable: String(e.message) }; } };

      if (what === "session" || what === "all") {
        const p = path.join(sd, "session.json");
        res.session = fs.existsSync(p) ? readJson(p) : null;
        if (res.session === null) res.sessionNote = "no session.json — `start` never completed, or it was torn down by abort/reset.";
      }
      if (what === "buffer" || what === "all") {
        const p = path.join(st.sbx, "state", "buffer.jsonl");
        if (!fs.existsSync(p)) {
          res.buffer = { exists: false, events: [], note: "no buffer.jsonl. Nothing has been captured. An empty buffer is the silent-failure state — it is NOT proof the flow works." };
        } else {
          const lines = fs.readFileSync(p, "utf8").split("\n").filter(Boolean);
          const events = lines.map((l) => { try { return JSON.parse(l); } catch { return { unparseable: l.slice(0, 200) }; } });
          const bySource = {};
          for (const e of events) bySource[e.source ?? "(none)"] = (bySource[e.source ?? "(none)"] || 0) + 1;
          res.buffer = { exists: true, count: events.length, bySource, events: events.slice(-20) };
        }
      }
      if (what === "watchers" || what === "all") {
        res.watchers = {};
        for (const w of ["codex-watcher", "claude-watcher", "git-watcher"]) {
          const p = path.join(sd, `${w}.json`);
          const logp = path.join(sd, `${w}.log`);
          const s = fs.existsSync(p) ? readJson(p) : null;
          res.watchers[w] = {
            state: s,
            pidAlive: s?.pid ? alive(s.pid) : null,
            log: fs.existsSync(logp) ? fs.readFileSync(logp, "utf8").split("\n").slice(-15).join("\n") : null,
          };
        }
      }
      if (what === "files" || what === "all") {
        const walk = (dir, depth = 0) => {
          if (depth > 3 || !fs.existsSync(dir)) return [];
          return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
            const p = path.join(dir, e.name);
            if (e.isDirectory()) return walk(p, depth + 1);
            return [{ path: p.slice(st.sbx.length + 1), bytes: fs.statSync(p).size }];
          });
        };
        res.files = {
          state: walk(sd),
          workspace: walk(path.join(st.sbx, "ws")),
          home: walk(path.join(st.sbx, "home")),
          codexhome: walk(path.join(st.sbx, "codexhome")),
        };
      }
      out({ sandbox: st.sbx, ...res });
      return;
    }

    // ------------------------------------------------------------------ qa
    // The repo already has a harness. Rather than reimplement its assertions,
    // run it and turn its human output into JSON.
    case "qa": {
      const script = path.join(REPO, "scripts", "qa-e2e.sh");
      if (!fs.existsSync(script)) die("scripts/qa-e2e.sh is missing", "It is the repo's own end-to-end harness and this command is a thin wrapper over it. Restore it from git.");
      const tool = positional[0] || "all";
      const t0 = Date.now();
      const r = spawnSync("bash", [script, tool], { cwd: REPO, encoding: "utf8", timeout: Number(flags.timeout || 600_000) });
      const text = (r.stdout || "") + (r.stderr || "");
      const checks = [...text.matchAll(/^\s*(PASS|FAIL):\s*(.+)$/gm)].map((m) => ({ result: m[1], check: m[2].trim() }));
      const evidence = saveEvidence(`qa-e2e-${tool}.txt`, text);
      out({
        harness: script, tool, exitCode: r.status, ms: Date.now() - t0,
        passed: checks.filter((c) => c.result === "PASS").length,
        failed: checks.filter((c) => c.result === "FAIL").length,
        checks, evidence,
        note: "This harness makes and tears down its OWN sandboxes; it does not use the one from `up`.",
      });
      return;
    }

    // ------------------------------------------------------------ evidence
    case "evidence": {
      if (!fs.existsSync(EVIDENCE)) return out({ dir: EVIDENCE, files: [], note: "nothing captured yet" });
      const files = fs.readdirSync(EVIDENCE).map((f) => {
        const s = fs.statSync(path.join(EVIDENCE, f));
        return { file: path.join(EVIDENCE, f), bytes: s.size, at: new Date(s.mtimeMs).toISOString() };
      }).sort((a, b) => a.at.localeCompare(b.at));
      out({ dir: EVIDENCE, files });
      return;
    }

    // ---------------------------------------------------------------- down
    case "down": {
      const st = readState();
      if (!st?.sbx) return out({ alreadyDown: true, evidence: EVIDENCE });
      const { ours, foreign } = promptsterProcesses(st);
      const killed = [];
      for (const p of ours) {
        // Only processes whose command line contains THIS sandbox path. Never a
        // name match: the real install runs the same binary name.
        try { process.kill(p.pid, "SIGTERM"); killed.push(p); } catch {}
      }
      if (killed.length) await new Promise((r) => setTimeout(r, 500));
      for (const p of killed) { try { process.kill(p.pid, "SIGKILL"); } catch {} }

      const drifted = realStateFingerprint() !== st.realStateFingerprint;
      fs.rmSync(st.sbx, { recursive: true, force: true });
      fs.rmSync(STATE, { force: true });
      out({
        removed: st.sbx,
        killed,
        spared: foreign,
        realStateUnchanged: !drifted,
        evidence: EVIDENCE,
        note: "Evidence is deliberately NOT removed. Cite the paths above in your summary.",
        ...(drifted ? { warning: "~/.promptster changed during this run — something escaped the sandbox. Investigate." } : {}),
      });
      return;
    }

    default:
      die(`unknown command: ${cmd}`, "Run `control-cli.mjs help` for the command list.");
  }
}

main().catch((e) =>
  die("control-cli crashed", "This is a bug in the control CLI, not in the CLI under test.", { detail: String(e?.stack || e) })
);
