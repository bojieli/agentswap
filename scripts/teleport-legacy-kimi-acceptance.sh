#!/usr/bin/env bash

set -euo pipefail

if [[ ${AGENTSWAP_LIVE_ACCEPTANCE:-} != 1 ]]; then
  echo "refusing to spend provider credits; set AGENTSWAP_LIVE_ACCEPTANCE=1" >&2
  exit 2
fi

: "${AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY:?set to the Anthropic key used only by this isolated test}"
: "${AGENTSWAP_KIMI_LEGACY_OPENAI_KEY:?set to the OpenAI-compatible key used only by this isolated test}"
: "${AGENTSWAP_KIMI_LEGACY_OPENAI_BASE_URL:?set to the OpenAI-compatible chat-completions base URL}"
: "${AGENTSWAP_KIMI_LEGACY_OPENAI_MODEL:?set to the OpenAI-compatible chat-completions model}"

for command in claude go jq rg shasum uvx; do
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
claude_home="$acceptance_root/claude"
kimi_share="$acceptance_root/kimi"
mkdir -p "$project" "$logs" "$claude_home" "$kimi_share"

cleanup() {
  # Keys are passed only through the environment. Fail closed if a client
  # unexpectedly wrote one: remove the affected artifact instead of retaining
  # a secret on disk.
  if [[ -d ${acceptance_root:-} ]]; then
    for secret in "${AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY:-}" "${AGENTSWAP_KIMI_LEGACY_OPENAI_KEY:-}"; do
      [[ -n $secret ]] || continue
      {
        rg -l -F -- "$secret" "$acceptance_root" 2>/dev/null || true
      } | while IFS= read -r leaked; do
        rm -f "$leaked"
      done
    done
  fi
}
trap cleanup EXIT INT TERM

fail() {
  echo "FAIL: $*" >&2
  echo "credential-free acceptance artifacts retained at $acceptance_root" >&2
  exit 1
}

digest_tree() {
  local root=$1
  find "$root" -type f -print | LC_ALL=C sort | while IFS= read -r file; do
    shasum -a 256 "$file"
  done | shasum -a 256 | awk '{print $1}'
}

repo=$(cd "$(dirname "$0")/.." && pwd -P)
binary="$acceptance_root/agentswap"
(cd "$repo" && go build -o "$binary" ./cmd/agentswap)

marker="AGENTSWAP_LEGACY_KIMI_MARKER_$(date -u +%Y%m%dT%H%M%SZ)_$RANDOM"
token="AS_LEGACY_KIMI_$RANDOM"
printf '%s\n' "$marker" >"$project/acceptance-marker.txt"
canonical_project=$(cd "$project" && pwd -P)

