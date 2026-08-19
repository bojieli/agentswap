#!/usr/bin/env bash

set -euo pipefail

if [[ ${AGENTSWAP_LIVE_ACCEPTANCE:-} != 1 ]]; then
  echo "refusing to spend provider credits; set AGENTSWAP_LIVE_ACCEPTANCE=1" >&2
  exit 2
fi

: "${AGENTSWAP_ANTHROPIC_ACCEPTANCE_KEY:?set to the real Anthropic API key used only by this isolated test}"
: "${AGENTSWAP_OPENAI_ACCEPTANCE_KEY:?set to the real OpenAI-compatible API key used only by this isolated test}"
: "${AGENTSWAP_OPENAI_ACCEPTANCE_BASE_URL:?set to the OpenAI-compatible provider base URL}"
: "${AGENTSWAP_OPENAI_ACCEPTANCE_MODEL:?set to the OpenAI-compatible provider model}"

for command in claude codex go jq rg; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 2
  fi
done

acceptance_root=$(mktemp -d "${AGENTSWAP_ACCEPTANCE_TMPDIR:-/tmp}/agentswap-credentials.XXXXXX")
pool="$acceptance_root/pool"
logs="$acceptance_root/logs"
project="$acceptance_root/project"
mkdir -p "$pool" "$logs" "$project" "$acceptance_root/claude" "$acceptance_root/codex"

repo=$(cd "$(dirname "$0")/.." && pwd -P)
binary="$acceptance_root/agentswap"
(cd "$repo" && go build -o "$binary" ./cmd/agentswap)

