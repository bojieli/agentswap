#!/usr/bin/env bash

set -euo pipefail

if [[ ${AGENTSWAP_LIVE_ACCEPTANCE:-} != 1 ]]; then
  echo "refusing to spend provider credits; set AGENTSWAP_LIVE_ACCEPTANCE=1" >&2
  exit 2
fi

: "${AGENTSWAP_OPENCODE_MODEL:?set to an OpenCode provider/model, for example krill/gpt-5.6-sol}"
: "${OPENCODE_CONFIG_CONTENT:?set to a secret-free OpenCode config that reads its key from an environment variable}"

fail() {
  echo "FAIL: $*" >&2
  echo "acceptance artifacts retained at ${acceptance_root:-unknown}" >&2
  exit 1
}

for command in claude codex kimi opencode go jq rg shasum; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 2
  fi
done

acceptance_root=$(mktemp -d "${AGENTSWAP_ACCEPTANCE_TMPDIR:-/tmp}/agentswap-live.XXXXXX")
project="$acceptance_root/project"
logs="$acceptance_root/logs"
mkdir -p "$project" "$logs" \
  "$acceptance_root/xdg-config" "$acceptance_root/xdg-data" \
  "$acceptance_root/xdg-cache" "$acceptance_root/xdg-state"

export XDG_CONFIG_HOME="$acceptance_root/xdg-config"
export XDG_DATA_HOME="$acceptance_root/xdg-data"
export XDG_CACHE_HOME="$acceptance_root/xdg-cache"
export XDG_STATE_HOME="$acceptance_root/xdg-state"
mkdir -p "$XDG_CONFIG_HOME/opencode"
printf '%s\n' "$OPENCODE_CONFIG_CONTENT" | jq -e . >"$XDG_CONFIG_HOME/opencode/opencode.json" || fail "OPENCODE_CONFIG_CONTENT is not valid JSON"

marker="AGENTSWAP_LIVE_MARKER_$(date -u +%Y%m%dT%H%M%SZ)_$RANDOM"
printf '%s\n' "$marker" >"$project/acceptance-marker.txt"
canonical_project=$(cd "$project" && pwd -P)

repo=$(cd "$(dirname "$0")/.." && pwd -P)
binary="$acceptance_root/agentswap"
(cd "$repo" && go build -o "$binary" ./cmd/agentswap)

fixture_prompt() {
  local harness=$1 token=$2
  printf '%s' "Create a session-migration acceptance fixture. SOURCE_HARNESS=$harness. Recall token: $token. Create a native two-step plan or todo list: step 1 'Inspect marker' in progress and step 2 'Teleport and continue' pending. Use a file-reading tool to read acceptance-marker.txt and report its exact content. Then use a shell tool to run: ls deliberately-missing-$harness.txt . Preserve and report the expected failure. Update the native plan so step 1 is completed and step 2 remains pending. Do not modify files. Finish with exactly two lines, replacing <marker-from-file> with the value you read: OBSERVED source=$harness token=$token marker=<marker-from-file> missing=deliberately-missing-$harness.txt outcome=failed plan1=completed plan2=pending ; FIXTURE_READY $token"
}

continuation_prompt() {
  local target=$1 source=$2 token=$3
  printf '%s' "Continue this teleported session. Without reading session files or logs, report from prior conversation: SOURCE_HARNESS, recall token, exact marker content, intentional missing filename and failed outcome, and the latest two plan statuses. Then make a new native file-reading tool call to read acceptance-marker.txt and report its exact content. Do not modify files. End with exactly two lines, replacing <recalled-marker> with the marker recalled from the prior conversation: RECALLED source=$source token=$token marker=<recalled-marker> missing=deliberately-missing-$source.txt outcome=failed plan1=completed plan2=pending ; TARGET_CONTINUED $target FROM $source $token"
}

assistant_text() {
  local log=$1 harness=$2
  case "$harness" in
    claude)
      jq -r 'select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text' "$log"
      ;;
    codex)
      jq -r 'select(.type == "item.completed" and .item.type == "agent_message") | .item.text' "$log"
      ;;
    kimi)
      jq -r 'select(.role == "assistant") | .content // empty' "$log"
      ;;
    opencode)
      jq -r 'select(.type == "text" and .part.type == "text") | .part.text // empty' "$log"
      ;;
    *) fail "cannot extract assistant text for unknown harness $harness" ;;
  esac
}