kimi_config="$acceptance_root/kimi-config.json"
jq -n \
  --arg base "$AGENTSWAP_KIMI_LEGACY_OPENAI_BASE_URL" \
  --arg model "$AGENTSWAP_KIMI_LEGACY_OPENAI_MODEL" \
  '{
    default_model: "acceptance",
    default_thinking: false,
    telemetry: false,
    providers: {
      acceptance: {
        type: "openai_legacy",
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

fixture_prompt="Create a session-migration acceptance fixture. SOURCE_HARNESS=claude. Recall token: $token. Create a native two-step plan or task list: step 1 'Inspect marker' in progress and step 2 'Teleport and continue' pending. Use a file-reading tool to read acceptance-marker.txt and report its exact content. Then use a shell tool to run: ls deliberately-missing-claude.txt . Preserve and report the expected failure. Update the native plan so step 1 is completed and step 2 remains pending. Do not modify files. Finish with exactly two lines, replacing <marker-from-file> with the value you read: OBSERVED source=claude token=$token marker=<marker-from-file> missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending ; FIXTURE_READY $token"

source_log="$logs/source-claude.jsonl"
(cd "$canonical_project" && \
  CLAUDE_CONFIG_DIR="$claude_home" \
  ANTHROPIC_API_KEY="$AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  claude -p "$fixture_prompt" --verbose --output-format stream-json \
    --max-budget-usd "${AGENTSWAP_CLAUDE_MAX_BUDGET:-1.00}" \
    --permission-mode bypassPermissions --tools "Read,Bash,TaskCreate,TaskUpdate" \
    >"$source_log" 2>"$logs/source-claude.stderr") || fail "Claude source fixture failed"

source_id=$(jq -r 'select(.type == "system" and .subtype == "init") | .session_id' "$source_log" | sed -n '1p')
[[ -n $source_id ]] || fail "Claude did not return a source session id"
jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Read" and (.input | tojson | contains("acceptance-marker.txt")))' "$source_log" >/dev/null || fail "Claude made no structured marker Read call"
jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Bash" and (.input | tojson | contains("deliberately-missing-claude.txt")))' "$source_log" >/dev/null || fail "Claude made no structured failing Bash call"
jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and (.name == "TaskCreate" or .name == "TaskUpdate"))' "$source_log" >/dev/null || fail "Claude made no native task-plan call"
jq -r 'select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text' "$source_log" >"$logs/source-claude.assistant.txt"
rg -Fq "OBSERVED source=claude token=$token marker=$marker missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending" "$logs/source-claude.assistant.txt" || fail "Claude did not report the exact source fixture"
rg -Fq "FIXTURE_READY $token" "$logs/source-claude.assistant.txt" || fail "Claude did not finish its source fixture"

source_path=$(find "$claude_home/projects" -type f -name "$source_id.jsonl" -print -quit)
[[ -f $source_path ]] || fail "could not resolve the Claude source session"
source_before=$(shasum -a 256 "$source_path" | awk '{print $1}')

teleport_log="$logs/teleport-claude-to-kimi.txt"
(cd "$canonical_project" && \
  CLAUDE_CONFIG_DIR="$claude_home" \
  KIMI_SHARE_DIR="$kimi_share" \
  AGENTSWAP_KIMI_FORMAT=legacy \
  "$binary" teleport kimi --from claude --session "$source_id" --cwd "$canonical_project" \
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

continuation_prompt="Continue this imported session. Without reading session files or logs, report from prior history: source harness claude, recall token $token, exact marker, missing filename, failed result, and the latest two plan statuses. Then make a new native ReadFile tool call to read acceptance-marker.txt and report its exact content. Do not modify files. End with exactly two lines: RECALLED source=claude token=$token marker=$marker missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending ; LEGACY_KIMI_CONTINUED $token"

resume_log="$logs/resume-kimi.jsonl"
(cd "$canonical_project" && \
  KIMI_SHARE_DIR="$kimi_share" \
  KIMI_CLI_NO_AUTO_UPDATE=1 \
  OPENAI_API_KEY="$AGENTSWAP_KIMI_LEGACY_OPENAI_KEY" \
  OPENAI_BASE_URL="$AGENTSWAP_KIMI_LEGACY_OPENAI_BASE_URL" \
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
rg -Fq "RECALLED source=claude token=$token marker=$marker missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending" "$logs/resume-kimi.assistant.txt" || fail "legacy Kimi did not recall the exact imported state"
rg -Fq "LEGACY_KIMI_CONTINUED $token" "$logs/resume-kimi.assistant.txt" || fail "legacy Kimi did not finish its resumed turn"

source_after=$(shasum -a 256 "$source_path" | awk '{print $1}')
[[ $source_before == "$source_after" ]] || fail "Claude source session changed during teleport or continuation"

legacy_before=$(digest_tree "$target_dir")
return_teleport_log="$logs/teleport-kimi-to-claude.txt"
(cd "$canonical_project" && \
  CLAUDE_CONFIG_DIR="$claude_home" \
  KIMI_SHARE_DIR="$kimi_share" \
  "$binary" teleport claude --from kimi --session "$target_id" --cwd "$canonical_project" \
    >"$return_teleport_log" 2>&1) || fail "agentswap could not read the real legacy Kimi source"

return_id=$(sed -nE 's/^Created Claude Code session ([^ ]+)$/\1/p' "$return_teleport_log" | sed -n '1p')
[[ -n $return_id ]] || fail "agentswap did not report the returned Claude target id"
rg -Fq "Resume: claude --resume $return_id" "$return_teleport_log" || fail "agentswap did not report the returned Claude resume command"

return_prompt="Continue this session after it passed through Python-era Kimi. Without reading session files or logs, recall the original source claude, token $token, marker $marker, missing filename deliberately-missing-claude.txt, failed result, completed/pending plan states, and that legacy Kimi continued it. Then make a new native Read tool call to read acceptance-marker.txt. Do not modify files. End with exactly two lines: ROUNDTRIP_RECALLED source=claude via=legacy-kimi token=$token marker=$marker missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending ; CLAUDE_RETURNED $token"

return_log="$logs/resume-returned-claude.jsonl"
(cd "$canonical_project" && \
  CLAUDE_CONFIG_DIR="$claude_home" \
  ANTHROPIC_API_KEY="$AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  claude -p "$return_prompt" --resume "$return_id" \
    --verbose --output-format stream-json \
    --max-budget-usd "${AGENTSWAP_CLAUDE_MAX_BUDGET:-1.00}" \
    --permission-mode bypassPermissions --tools "Read" \
    >"$return_log" 2>"$logs/resume-returned-claude.stderr") || fail "Claude could not resume the session imported from legacy Kimi"

jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Read" and (.input | tojson | contains("acceptance-marker.txt")))' "$return_log" >/dev/null || fail "returned Claude made no fresh marker Read call"
jq -r 'select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text' "$return_log" >"$logs/resume-returned-claude.assistant.txt"
rg -Fq "ROUNDTRIP_RECALLED source=claude via=legacy-kimi token=$token marker=$marker missing=deliberately-missing-claude.txt outcome=failed plan1=completed plan2=pending" "$logs/resume-returned-claude.assistant.txt" || fail "returned Claude did not recall the exact legacy Kimi history"
rg -Fq "CLAUDE_RETURNED $token" "$logs/resume-returned-claude.assistant.txt" || fail "returned Claude did not finish its resumed turn"

return_path=$(find "$claude_home/projects" -type f -name "$return_id.jsonl" -print -quit)
[[ -f $return_path ]] || fail "returned Claude target session is missing"
jq -e --arg cwd "$canonical_project" 'select((.type == "user" or .type == "assistant") and .cwd == $cwd)' "$return_path" >/dev/null || fail "returned Claude target has the wrong cwd"
legacy_after=$(digest_tree "$target_dir")
[[ $legacy_before == "$legacy_after" ]] || fail "legacy Kimi source changed while teleporting it to Claude"

for secret in "$AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY" "$AGENTSWAP_KIMI_LEGACY_OPENAI_KEY"; do
  if rg -l -F -- "$secret" "$acceptance_root" >/dev/null 2>&1; then
    fail "a client persisted a provider credential"
  fi
done

{
  printf '%s\n' "agentswap legacy Kimi live acceptance"
  printf 'project=%s\nmarker=%s\n' "$canonical_project" "$marker"
  printf 'claude_version=%s\n' "$(claude --version)"
  printf 'kimi_cli_version=%s\n' "$(uvx --from kimi-cli kimi --version)"
  printf 'source_claude=%s\ntarget_legacy_kimi=%s\ntarget_returned_claude=%s\n' "$source_id" "$target_id" "$return_id"
  printf 'claude_digest_before=%s\nclaude_digest_after=%s\n' "$source_before" "$source_after"
  printf 'legacy_digest_before=%s\nlegacy_digest_after=%s\n' "$legacy_before" "$legacy_after"
  printf '%s\n' 'structured_source_tools=PASS' 'legacy_registry=PASS' \
    'claude_to_legacy_kimi=PASS' 'legacy_kimi_to_claude=PASS' \
    'exact_history_recall=PASS' 'fresh_native_tools=PASS' \
    'sources_immutable=PASS' 'credential_retention=PASS'
} >"$logs/acceptance-summary.txt"

echo "PASS: Python-era Kimi resumed an agentswap-generated legacy session"
echo "PASS: exact Claude history, tool failure, and plan state were retained"
echo "PASS: legacy Kimi made a fresh native ReadFile call"
echo "PASS: Claude resumed the real legacy Kimi session and made a fresh Read call"
echo "PASS: both native source digests remained unchanged during their teleports"
echo "acceptance summary: $logs/acceptance-summary.txt"
echo "credential-free acceptance artifacts retained at $acceptance_root"
