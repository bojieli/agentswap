#!/usr/bin/env bash

set -euo pipefail

if [[ ${AGENTSWAP_LIVE_ACCEPTANCE:-} != 1 ]]; then
  echo "refusing to spend provider credits; set AGENTSWAP_LIVE_ACCEPTANCE=1" >&2
  exit 2
fi

: "${AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY:?set to the OpenAI-compatible key used only by this isolated test}"
: "${AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_BASE_URL:?set to the OpenAI-compatible provider base URL}"
: "${AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_MODEL:?set to the OpenAI-compatible provider model}"

for command in codex go jq rg shasum uvx; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 2
  fi
done

acceptance_alias=$(mktemp -d "${AGENTSWAP_ACCEPTANCE_TMPDIR:-/tmp}/agentswap-legacy-kimi.XXXXXX")
# Python-era Kimi hashes the spelling of the work directory. Resolve /tmp to
# /private/tmp on macOS so agentswap and Kimi use the same registry key.
acceptance_root=$(cd "$acceptance_alias" && pwd -P)
project="$acceptance_root/project"
logs="$acceptance_root/logs"
codex_home="$acceptance_root/codex"
kimi_share="$acceptance_root/kimi"
mkdir -p "$project" "$logs" "$codex_home" "$kimi_share"

cleanup() {
  # The key is passed only through the environment. Fail closed if a client
  # unexpectedly wrote it: remove the affected artifact instead of retaining
  # a secret on disk.
  if [[ -d ${acceptance_root:-} && -n ${AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY:-} ]]; then
    {
      rg -l -F -- "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY" "$acceptance_root" 2>/dev/null || true
    } | while IFS= read -r leaked; do
      rm -f "$leaked"
    done
  fi
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $*" >&2
  echo "credential-free acceptance artifacts retained at $acceptance_root" >&2
  exit 1
}

repo=$(cd "$(dirname "$0")/.." && pwd -P)
binary="$acceptance_root/agentswap"
(cd "$repo" && go build -o "$binary" ./cmd/agentswap)

marker="AGENTSWAP_LEGACY_KIMI_MARKER_$(date -u +%Y%m%dT%H%M%SZ)_$RANDOM"
token="AS_LEGACY_KIMI_$RANDOM"
printf '%s\n' "$marker" >"$project/acceptance-marker.txt"
canonical_project=$(cd "$project" && pwd -P)

{
  printf 'model = "%s"\n' "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_MODEL"
  printf '%s\n' 'model_provider = "legacy_kimi_acceptance"'
  printf '%s\n' '' '[model_providers.legacy_kimi_acceptance]'
  printf '%s\n' 'name = "legacy Kimi acceptance"'
  printf 'base_url = "%s"\n' "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_BASE_URL"
  printf '%s\n' 'env_key = "OPENAI_API_KEY"' 'wire_api = "responses"'
} >"$codex_home/config.toml"

kimi_config="$acceptance_root/kimi-config.json"
jq -n \
  --arg base "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_BASE_URL" \
  --arg model "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_MODEL" \
  '{
    default_model: "acceptance",
    default_thinking: false,
    telemetry: false,
    providers: {
      acceptance: {
        type: "openai_responses",
        base_url: $base,
        api_key: "placeholder-from-OPENAI_API_KEY"
      }
    },
    models: {
      acceptance: {
        provider: "acceptance",
        model: $model,
        max_context_size: 200000,
        capabilities: ["thinking"]
      }
    }
  }' >"$kimi_config"

fixture_prompt="Create a session-migration acceptance fixture. SOURCE_HARNESS=codex. Recall token: $token. Create a native two-step plan: step 1 'Inspect marker' in progress and step 2 'Teleport and continue' pending. Use a command tool to read acceptance-marker.txt and report its exact content. Then use a command tool to run: ls deliberately-missing-codex.txt . Preserve and report the expected failure. Update the native plan so step 1 is completed and step 2 remains pending. Do not modify files. Finish with exactly two lines, replacing <marker-from-file> with the value you read: OBSERVED source=codex token=$token marker=<marker-from-file> missing=deliberately-missing-codex.txt outcome=failed plan1=completed plan2=pending ; FIXTURE_READY $token"

source_log="$logs/source-codex.jsonl"
(cd "$canonical_project" && \
  CODEX_HOME="$codex_home" \
  OPENAI_API_KEY="$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY" \
  codex exec --json --skip-git-repo-check -s read-only "$fixture_prompt" </dev/null \
    >"$source_log" 2>"$logs/source-codex.stderr") || fail "Codex source fixture failed"

source_id=$(jq -r 'select(.type == "thread.started") | .thread_id' "$source_log" | sed -n '1p')
[[ -n $source_id ]] || fail "Codex did not return a source session id"
jq -e 'select(.type == "item.completed" and .item.type == "command_execution" and (.item.command | contains("acceptance-marker.txt")) and .item.exit_code == 0)' "$source_log" >/dev/null || fail "Codex made no successful structured marker command"
jq -e 'select(.type == "item.completed" and .item.type == "command_execution" and (.item.command | contains("deliberately-missing-codex.txt")) and .item.exit_code != 0)' "$source_log" >/dev/null || fail "Codex made no structured failing command"
jq -e 'select(.type == "item.completed" and .item.type == "todo_list")' "$source_log" >/dev/null || fail "Codex made no native plan update"
jq -r 'select(.type == "item.completed" and .item.type == "agent_message") | .item.text' "$source_log" >"$logs/source-codex.assistant.txt"
rg -Fq "OBSERVED source=codex token=$token marker=$marker missing=deliberately-missing-codex.txt outcome=failed plan1=completed plan2=pending" "$logs/source-codex.assistant.txt" || fail "Codex did not report the exact source fixture"
rg -Fq "FIXTURE_READY $token" "$logs/source-codex.assistant.txt" || fail "Codex did not finish its source fixture"

