#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
const config = JSON.parse(
  fs.readFileSync(path.join(repoRoot, 'scripts/cli-release-config.json'), 'utf8'),
);

export { config, repoRoot };

export function fail(message) {
  console.error(`ERROR: ${message}`);
  process.exit(1);
}

export function runGit(args, options = {}) {
  return execFileSync('git', args, {
    cwd: repoRoot,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    ...options,
  }).trim();
}

export function relPath(filePath) {
  return path.relative(repoRoot, filePath).replaceAll(path.sep, '/');
}

export function isOwnedPath(filePath) {
  return config.ownedPaths.some((ownedPath) => {
    return filePath === ownedPath || filePath.startsWith(`${ownedPath}/`);
  });
}

export function listGitStatus() {
  const output = runGit(['status', '--porcelain=v1', '--untracked-files=all']);
  if (!output) return [];

  return output
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      const status = line.slice(0, 2);
      const rawPath = line.slice(3).trim();
      const filePath = rawPath.includes(' -> ') ? rawPath.split(' -> ').at(-1) : rawPath;

      return { status, filePath };
    });
}

export function listOwnedGitStatus() {
  return listGitStatus().filter((entry) => isOwnedPath(entry.filePath));
}

export function ensurePathsClean(paths) {
  const dirty = listGitStatus().filter((entry) => paths.includes(entry.filePath));
  if (dirty.length === 0) return;

  console.error('Release manifest paths already have local changes:');
  for (const entry of dirty) {
    console.error(`- ${entry.status} ${entry.filePath}`);
  }
  fail('clean or commit the release manifest before preparing a new CLI release');
}

export function ensureTagDoesNotExist(tagName) {
  const existing = runGit(['tag', '--list', tagName]);
  if (existing) fail(`git tag ${tagName} already exists`);
}

export function normalizeVersion(input) {
  const version = input?.trim();
  if (!version || !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) {
    fail(`invalid version "${input}". Expected semver like 1.2.3`);
  }

  return version;
}

export function tagNameFor(version) {
  return `${config.tagPrefix}${version}`;
}

export function readJson(relFile) {
  return JSON.parse(fs.readFileSync(path.join(repoRoot, relFile), 'utf8'));
}

export function writeJson(relFile, value) {
  fs.writeFileSync(path.join(repoRoot, relFile), `${JSON.stringify(value, null, 2)}\n`);
}

export function readChangelog() {
  const file = path.join(repoRoot, config.changelogFile);
  if (!fs.existsSync(file)) {
    return '# CLI Changelog\n\n';
  }

  return fs.readFileSync(file, 'utf8');
}

export function writeChangelog(contents) {
  fs.writeFileSync(path.join(repoRoot, config.changelogFile), contents);
}

export function getLatestCliTag() {
  const output = runGit(['tag', '--list', `${config.tagPrefix}*`, '--sort=-version:refname']);

  return output ? output.split('\n')[0] : null;
}

export function buildChangelogEntriesSince(tagName) {
  const range = tagName ? `${tagName}..HEAD` : 'HEAD';
  const args = [
    'log',
    '--no-merges',
    '--pretty=format:%s%x09%h',
    range,
    '--',
    ...config.changelogSourcePaths,
  ];
  const output = runGit(args);
  if (!output) return [];

  const seen = new Set();
  const entries = [];
  for (const line of output.split('\n')) {
    const [subject, sha] = line.split('\t');
    if (!subject || seen.has(subject)) continue;
    seen.add(subject);
    entries.push(`- ${subject} (${sha})`);
  }

  return entries;
}

export function renderChangelogEntry(version) {
  const today = new Date().toISOString().slice(0, 10);
  const previousTag = getLatestCliTag();
  const changes = buildChangelogEntriesSince(previousTag);
  const lines = [
    `## ${version} - ${today}`,
    '',
    ...(changes.length ? changes : ['- Release metadata refresh for the CLI publishing path.']),
    '',
  ];

  return lines.join('\n');
}

export function upsertChangelogEntry(version) {
  const changelog = readChangelog();
  const header = '# CLI Changelog\n\n';
  const entryRegex = new RegExp(`^## ${escapeRegExp(version)} - .*$`, 'm');
  const entry = renderChangelogEntry(version);

  if (entryRegex.test(changelog)) {
    const replaced = changelog.replace(
      new RegExp(`## ${escapeRegExp(version)} - .*?(?=\\n## |$)`, 's'),
      entry.trimEnd(),
    );
    writeChangelog(replaced.endsWith('\n') ? replaced : `${replaced}\n`);
    return;
  }

  const body = changelog.startsWith('# CLI Changelog') ? changelog.slice(header.length) : changelog;
  writeChangelog(`${header}${entry}${body.replace(/^\n+/, '')}`);
}

export function extractChangelogSection(version) {
  const changelog = readChangelog();
  const match = changelog.match(
    new RegExp(`(^## ${escapeRegExp(version)} - [\\s\\S]*?(?=\\n## |$(?![\\r\\n])))`, 'm'),
  );
  if (!match) {
    fail(`missing changelog entry for ${version} in ${config.changelogFile}`);
  }

  return match[1].trim();
}

export function stagePaths(paths, dryRun = false) {
  if (dryRun) return;
  execFileSync('git', ['add', '--', ...paths], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