assert_fixture_tooling() {
  local log=$1 harness=$2
  case "$harness" in
    claude)
      jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Read" and (.input | tojson | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "Claude source made no structured marker Read call"
      jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Bash" and (.input | tojson | contains("deliberately-missing-claude.txt")))' "$log" >/dev/null || fail "Claude source made no structured failing Bash call"
      jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and (.name == "TaskCreate" or .name == "TaskUpdate"))' "$log" >/dev/null || fail "Claude source made no native task-plan call"
      ;;
    codex)
      jq -s -e '([.[] | select(.type == "item.completed" and .item.type == "command_execution" and (.item.command | contains("acceptance-marker.txt")))] | length) >= 1' "$log" >/dev/null || fail "Codex source made no structured marker command"
      jq -e 'select(.type == "item.completed" and .item.type == "command_execution" and (.item.command | contains("deliberately-missing-codex.txt")) and .item.exit_code != 0)' "$log" >/dev/null || fail "Codex source did not retain a nonzero missing-file command"
      jq -e 'select(.type == "item.completed" and .item.type == "todo_list")' "$log" >/dev/null || fail "Codex source made no native plan update"
      ;;
    kimi)
      jq -e 'select(.role == "assistant") | .tool_calls[]? | select(.function.name == "Read" and (.function.arguments | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "Kimi source made no structured marker Read call"
      jq -e 'select(.role == "assistant") | .tool_calls[]? | select(.function.name == "Bash" and (.function.arguments | contains("deliberately-missing-kimi.txt")))' "$log" >/dev/null || fail "Kimi source made no structured failing Bash call"
      jq -e 'select(.role == "assistant") | .tool_calls[]? | select(.function.name == "TodoList")' "$log" >/dev/null || fail "Kimi source made no native todo update"
      ;;
    opencode)
      jq -e 'select(.type == "tool_use" and .part.tool == "read" and (.part.state.input | tojson | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "OpenCode source made no structured marker read call"
      jq -e 'select(.type == "tool_use" and .part.tool == "bash" and (.part.state.input | tojson | contains("deliberately-missing-opencode.txt")))' "$log" >/dev/null || fail "OpenCode source made no structured failing bash call"
      jq -e 'select(.type == "tool_use" and .part.tool == "todowrite")' "$log" >/dev/null || fail "OpenCode source made no native todo update"
      ;;
  esac
}

assert_fresh_read() {
  local log=$1 harness=$2
  case "$harness" in
    claude)
      jq -e 'select(.type == "assistant") | .message.content[]? | select(.type == "tool_use" and .name == "Read" and (.input | tojson | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "resumed Claude made no new structured marker Read call"
      ;;
    codex)
      jq -e 'select(.type == "item.completed" and .item.type == "command_execution" and (.item.command | contains("acceptance-marker.txt")) and .item.exit_code == 0)' "$log" >/dev/null || fail "resumed Codex made no successful new marker command"
      ;;
    kimi)
      jq -e 'select(.role == "assistant") | .tool_calls[]? | select(.function.name == "Read" and (.function.arguments | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "resumed Kimi made no new structured marker Read call"
      ;;
    opencode)
      jq -e 'select(.type == "tool_use" and .part.tool == "read" and (.part.state.input | tojson | contains("acceptance-marker.txt")))' "$log" >/dev/null || fail "resumed OpenCode made no new structured marker read call"
      ;;
  esac
}

assert_fixture() {
  local log=$1 harness=$2 token=$3
  local assistant_log="$log.assistant.txt"
  assistant_text "$log" "$harness" >"$assistant_log"
  rg -Fq "OBSERVED source=$harness token=$token marker=$marker missing=deliberately-missing-$harness.txt outcome=failed plan1=completed plan2=pending" "$assistant_log" || fail "$harness source assistant did not report the exact fixture state"
  rg -Fq "FIXTURE_READY $token" "$assistant_log" || fail "$harness source did not finish its fixture turn"
  assert_fixture_tooling "$log" "$harness"
}