source_path=$(find "$codex_home/sessions" -type f -name "*-$source_id.jsonl" -print -quit)
[[ -f $source_path ]] || fail "could not resolve the Codex source session"
source_before=$(shasum -a 256 "$source_path" | awk '{print $1}')

teleport_log="$logs/teleport.txt"
(cd "$canonical_project" && \
  CODEX_HOME="$codex_home" \
  KIMI_SHARE_DIR="$kimi_share" \
  AGENTSWAP_KIMI_FORMAT=legacy \
  "$binary" teleport kimi --from codex --session "$source_id" --cwd "$canonical_project" \
    >"$teleport_log" 2>&1) || fail "agentswap could not create the legacy Kimi target"

target_id=$(sed -nE 's/^Created Kimi Code session ([^ ]+)$/\1/p' "$teleport_log" | sed -n '1p')
[[ -n $target_id ]] || fail "agentswap did not report a legacy Kimi target id"
rg -Fq "Resume: kimi -r $target_id" "$teleport_log" || fail "agentswap did not report the legacy resume command"

target_dir=$(find "$kimi_share/sessions" -type d -name "$target_id" -print -quit)
[[ -d $target_dir ]] || fail "legacy Kimi target directory is missing"
for native_file in context.jsonl state.json wire.jsonl; do
  [[ -f $target_dir/$native_file ]] || fail "legacy Kimi target omitted $native_file"
done
jq -e --arg cwd "$canonical_project" --arg id "$target_id" '
  [.work_dirs[]? | select(.path == $cwd and .kaos == "local" and .last_session_id == $id)] | length == 1
' "$kimi_share/kimi.json" >/dev/null || fail "legacy kimi.json does not index the target under its canonical cwd"

continuation_prompt="Continue this imported session. Without reading session files or logs, report from prior history: source harness codex, recall token $token, exact marker, missing filename, failed result, and the latest two plan statuses. Then make a new native ReadFile tool call to read acceptance-marker.txt and report its exact content. Do not modify files. End with exactly two lines: RECALLED source=codex token=$token marker=$marker missing=deliberately-missing-codex.txt outcome=failed plan1=completed plan2=pending ; LEGACY_KIMI_CONTINUED $token"

resume_log="$logs/resume-kimi.jsonl"
(cd "$canonical_project" && \
  KIMI_SHARE_DIR="$kimi_share" \
  KIMI_CLI_NO_AUTO_UPDATE=1 \
  OPENAI_API_KEY="$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY" \
  OPENAI_BASE_URL="$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_BASE_URL" \
  uvx --from kimi-cli kimi --work-dir "$canonical_project" \
    --session "$target_id" --config-file "$kimi_config" --no-thinking \
    --yolo --print --output-format stream-json --prompt "$continuation_prompt" \
    >"$resume_log" 2>"$logs/resume-kimi.stderr") || fail "Python-era Kimi could not resume the generated target"

jq -e 'select(.role == "assistant") | .tool_calls[]? | select(.function.name == "ReadFile" and (.function.arguments | contains("acceptance-marker.txt")))' "$resume_log" >/dev/null || fail "resumed legacy Kimi made no native ReadFile call"
jq -r '
  select(.role == "assistant") |
  if (.content | type) == "string" then .content
  else .content[]? | select(.type == "text") | .text
  end
' "$resume_log" >"$logs/resume-kimi.assistant.txt"
rg -Fq "RECALLED source=codex token=$token marker=$marker missing=deliberately-missing-codex.txt outcome=failed plan1=completed plan2=pending" "$logs/resume-kimi.assistant.txt" || fail "legacy Kimi did not recall the exact imported state"
rg -Fq "LEGACY_KIMI_CONTINUED $token" "$logs/resume-kimi.assistant.txt" || fail "legacy Kimi did not finish its resumed turn"

source_after=$(shasum -a 256 "$source_path" | awk '{print $1}')
[[ $source_before == "$source_after" ]] || fail "Codex source session changed during teleport or continuation"

if rg -l -F -- "$AGENTSWAP_KIMI_LEGACY_ACCEPTANCE_KEY" "$acceptance_root" >/dev/null 2>&1; then
  fail "a client persisted the provider credential"
fi

{
  printf '%s\n' "agentswap legacy Kimi live acceptance"
  printf 'project=%s\nmarker=%s\n' "$canonical_project" "$marker"
  printf 'codex_version=%s\n' "$(codex --version)"
  printf 'kimi_cli_version=%s\n' "$(uvx --from kimi-cli kimi --version)"
  printf 'source_codex=%s\ntarget_legacy_kimi=%s\n' "$source_id" "$target_id"
  printf 'source_digest_before=%s\nsource_digest_after=%s\n' "$source_before" "$source_after"
  printf '%s\n' 'structured_source_tools=PASS' 'legacy_registry=PASS' \
    'exact_history_recall=PASS' 'fresh_readfile=PASS' 'source_immutable=PASS' \
    'credential_retention=PASS'
} >"$logs/acceptance-summary.txt"

echo "PASS: Python-era Kimi resumed an agentswap-generated legacy session"
echo "PASS: exact Codex history, tool failure, and plan state were retained"
echo "PASS: legacy Kimi made a fresh native ReadFile call"
echo "PASS: the Codex source digest remained unchanged"
echo "acceptance summary: $logs/acceptance-summary.txt"
echo "credential-free acceptance artifacts retained at $acceptance_root"
