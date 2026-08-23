#!/usr/bin/env node
"use strict";

/**
 * Publish gate: prove that the tarball we are about to push to npm actually
 * yields a runnable `promptster` on PATH.
 *
 * It packs the package, installs the tarball into a throwaway global prefix,
 * and EXECUTES the resulting command. That distinction is the whole point:
 * check-binaries.js reads a file list, and a file list was green for v1.7.0 —
 * every binary was present and there was simply nothing to dispatch to them,
 * so `npm install -g @promptster/cli@1.7.0` installed no command at all and
 * still exited 0.
 *
 * Two details this gate is deliberately picky about, because each one on its
 * own is enough to ship an install that does nothing:
 *
 *  - It packs with the SAME packer the release workflow publishes with. `pnpm
 *    pack` rewrites every file to mode 0644; `npm pack` preserves 0755. The
 *    release runs `pnpm publish`, so a gate that packed with npm would be
 *    green over a tarball whose Go binaries arrive non-executable.
 *  - It runs the command a second time with the bundled binary forced to 0644,
 *    which is the state pnpm ships. That asserts the shim restores the
 *    executable bit itself rather than depending on the packer's mood.
 *
 * The install passes --ignore-scripts, so a package that needs a postinstall
 * to become runnable fails here rather than on the machines of users whose npm
 * config disables scripts.
 *
 * Usage: node scripts/check-install.js
 */

const { execFileSync, spawnSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

// The gate packs, and a packer runs lifecycle scripts. prepublishOnly is not
// one of them today, but this makes a future packer change a clear message
// instead of an infinite loop inside `pnpm publish`.
if (process.env.PROMPTSTER_INSTALL_CHECK === "1") {
  console.error("ERROR: check-install.js re-entered itself — the packer is running prepublishOnly");
  process.exit(1);
}
process.env.PROMPTSTER_INSTALL_CHECK = "1";

const packageDir = path.resolve(__dirname, "..");
const pkg = JSON.parse(fs.readFileSync(path.join(packageDir, "package.json"), "utf8"));

const BINARY_BY_PLATFORM = {
  "darwin-arm64": "promptster-darwin-arm64",
  "darwin-x64": "promptster-darwin-x64",
  "linux-arm64": "promptster-linux-arm64",
  "linux-x64": "promptster-linux-x64",
  "win32-x64": "promptster-win32-x64.exe",
};

function fail(message, ...details) {
  console.error(`ERROR: ${message}`);
  for (const line of details) console.error(`  ${line}`);
  process.exit(1);
}

function hasCommand(name) {
  const probe = spawnSync(name, ["--version"], { stdio: "ignore", shell: process.platform === "win32" });
  return !probe.error && probe.status === 0;
}

const platformKey = `${process.platform}-${process.arch}`;
const hostBinary = BINARY_BY_PLATFORM[platformKey];
if (!hostBinary) {
  fail(
    `cannot verify the install on unsupported platform ${platformKey}`,
    `supported: ${Object.keys(BINARY_BY_PLATFORM).sort().join(", ")}`,
  );
}
if (!fs.existsSync(path.join(packageDir, "binaries", hostBinary))) {
  fail(
    `binaries/${hostBinary} is missing, so the packed tarball cannot be run here`,
    "run: node scripts/build.js",
  );
}

// Match .github/workflows/release.yml, which publishes with pnpm.
const packer = hasCommand("pnpm") ? "pnpm" : "npm";

const workDir = fs.mkdtempSync(path.join(os.tmpdir(), "promptster-install-check-"));
const prefixDir = path.join(workDir, "prefix");
fs.mkdirSync(prefixDir);

function runPromptster(command, label) {
  const run = spawnSync(command, ["--version"], { encoding: "utf8" });
  if (run.error) {
    fail(`could not execute the installed promptster (${label}): ${run.error.message}`, `path: ${command}`);
  }
  if (run.status !== 0) {
    fail(
      `\`promptster --version\` from the packed tarball exited ${run.status} (${label})`,
      ...String(run.stderr || "").trim().split("\n").filter(Boolean),
    );
  }
  const reported = String(run.stdout || "").trim();
  if (reported !== pkg.version) {
    fail(
      `\`promptster --version\` printed ${JSON.stringify(reported)}, expected ${JSON.stringify(pkg.version)}`,
      "the bundled binaries were built from a different version stamp",
      `run: node scripts/build.js ${pkg.version}`,
    );
  }
  return reported;
}

let ok = false;
try {
  execFileSync(packer, ["pack", "--pack-destination", workDir], {
    cwd: packageDir,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "inherit"],
  });
  const tarballs = fs.readdirSync(workDir).filter((entry) => entry.endsWith(".tgz"));
  if (tarballs.length !== 1) {
    fail(`expected exactly one tarball from \`${packer} pack\`, got ${tarballs.length}`);
  }
  const tarball = path.join(workDir, tarballs[0]);

  execFileSync(
    "npm",
    [
      "install",
      "--global",
      "--prefix",
      prefixDir,
      "--ignore-scripts",
      "--no-audit",
      "--no-fund",
      "--loglevel",
      "error",
      tarball,
    ],
    { cwd: workDir, encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] },
  );

  // On Windows npm writes promptster.cmd at the prefix root; elsewhere it
  // links into <prefix>/bin.
  const candidates =
    process.platform === "win32"
      ? [path.join(prefixDir, "promptster.cmd"), path.join(prefixDir, "promptster")]
      : [path.join(prefixDir, "bin", "promptster")];
  const command = candidates.find((candidate) => fs.existsSync(candidate));
  if (!command) {
    fail(
      "the packed tarball installed no `promptster` command",
      `packed with: ${packer}`,
      `looked for: ${candidates.join(", ")}`,
      `package.json "bin" is ${JSON.stringify(pkg.bin)} — is that file committed and matched by "files"?`,
    );
  }

  const reported = runPromptster(command, `packed with ${packer}`);

  if (process.platform !== "win32") {
    const installedBinary = path.join(
      prefixDir,
      "lib",
      "node_modules",
      pkg.name,
      "binaries",
      hostBinary,
    );
    if (!fs.existsSync(installedBinary)) {
      fail(`installed package is missing binaries/${hostBinary}`, `looked at: ${installedBinary}`);
    }
    fs.chmodSync(installedBinary, 0o644);
    runPromptster(command, "bundled binary forced to mode 0644");
  }

  console.log(`✓ packed tarball (${packer}) installs a runnable promptster (${reported})`);
  ok = true;
} finally {
  fs.rmSync(workDir, { recursive: true, force: true });
  if (!ok) process.exitCode = process.exitCode || 1;
}
