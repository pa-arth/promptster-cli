#!/usr/bin/env node
import { extractChangelogSection, normalizeVersion } from './cli-release-lib.mjs';

const args = process.argv.slice(2);
const versionArg = args.find((arg) => !arg.startsWith('--'));
const version = normalizeVersion(versionArg);

console.log(extractChangelogSection(version));