assert_continuation() {
  local log=$1 target=$2 source=$3 token=$4
  local assistant_log="$log.assistant.txt"
  assistant_text "$log" "$target" >"$assistant_log"
  rg -Fq "RECALLED source=$source token=$token marker=$marker missing=deliberately-missing-$source.txt outcome=failed plan1=completed plan2=pending" "$assistant_log" || fail "$source->$target assistant did not recall the exact prior state"
  rg -Fq "TARGET_CONTINUED $target FROM $source $token" "$assistant_log" || fail "$source->$target did not finish its resumed turn"
  assert_fresh_read "$log" "$target"
}

echo "creating four native source sessions" >&2

claude_token="AS_CLAUDE_$RANDOM"
claude_log="$logs/source-claude.jsonl"
(cd "$project" && claude -p "$(fixture_prompt claude "$claude_token")" \
  --verbose --output-format stream-json \
  --max-budget-usd "${AGENTSWAP_CLAUDE_MAX_BUDGET:-1.00}" \
  --permission-mode bypassPermissions \
  --tools "Read,Bash,TaskCreate,TaskUpdate" \
  >"$claude_log" 2>"$logs/source-claude.stderr")
claude_id=$(jq -r 'select(.type == "system" and .subtype == "init") | .session_id' "$claude_log" | sed -n '1p')
[[ -n $claude_id ]] || fail "Claude did not return a session id"
assert_fixture "$claude_log" claude "$claude_token"

codex_token="AS_CODEX_$RANDOM"
codex_log="$logs/source-codex.jsonl"
(cd "$project" && codex exec --json --skip-git-repo-check -s read-only \
  "$(fixture_prompt codex "$codex_token")" </dev/null \
  >"$codex_log" 2>"$logs/source-codex.stderr")
codex_id=$(jq -r 'select(.type == "thread.started") | .thread_id' "$codex_log" | sed -n '1p')
[[ -n $codex_id ]] || fail "Codex did not return a session id"
assert_fixture "$codex_log" codex "$codex_token"

kimi_token="AS_KIMI_$RANDOM"
kimi_log="$logs/source-kimi.jsonl"
(cd "$project" && kimi --output-format stream-json \
  -p "$(fixture_prompt kimi "$kimi_token")" \
  >"$kimi_log" 2>"$logs/source-kimi.stderr")
kimi_id=$(jq -r 'select(.type == "session.resume_hint") | .session_id' "$kimi_log" | sed -n '1p')
[[ -n $kimi_id ]] || fail "Kimi did not return a session id"
assert_fixture "$kimi_log" kimi "$kimi_token"

opencode_token="AS_OPENCODE_$RANDOM"
opencode_log="$logs/source-opencode.jsonl"
(cd "$project" && opencode run --auto --format json \
  -m "$AGENTSWAP_OPENCODE_MODEL" \
  "$(fixture_prompt opencode "$opencode_token")" \
  >"$opencode_log" 2>"$logs/source-opencode.stderr")
opencode_id=$(jq -r '.sessionID // empty' "$opencode_log" | sed -n '1p')
[[ -n $opencode_id ]] || fail "OpenCode did not return a session id"
assert_fixture "$opencode_log" opencode "$opencode_token"

find_one() {
  local root=$1 pattern=$2
  find "$root" -type f -name "$pattern" -print -quit
}

digest_tree() {
  local root=$1
  find "$root" -type f -print | LC_ALL=C sort | while IFS= read -r file; do
    shasum -a 256 "$file"
  done | shasum -a 256 | awk '{print $1}'
}

claude_source=$(find_one "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects" "$claude_id.jsonl")
codex_source=$(find_one "${CODEX_HOME:-$HOME/.codex}/sessions" "*-$codex_id.jsonl")
kimi_source=$(find "${KIMI_CODE_HOME:-$HOME/.kimi-code}/sessions" -type d -name "$kimi_id" -print -quit)
[[ -f $claude_source && -f $codex_source && -d $kimi_source ]] || fail "could not resolve native source paths"

claude_before=$(shasum -a 256 "$claude_source" | awk '{print $1}')
codex_before=$(shasum -a 256 "$codex_source" | awk '{print $1}')
kimi_before=$(digest_tree "$kimi_source")
opencode_before=$(opencode export "$opencode_id" | shasum -a 256 | awk '{print $1}')

