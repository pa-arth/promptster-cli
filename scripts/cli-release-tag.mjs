#!/usr/bin/env node
import {
  config,
  ensureTagDoesNotExist,
  extractChangelogSection,
  fail,
  listOwnedGitStatus,
  normalizeVersion,
  readJson,
  repoRoot,
  runGit,
  tagNameFor,
} from './cli-release-lib.mjs';
import { execFileSync } from 'node:child_process';

const args = process.argv.slice(2);
const dryRun = args.includes('--dry-run');
const versionArg = args.find((arg) => !arg.startsWith('--'));
const version = normalizeVersion(versionArg);
const tagName = tagNameFor(version);

ensureTagDoesNotExist(tagName);

const pkg = readJson(config.packageFile);
if (pkg.version !== version) {
  fail(`${config.packageFile} is ${pkg.version}, expected ${version}`);
}

extractChangelogSection(version);

const ownedDirty = listOwnedGitStatus();
if (ownedDirty.length > 0) {
  console.error('CLI-owned paths still have local changes:');
  for (const entry of ownedDirty) {
    console.error(`- ${entry.status} ${entry.filePath}`);
  }
  fail('commit or stash CLI-owned changes before tagging');
}

const headTags = runGit(['tag', '--points-at', 'HEAD']);
if (headTags.split('\n').includes(tagName)) {
  fail(`HEAD already has tag ${tagName}`);
}

if (!dryRun) {
  execFileSync('git', ['tag', '-a', tagName, '-m', `CLI v${version}`], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
}

console.log(`${dryRun ? 'Would create' : 'Created'} tag ${tagName}`);
console.log(`Push with: git push origin HEAD --follow-tags`);
