#!/usr/bin/env node
import {
  config,
  ensurePathsClean,
  ensureTagDoesNotExist,
  normalizeVersion,
  readJson,
  stagePaths,
  tagNameFor,
  upsertChangelogEntry,
  writeJson,
} from './cli-release-lib.mjs';

const args = process.argv.slice(2);
const dryRun = args.includes('--dry-run');
const versionArg = args.find((arg) => !arg.startsWith('--'));
const version = normalizeVersion(versionArg);
const tagName = tagNameFor(version);

ensureTagDoesNotExist(tagName);
ensurePathsClean(config.releaseManifestPaths);

const pkg = readJson(config.packageFile);
pkg.version = version;

if (!dryRun) {
  writeJson(config.packageFile, pkg);
  upsertChangelogEntry(version);
}

stagePaths(config.releaseManifestPaths, dryRun);

console.log(`${dryRun ? 'Would prepare' : 'Prepared'} CLI release ${tagName}`);
for (const relFile of config.releaseManifestPaths) {
  console.log(`- ${relFile}`);
}

console.log('');
console.log('Next steps:');
console.log(`1. Review ${config.changelogFile}`);
console.log(
  `2. Commit the staged release manifest, for example: git commit -m "chore(cli): release v${version}"`,
);
console.log(`3. Run: node scripts/cli-release-tag.mjs ${version}`);
