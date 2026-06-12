# @promptster/cli

The official Promptster CLI. Capture and submit AI-native developer assessments.

## Installation

```bash
npm install -g @promptster/cli
# or
pnpm add -g @promptster/cli
```

## Usage

```bash
# Validate your candidate key and accept terms
promptster redeem PST-XXXX-XXXX

# Configure hooks/MCP and begin your assessment
promptster start

# Submit when you're done
promptster done

# Check current session info
promptster status

# Diagnose your setup
promptster doctor

# Drain queued important decisions
promptster explain
```

## Supported Platforms

- macOS (Intel + Apple Silicon)
- Linux (x64 + arm64)
- Windows (x64)

## Releasing a New Version

From the repo root:

```bash
# 1. Prepare and stage only the CLI release manifest
node scripts/cli-release-prepare.mjs 0.2.8

# 2. Review the changelog, then commit the staged files
git commit -m "chore(cli): release v0.2.8"

# 3. Create the release tag after CLI-owned paths are clean
node scripts/cli-release-tag.mjs 0.2.8

# 4. Push commit + tag to trigger CI publish
git push origin HEAD --follow-tags
```

Release notes come from `CHANGELOG.md`. The helper scripts only stage the release manifest and will not sweep in unrelated repo changes.

Source lives at [github.com/pa-arth/promptster-cli](https://github.com/pa-arth/promptster-cli).

## License

MIT. See the repository LICENSE for details.
The promptster CLI binary is free to use for assessment participation by candidates and reviewers.
