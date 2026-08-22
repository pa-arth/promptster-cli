# Promptster CLI

The session recorder behind [Promptster](https://promptster.ai) — it sets up a
candidate's assessment environment on their own machine, clones the assessment
repository, installs capture hooks for AI coding tools (Claude Code,
Codex CLI), and streams the working session (prompts, tool calls, file diffs,
terminal commands, decisions) to a Promptster-compatible backend for replay
and review.

It is a single static Go binary. Candidates run two commands:

```bash
promptster start PST-XXXX-XXXX   # consent → clone repo → install hooks → start capture
# ... work normally in Claude Code / Codex ...
promptster done                  # run verification, upload the workspace, submit
```

## Install

```bash
# Hosted installer (macOS / Linux)
curl -fsSL https://get.promptster.ai | sh

# Or via npm (all platforms, including Windows)
npm install -g @promptster/cli

# Or from source
go install github.com/pa-arth/promptster-cli@latest
```

## Commands

| Command | What it does |
|---|---|
| `promptster start <KEY>` | Redeem a candidate key, clone the assessment repo, install hooks, begin capture |
| `promptster status` / `brief` | Session status and the task brief (`--json` for machine-readable) |
| `promptster explain "<why>"` | Record a decision rationale at any point during the session |
| `promptster verify` | Run the assessment's verification suite locally |
| `promptster done` | Final verification, workspace upload, session submit |
| `promptster doctor` | Diagnose hook, auth, and connectivity problems |
| `promptster abort` / `reset` | Tear down the session / wipe local state |

Non-interactive flags for scripted use: `start --accept-tos --workspace PATH`,
`done --auto`, `status --json`, `brief --json`.

## Using it with your own backend

The CLI is not hardwired to Promptster's hosted service. Point it at any
backend that implements the same HTTP contract and it will do candidate
environment setup, repository cloning, hook installation, and event capture
against your infrastructure:

```bash
export PROMPTSTER_API_URL="https://assessments.your-org.com"
promptster start YOUR-KEY-FORMAT
```

The API base URL is persisted into the session at `start`, so hooks and
background watchers keep talking to your backend for the life of the session
without the env var being present in every shell.

### Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `PROMPTSTER_API_URL` | Backend base URL all API calls go to | `https://api.promptster.ai` |
| `PROMPTSTER_APP_URL` | Web app base used in result links shown to candidates | `https://promptster.ai` |
| `PROMPTSTER_STATE_DIR` | Local state directory | `~/.promptster` |
| `PROMPTSTER_DEBUG=1` | Verbose hook logging | off |
| `PROMPTSTER_DISABLE_DECISION_PROMPT=1` | Disable decision-capture nudges | off |
| `PROMPTSTER_VERSION` | Pin a version in the curl installer | `latest` |

(`PROMPTSTER_BUFFER_PATH`, `PROMPTSTER_HOOK_SOURCE`, and the
`PROMPTSTER_DECISION_*` path overrides exist for testing and are not needed in
normal use.)

### The API contract the CLI speaks

All endpoints are relative to `PROMPTSTER_API_URL`. Candidate-key endpoints
authenticate with `X-API-Key: <candidate key>`.

| Endpoint | Used for |
|---|---|
| `GET /v1/health` | Connectivity preflight |
| `POST /v1/candidate/redeem` | Exchange a candidate key for session + assessment details |
| `GET /v1/candidate/consent` | Canonical consent disclosure text |
| `POST /v1/candidate/device-check` | Device fingerprint for session continuity |
| `POST /v1/hooks/ingest` | Telemetry event stream (prompts, diffs, commands, decisions) |
| `POST /v1/candidate/upload-url` / `workspace-commit` | Workspace snapshot upload at submit |
| `POST /v1/candidate/complete` | Final submission |
| `GET /v1/sessions/{id}` | Session status |
| `GET /v1/cli/version-check` | Minimum/latest version gate (optional — failures are silent) |
| `POST /v1/proxy/anthropic`, `/v1/proxy/openai/v1` | Managed-billing LLM proxy (optional — only used when the backend issues proxy credentials) |

Endpoints marked optional degrade gracefully when your backend doesn't
implement them.

## What gets captured

Hooks are installed per-workspace for Claude Code (`.claude/settings.local.json`)
and Codex (rollout transcript tailing), plus an
opt-in shell hook for human terminal commands. Events are normalized to a
common schema (`prompt`, `file_diff`, `command`, `ai_response`, `tool_intent`,
`decision_event`, …) before upload. Candidates see a full disclosure and must
consent before any capture starts; `promptster abort` tears everything down.

## Development

```bash
go build ./...        # build
go vet ./...          # lint
go test ./...         # tests
make build            # versioned binary in bin/
```

Releases: bump `npm/package.json` + add a `npm/CHANGELOG.md` entry, tag
`vX.Y.Z` on main — `.github/workflows/release.yml` cross-compiles 5 platforms,
publishes `@promptster/cli` to npm, and creates the GitHub releases.

## License

[MIT](./LICENSE)
