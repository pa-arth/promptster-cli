#!/usr/bin/env node
"use strict";

/**
 * Cross-compiles the Go CLI binary for all target platforms.
 * Run from the monorepo root or from this directory.
 *
 * Usage:
 *   node scripts/build.js [version]
 *
 * Example:
 *   node scripts/build.js 0.1.0
 */

const { execSync } = require("child_process");
const path = require("path");
const fs = require("fs");

const pkg = JSON.parse(fs.readFileSync(path.resolve(__dirname, "../package.json"), "utf8"));
const version = process.argv[2] || pkg.version || "dev";

const TARGETS = [
  { goos: "linux",   goarch: "amd64", out: "promptster-linux-x64" },
  { goos: "linux",   goarch: "arm64", out: "promptster-linux-arm64" },
  { goos: "darwin",  goarch: "amd64", out: "promptster-darwin-x64" },
  { goos: "darwin",  goarch: "arm64", out: "promptster-darwin-arm64" },
  { goos: "windows", goarch: "amd64", out: "promptster-win32-x64.exe" },
];

// Path to the Go source (relative to this script's parent)
const goSrcDir = path.resolve(__dirname, "../..");
const binariesDir = path.resolve(__dirname, "../binaries");

if (!fs.existsSync(binariesDir)) {
  fs.mkdirSync(binariesDir, { recursive: true });
}

if (!fs.existsSync(goSrcDir)) {
  console.error(`Go source not found at: ${goSrcDir}`);
  console.error("Expected: the repository root (go.mod alongside main.go)");
  process.exit(1);
}

console.log(`Building promptster v${version} for all platforms...\n`);

for (const { goos, goarch, out } of TARGETS) {
  const outPath = path.join(binariesDir, out);
  const cmd = `go build -ldflags "-X main.version=${version}" -o ${outPath} .`;
  console.log(`  ${goos}/${goarch} → binaries/${out}`);
  try {
    execSync(cmd, {
      cwd: goSrcDir,
      env: { ...process.env, GOOS: goos, GOARCH: goarch, CGO_ENABLED: "0" },
      stdio: "inherit",
    });
  } catch (err) {
    console.error(`\nFailed to build ${goos}/${goarch}`);
    process.exit(1);
  }
}

console.log("\n✓ All binaries built successfully");
console.log(`  Output: ${binariesDir}`);
