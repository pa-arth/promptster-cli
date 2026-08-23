#!/usr/bin/env node
"use strict";

/**
 * The `promptster` command installed by `npm install -g @promptster/cli`.
 *
 * package.json's "bin" has always pointed here, but this file did not exist
 * until now: it was never committed (the root .gitignore's `bin/` pattern
 * matched `npm/bin/` at any depth), so the published tarball shipped
 * `binaries/` with nothing to dispatch to them. npm creates no link for a
 * "bin" target that is absent from the tarball and does not fail the install,
 * so `npm install -g @promptster/cli@1.7.0` reported success and left no
 * `promptster` on PATH at all. npm/scripts/check-install.js is the gate that
 * now blocks a publish in that state.
 *
 * This shim only picks the right prebuilt Go binary and hands it the argv.
 * Keep it dependency-free and Node >=16 compatible (see "engines").
 */

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

// Keep in sync with the TARGETS table in scripts/build.js and the EXPECTED
// list in scripts/check-binaries.js.
const BINARY_BY_PLATFORM = {
  "darwin-arm64": "promptster-darwin-arm64",
  "darwin-x64": "promptster-darwin-x64",
  "linux-arm64": "promptster-linux-arm64",
  "linux-x64": "promptster-linux-x64",
  "win32-x64": "promptster-win32-x64.exe",
};

const platformKey = `${process.platform}-${process.arch}`;
const binaryName = BINARY_BY_PLATFORM[platformKey];

if (!binaryName) {
  console.error(`promptster: unsupported platform ${platformKey}`);
  console.error("Supported: " + Object.keys(BINARY_BY_PLATFORM).sort().join(", "));
  console.error("Build from source instead: go install github.com/pa-arth/promptster-cli@latest");
  process.exit(1);
}

const binaryPath = path.join(__dirname, "..", "binaries", binaryName);

if (!fs.existsSync(binaryPath)) {
  console.error(`promptster: missing bundled binary ${binaryName}`);
  console.error(`Looked in: ${path.dirname(binaryPath)}`);
  console.error("This install is incomplete — reinstall with: npm install -g @promptster/cli");
  process.exit(1);
}

// npm rewrites the mode of every packed file to 0644 except the entries named
// in "bin", so the Go binaries land on disk without an executable bit and
// spawning them fails with EACCES. Restore it here rather than in a
// postinstall, because a global install run with --ignore-scripts (common in
// locked-down environments) would skip a postinstall entirely.
if (process.platform !== "win32") {
  try {
    const mode = fs.statSync(binaryPath).mode & 0o7777;
    if ((mode & 0o111) !== 0o111) {
      fs.chmodSync(binaryPath, mode | 0o755);
    }
  } catch (err) {
    console.error(`promptster: could not make ${binaryPath} executable: ${err.message}`);
    console.error(`Fix it manually with: chmod +x ${binaryPath}`);
    process.exit(1);
  }
}

const result = spawnSync(binaryPath, process.argv.slice(2), { stdio: "inherit" });

if (result.error) {
  console.error(`promptster: failed to run ${binaryPath}: ${result.error.message}`);
  process.exit(1);
}

// Re-raise the child's terminating signal so a Ctrl+C during an assessment
// looks like a signal death to the caller's shell, not exit 0.
if (result.signal) {
  process.kill(process.pid, result.signal);
}

process.exit(result.status === null ? 1 : result.status);