run_pair() {
  local source=$1 source_id=$2 target=$3 token=$4
  local pair="$source-to-$target"
  local teleport_log="$logs/$pair.teleport"
  local resume_log="$logs/$pair.resume.jsonl"
  echo "testing $source->$target" >&2
  (cd "$project" && "$binary" teleport "$target" --from "$source" \
    --session "$source_id" --cwd "$project" >"$teleport_log" 2>&1)

  local target_id
  target_id=$(sed -nE 's/^Created .* session ([^ ]+)$/\1/p' "$teleport_log" | sed -n '1p')
  [[ -n $target_id ]] || fail "$pair did not create a target id"

  local prompt
  prompt=$(continuation_prompt "$target" "$source" "$token")
  case "$target" in
    claude)
      (cd "$project" && claude -p "$prompt" --resume "$target_id" \
        --verbose --output-format stream-json \
        --max-budget-usd "${AGENTSWAP_CLAUDE_MAX_BUDGET:-1.00}" \
        --permission-mode bypassPermissions --tools "Read" \
        >"$resume_log" 2>"$logs/$pair.resume.stderr")
      ;;
    codex)
      (cd "$project" && codex exec resume --json --skip-git-repo-check \
        "$target_id" "$prompt" </dev/null \
        >"$resume_log" 2>"$logs/$pair.resume.stderr")
      ;;
    kimi)
      local model
      model=$(sed -nE 's/^Resume: kimi --session [^ ]+ --model ([^ ]+)$/\1/p' "$teleport_log" | sed -n '1p')
      [[ -n $model ]] || fail "$pair did not report a Kimi target model"
      (cd "$project" && kimi --session "$target_id" --model "$model" \
        --output-format stream-json -p "$prompt" \
        >"$resume_log" 2>"$logs/$pair.resume.stderr")
      ;;
    opencode)
      (cd "$project" && opencode run --auto --format json \
        --session "$target_id" -m "$AGENTSWAP_OPENCODE_MODEL" "$prompt" \
        >"$resume_log" 2>"$logs/$pair.resume.stderr")
      ;;
    *) fail "unknown target $target" ;;
  esac
  assert_continuation "$resume_log" "$target" "$source" "$token"
  printf '%s\n' "$target_id"
}

target_codex_from_claude=$(run_pair claude "$claude_id" codex "$claude_token")
target_kimi_from_claude=$(run_pair claude "$claude_id" kimi "$claude_token")
target_opencode_from_claude=$(run_pair claude "$claude_id" opencode "$claude_token")
target_claude_from_codex=$(run_pair codex "$codex_id" claude "$codex_token")
target_kimi_from_codex=$(run_pair codex "$codex_id" kimi "$codex_token")
target_opencode_from_codex=$(run_pair codex "$codex_id" opencode "$codex_token")
target_claude_from_kimi=$(run_pair kimi "$kimi_id" claude "$kimi_token")
target_codex_from_kimi=$(run_pair kimi "$kimi_id" codex "$kimi_token")
target_opencode_from_kimi=$(run_pair kimi "$kimi_id" opencode "$kimi_token")
target_claude_from_opencode=$(run_pair opencode "$opencode_id" claude "$opencode_token")
target_codex_from_opencode=$(run_pair opencode "$opencode_id" codex "$opencode_token")
target_kimi_from_opencode=$(run_pair opencode "$opencode_id" kimi "$opencode_token")

claude_after=$(shasum -a 256 "$claude_source" | awk '{print $1}')
codex_after=$(shasum -a 256 "$codex_source" | awk '{print $1}')
kimi_after=$(digest_tree "$kimi_source")
opencode_after=$(opencode export "$opencode_id" | shasum -a 256 | awk '{print $1}')

[[ $claude_before == "$claude_after" ]] || fail "Claude source changed"
[[ $codex_before == "$codex_after" ]] || fail "Codex source changed"
[[ $kimi_before == "$kimi_after" ]] || fail "Kimi source changed"
[[ $opencode_before == "$opencode_after" ]] || fail "OpenCode source changed"

