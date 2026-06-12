#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

const EXPECTED = [
  "promptster-linux-x64",
  "promptster-linux-arm64",
  "promptster-darwin-x64",
  "promptster-darwin-arm64",
  "promptster-win32-x64.exe",
];

const binDir = path.join(__dirname, "..", "binaries");
let missing = [];

for (const name of EXPECTED) {
  const p = path.join(binDir, name);
  if (!fs.existsSync(p)) {
    missing.push(name);
  }
}

if (missing.length > 0) {
  console.error("ERROR: Missing binaries before publish:");
  for (const m of missing) console.error("  - " + m);
  console.error("\nRun: node scripts/build.js");
  process.exit(1);
}

console.log("✓ All binaries present");
