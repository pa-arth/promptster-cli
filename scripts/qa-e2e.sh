#!/usr/bin/env bash
#
# qa-e2e.sh — sandboxed end-to-end QA for promptster-cli, per AI tool.
#
# Runs the REAL `promptster start` flow (hooks, proxy config, watchers) plus a
# live capture check and `doctor`/`brief`, for either of claude|codex — all
# inside a throwaway sandbox so your real ~/.zshrc, ~/.promptster, ~/.codex and
# ~/.claude are never touched and no network mutation happens.
#
# How the isolation works:
#   HOME                  -> $SBX/home      (shell RC, ~/.promptster, ~/.claude)
#   PROMPTSTER_STATE_DIR  -> $SBX/state     (session.json, buffer.jsonl, watcher state)
#   PROMPTSTER_BUFFER_PATH-> $SBX/state/buffer.jsonl
#   CODEX_HOME            -> $SBX/codexhome (codex config.toml + rollouts)
#   PROMPTSTER_API_URL    -> http://127.0.0.1:9  (unreachable: events buffer locally)
#
# We craft session.json directly (consentAccepted=true, no repo) so no real key
# redemption is needed. We NEVER pass --restart (that could kill a running editor).
#
# Usage:
#   scripts/qa-e2e.sh [codex|claude|all]   (default: all)
#
# Exit code is non-zero if any assertion fails.

set -uo pipefail

TOOLS="${1:-all}"
[ "$TOOLS" = "all" ] && TOOLS="codex claude"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FAILS=0
pass(){ echo "    PASS: $1"; }
fail(){ echo "    FAIL: $1"; FAILS=$((FAILS+1)); }
strip(){ sed -E 's/\x1b\[[0-9;]*m//g'; }

# Build once into a temp location.
BUILD_BIN="$(mktemp -d)/promptster"
echo "==> building promptster from $REPO_ROOT"
( cd "$REPO_ROOT" && go build -o "$BUILD_BIN" . ) || { echo "BUILD FAILED"; exit 1; }

qa_one() {
  local tool="$1"
  local SBX BIN WS REAL_WS
  SBX="$(mktemp -d "/tmp/pst-qa-${tool}.XXXXXX")"
  BIN="$SBX/promptster"; cp "$BUILD_BIN" "$BIN"
  mkdir -p "$SBX/home" "$SBX/state" "$SBX/ws" "$SBX/codexhome"
  WS="$SBX/ws"; REAL_WS="$(cd "$WS" && pwd -P)"

  cat > "$SBX/state/session.json" <<JSON
{ "sessionId":"sess-$tool","sessionToken":"tok-$tool","key":"PST-QA",
  "assessmentTitle":"$tool QA","orgName":"Acme","taskBrief":"Do the task.",
  "taskRoot":"$WS","timeLimitMinutes":90,"consentAccepted":true,
  "allowedTools":["$tool"],"apiUrl":"http://127.0.0.1:9" }
JSON

  run(){ HOME="$SBX/home" SHELL="/bin/zsh" PROMPTSTER_STATE_DIR="$SBX/state" \
         PROMPTSTER_BUFFER_PATH="$SBX/state/buffer.jsonl" CODEX_HOME="$SBX/codexhome" \
         PROMPTSTER_API_URL="http://127.0.0.1:9" "$BIN" "$@"; }

  echo ""
  echo "================ QA: $tool ================"
  echo "  [start]"
  run start --tools "$tool" --workspace "$WS" --accept-tos < /dev/null > "$SBX/start.out" 2>&1
  if grep -q "Next steps:" "$SBX/start.out"; then pass "start completed the setup flow"; else fail "start did not finish"; sed 's/^/      /' "$SBX/start.out" | tail -5; fi

  # session reflects the chosen tool
  grep -q "\"$tool\"" "$SBX/state/session.json" && grep -q '"tools"' "$SBX/state/session.json" \
    && pass "session.json tools=[$tool]" || fail "session tools not persisted"

  # binary self-installed under fake HOME
  [ -x "$SBX/home/.promptster/bin/promptster" ] && pass "binary installed to ~/.promptster/bin" || fail "binary install"

  # per-tool setup + capture
  case "$tool" in
    claude)
      grep -q "promptster hook" "$SBX/ws/.claude/settings.local.json" 2>/dev/null \
        && pass ".claude/settings.local.json hooks written" || fail "claude hooks"
      echo "{\"hook_event_name\":\"UserPromptSubmit\",\"session_id\":\"cs1\",\"cwd\":\"$REAL_WS\",\"prompt\":\"qa prompt\"}" \
        | run hook pre-tool-use >/dev/null 2>&1
      ;;
    codex)
      CFG="$SBX/codexhome/config.toml"
      { [ -f "$CFG" ] && grep -q 'model_provider = "promptster"' "$CFG"; } \
        && pass "codex config.toml proxy provider written" || fail "codex proxy config"
      local NOW; NOW="$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"
      local RDIR="$SBX/codexhome/sessions/$(date -u +%Y/%m/%d)"; mkdir -p "$RDIR"
      cat > "$RDIR/rollout-qa.jsonl" <<RJ
{"timestamp":"$NOW","type":"session_meta","payload":{"id":"cx1","cwd":"$REAL_WS","originator":"codex_cli"}}
{"timestamp":"$NOW","type":"event_msg","payload":{"type":"user_message","message":"qa prompt"}}
RJ
      for _ in $(seq 1 12); do sleep 1; grep -q '"source":"codex"' "$SBX/state/buffer.jsonl" 2>/dev/null && break; done
      ;;
  esac

  # capture assertion: an event for this tool landed in the buffer
  local want="$tool"; [ "$tool" = "claude" ] && want="claude-code"
  if grep -q "\"source\":\"$want\"" "$SBX/state/buffer.jsonl" 2>/dev/null; then
    pass "capture: buffered an event with source=$want"
  else
    fail "capture: no source=$want event in buffer.jsonl"
  fi

  # doctor is tool-aware: shows this tool, hides the others' sections
  run doctor > "$SBX/doctor.out" 2>&1; strip < "$SBX/doctor.out" > "$SBX/doctor.txt"
  case "$tool" in
    codex)  grep -q "codex binary" "$SBX/doctor.txt" && ! grep -q "Claude hooks" "$SBX/doctor.txt" \
              && pass "doctor: codex-aware (no Claude hooks section)" || fail "doctor codex-aware" ;;
    claude) grep -q "claude binary" "$SBX/doctor.txt" && grep -q "Claude hooks" "$SBX/doctor.txt" \
              && pass "doctor: claude-aware (Claude hooks section present)" || fail "doctor claude-aware" ;;
  esac

  # brief surfaces the tool
  run brief 2>/dev/null | strip | grep -q "Tools" && pass "brief: Tools shown" || fail "brief Tools row"

  # teardown: stop any daemons this sandbox spawned, remove it
  pkill -f "$SBX/home/.promptster/bin/promptster" 2>/dev/null
  sleep 0.3
  rm -rf "$SBX"
}

for t in $TOOLS; do qa_one "$t"; done

echo ""
echo "================ summary ================"
if [ "$FAILS" -eq 0 ]; then echo "  ALL CHECKS PASSED"; else echo "  $FAILS CHECK(S) FAILED"; fi
rm -rf "$(dirname "$BUILD_BIN")"
exit "$FAILS"