daemon_pid=""
cleanup() {
  if [[ -n $daemon_pid ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill -INT "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  # The evidence is status/output only. Never retain real credentials in the
  # artifact directory, even when an assertion fails.
  find "$pool" -maxdepth 1 -type f \( -name 'accounts.json*' -o -name '*.tmp' \) -delete 2>/dev/null || true
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $*" >&2
  echo "credential-free acceptance artifacts retained at $acceptance_root" >&2
  exit 1
}

run_agentswap() {
  AGENTSWAP_HOME="$pool" "$binary" "$@"
}

AGENTSWAP_HOME="$pool" AGENTSWAP_API_KEY="agentswap-intentionally-invalid-anthropic" \
  "$binary" add-key anthropic --id invalid-anthropic --priority 1 >"$logs/add-invalid-anthropic.log"
AGENTSWAP_HOME="$pool" AGENTSWAP_API_KEY="$AGENTSWAP_ANTHROPIC_ACCEPTANCE_KEY" \
  "$binary" add-key anthropic --id valid-anthropic --priority 2 >"$logs/add-valid-anthropic.log"
AGENTSWAP_HOME="$pool" AGENTSWAP_API_KEY="agentswap-intentionally-invalid-openai" \
  "$binary" add-key openai --id invalid-openai --priority 1 \
  --base-url "$AGENTSWAP_OPENAI_ACCEPTANCE_BASE_URL" >"$logs/add-invalid-openai.log"
AGENTSWAP_HOME="$pool" AGENTSWAP_API_KEY="$AGENTSWAP_OPENAI_ACCEPTANCE_KEY" \
  "$binary" add-key openai --id valid-openai --priority 2 \
  --base-url "$AGENTSWAP_OPENAI_ACCEPTANCE_BASE_URL" >"$logs/add-valid-openai.log"

AGENTSWAP_HOME="$pool" "$binary" serve --addr 127.0.0.1:0 >"$logs/daemon.log" 2>&1 &
daemon_pid=$!
for ((attempt = 0; attempt < 200; attempt++)); do
  [[ -s $pool/daemon.json ]] && break
  kill -0 "$daemon_pid" 2>/dev/null || fail "daemon exited before publishing its address"
  sleep 0.05
done
[[ -s $pool/daemon.json ]] || fail "daemon did not publish its address"
addr=$(jq -er '.addr | select(length > 0)' "$pool/daemon.json")

claude_marker="AGENTSWAP_REAL_ANTHROPIC_FAILOVER_$(date -u +%Y%m%dT%H%M%SZ)_$RANDOM"
CLAUDE_CONFIG_DIR="$acceptance_root/claude" \
ANTHROPIC_BASE_URL="http://$addr/anthropic" \
ANTHROPIC_API_KEY="client-placeholder-never-forward" \
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  claude -p "Reply with exactly: $claude_marker" \
  --output-format json --max-budget-usd "${AGENTSWAP_CLAUDE_MAX_BUDGET:-0.50}" \
  --permission-mode bypassPermissions --tools "" \
  >"$logs/claude.json" 2>"$logs/claude.stderr" || fail "Claude request through the rotating pool failed"
jq -er --arg marker "$claude_marker" '(.result // "") | contains($marker)' "$logs/claude.json" >/dev/null || fail "Claude response omitted its marker"

run_agentswap status --addr "$addr" >"$logs/status-after-anthropic.txt"
rg -q '^invalid-anthropic[[:space:]].*rejected' "$logs/status-after-anthropic.txt" || fail "invalid Anthropic key was not marked rejected"
rg -q '^valid-anthropic[[:space:]].*available' "$logs/status-after-anthropic.txt" || fail "valid Anthropic key did not serve the retried request"

{
  printf 'model = "%s"\n' "$AGENTSWAP_OPENAI_ACCEPTANCE_MODEL"
  printf '%s\n' 'model_provider = "agentswap_acceptance"'
  printf '%s\n' '' '[model_providers.agentswap_acceptance]'
  printf '%s\n' 'name = "agentswap live acceptance"'
  printf 'base_url = "http://%s/openai"\n' "$addr"
  printf '%s\n' 'env_key = "OPENAI_API_KEY"' 'wire_api = "responses"'
} >"$acceptance_root/codex/config.toml"

codex_marker="AGENTSWAP_REAL_OPENAI_FAILOVER_$(date -u +%Y%m%dT%H%M%SZ)_$RANDOM"
CODEX_HOME="$acceptance_root/codex" OPENAI_API_KEY="client-placeholder-never-forward" \
  codex exec --json --skip-git-repo-check -s read-only \
  "Reply with exactly: $codex_marker" </dev/null \
  >"$logs/codex.jsonl" 2>"$logs/codex.stderr" || fail "Codex request through the rotating pool failed"
jq -er --arg marker "$codex_marker" 'select(.type == "item.completed" and .item.type == "agent_message") | .item.text | contains($marker)' "$logs/codex.jsonl" >/dev/null || fail "Codex response omitted its marker"

run_agentswap status --addr "$addr" >"$logs/status-final.txt"
rg -q '^invalid-openai[[:space:]].*rejected' "$logs/status-final.txt" || fail "invalid OpenAI-compatible key was not marked rejected"
rg -q '^valid-openai[[:space:]].*available' "$logs/status-final.txt" || fail "valid OpenAI-compatible key did not serve the retried request"

{
  printf '%s\n' "agentswap real credential failover acceptance"
  printf 'claude_version=%s\n' "$(claude --version)"
  printf 'codex_version=%s\n' "$(codex --version)"
  printf 'anthropic_invalid_to_valid=PASS\n'
  printf 'openai_invalid_to_valid=PASS\n'
  printf 'client_credential_replacement=PASS\n'
  printf 'isolated_pool=PASS\n'
} >"$logs/acceptance-summary.txt"

echo "PASS: real Claude request rotated from an invalid to a valid Anthropic key"
echo "PASS: real Codex request rotated from an invalid to a valid OpenAI-compatible key"
echo "PASS: client placeholder credentials never reached either upstream"
echo "acceptance summary: $logs/acceptance-summary.txt"
echo "credential-free acceptance artifacts retained at $acceptance_root"