opencode_list="$logs/opencode-session-list.json"
(cd "$project" && opencode session list --format json >"$opencode_list")
for id in "$target_opencode_from_claude" "$target_opencode_from_codex" "$target_opencode_from_kimi"; do
  jq -e --arg id "$id" --arg cwd "$canonical_project" '[.[] | select(.id == $id and .directory == $cwd)] | length == 1' "$opencode_list" >/dev/null || fail "OpenCode cwd list omitted $id for $canonical_project"
done

claude_key=$(printf '%s' "$canonical_project" | LC_ALL=C sed 's/[^A-Za-z0-9-]/-/g')
for id in "$target_claude_from_codex" "$target_claude_from_kimi" "$target_claude_from_opencode"; do
  [[ -f ${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/$claude_key/$id.jsonl ]] || fail "Claude cwd index omitted $id"
done
for id in "$target_codex_from_claude" "$target_codex_from_kimi" "$target_codex_from_opencode"; do
  codex_target=$(find_one "${CODEX_HOME:-$HOME/.codex}/sessions" "*-$id.jsonl")
  [[ -n $codex_target ]] || fail "Codex cwd target is missing: $id"
  jq -e --arg cwd "$canonical_project" 'select(.type == "session_meta") | .payload.cwd == $cwd' "$codex_target" >/dev/null || fail "Codex cwd target has incorrect cwd: $id"
done
for id in "$target_kimi_from_claude" "$target_kimi_from_codex" "$target_kimi_from_opencode"; do
  kimi_target=$(find "${KIMI_CODE_HOME:-$HOME/.kimi-code}/sessions" -type d -name "$id" -print -quit)
  [[ -n $kimi_target ]] || fail "Kimi cwd target is missing: $id"
  jq -e --arg cwd "$canonical_project" '.cwd == $cwd or .workDir == $cwd' "$kimi_target/state.json" >/dev/null || fail "Kimi cwd target has incorrect cwd: $id"
done

summary="$logs/acceptance-summary.txt"
{
  printf '%s\n' "agentswap live teleport acceptance"
  printf 'project=%s\nmarker=%s\n' "$canonical_project" "$marker"
  printf 'claude_version=%s\n' "$(claude --version)"
  printf 'codex_version=%s\n' "$(codex --version)"
  printf 'kimi_version=%s\n' "$(kimi --version)"
  printf 'opencode_version=%s\n' "$(opencode --version)"
  printf 'source_claude=%s\nsource_codex=%s\nsource_kimi=%s\nsource_opencode=%s\n' "$claude_id" "$codex_id" "$kimi_id" "$opencode_id"
  printf 'claude_digest_before=%s\nclaude_digest_after=%s\n' "$claude_before" "$claude_after"
  printf 'codex_digest_before=%s\ncodex_digest_after=%s\n' "$codex_before" "$codex_after"
  printf 'kimi_digest_before=%s\nkimi_digest_after=%s\n' "$kimi_before" "$kimi_after"
  printf 'opencode_digest_before=%s\nopencode_digest_after=%s\n' "$opencode_before" "$opencode_after"
  printf '%s\n' "matrix=PASS"
  for pair in \
    "claude->codex:$target_codex_from_claude" "claude->kimi:$target_kimi_from_claude" "claude->opencode:$target_opencode_from_claude" \
    "codex->claude:$target_claude_from_codex" "codex->kimi:$target_kimi_from_codex" "codex->opencode:$target_opencode_from_codex" \
    "kimi->claude:$target_claude_from_kimi" "kimi->codex:$target_codex_from_kimi" "kimi->opencode:$target_opencode_from_kimi" \
    "opencode->claude:$target_claude_from_opencode" "opencode->codex:$target_codex_from_opencode" "opencode->kimi:$target_kimi_from_opencode"; do
    printf 'pair_%s=PASS\n' "${pair%%:*}"
    printf 'target_%s=%s\n' "${pair%%:*}" "${pair#*:}"
  done
} >"$summary"

echo "PASS: all 12 directed teleports completed a real resumed tool turn"
echo "PASS: all four source digests remained unchanged"
echo "PASS: target sessions are present in native cwd-scoped storage"
echo "acceptance summary: $summary"
echo "acceptance artifacts retained at $acceptance_root"
