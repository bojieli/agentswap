#!/usr/bin/env bash

set -euo pipefail

for command in expect go jq rg; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 2
  fi
done

acceptance_alias=$(mktemp -d "${AGENTSWAP_ACCEPTANCE_TMPDIR:-/tmp}/agentswap-pty.XXXXXX")
acceptance_root=$(cd "$acceptance_alias" && pwd -P)
project="$acceptance_root/project"
claude_home="$acceptance_root/claude"
codex_home="$acceptance_root/codex"
logs="$acceptance_root/logs"
mkdir -p "$project" "$claude_home/projects" "$codex_home" "$logs"

fail() {
  echo "FAIL: $*" >&2
  echo "acceptance artifacts retained at $acceptance_root" >&2
  exit 1
}

repo=$(cd "$(dirname "$0")/.." && pwd -P)
binary="$acceptance_root/agentswap"
(cd "$repo" && go build -o "$binary" ./cmd/agentswap)

canonical_project=$(cd "$project" && pwd -P)
project_key=$(printf '%s' "$canonical_project" | LC_ALL=C sed 's/[^A-Za-z0-9-]/-/g')
project_sessions="$claude_home/projects/$project_key"
mkdir -p "$project_sessions"

older_id="11111111-1111-4111-8111-111111111111"
newer_id="22222222-2222-4222-8222-222222222222"

write_claude_fixture() {
  local id=$1 title=$2 timestamp=$3 path="$project_sessions/$1.jsonl"
  jq -cn \
    --arg id "$id" --arg cwd "$canonical_project" --arg text "$title" --arg ts "$timestamp" \
    '{type:"user",uuid:"user-1",parentUuid:null,sessionId:$id,cwd:$cwd,timestamp:$ts,slug:"pty-acceptance",message:{role:"user",content:[{type:"text",text:$text}]}}' \
    >"$path"
  jq -cn \
    --arg id "$id" --arg cwd "$canonical_project" --arg ts "$timestamp" \
    '{type:"assistant",uuid:"assistant-1",parentUuid:"user-1",sessionId:$id,cwd:$cwd,timestamp:$ts,slug:"pty-acceptance",message:{id:"msg-1",role:"assistant",model:"claude-sonnet-4-6",content:[{type:"text",text:"Fixture ready."}]}}' \
    >>"$path"
}

write_claude_fixture "$older_id" "Select the older PTY fixture" "2026-08-19T10:00:00Z"
write_claude_fixture "$newer_id" "Do not select the newer PTY fixture" "2026-08-19T10:01:00Z"
touch -t 202608191000 "$project_sessions/$older_id.jsonl"
touch -t 202608191001 "$project_sessions/$newer_id.jsonl"

export AGENTSWAP_PTY_BINARY="$binary"
export AGENTSWAP_PTY_CLAUDE_HOME="$claude_home"
export AGENTSWAP_PTY_CODEX_HOME="$codex_home"

selection_log="$logs/selection.log"
export AGENTSWAP_PTY_LOG="$selection_log"
(cd "$canonical_project" && expect -c '
  set timeout 20
  log_file -noappend $env(AGENTSWAP_PTY_LOG)
  spawn -noecho env CLAUDE_CONFIG_DIR=$env(AGENTSWAP_PTY_CLAUDE_HOME) CODEX_HOME=$env(AGENTSWAP_PTY_CODEX_HOME) $env(AGENTSWAP_PTY_BINARY) teleport codex --from claude
  expect {
    -re {Choose \[1-2\]: $} { send "2\r" }
    timeout { puts stderr "timed out waiting for the native session picker"; exit 124 }
    eof { puts stderr "picker exited before prompting"; exit 125 }
  }
  expect eof
  set status [wait]
  exit [lindex $status 3]
') || fail "valid PTY selection failed"

rg -Fq "1) claude     $newer_id" "$selection_log" || fail "picker did not list the newest cwd session first"
rg -Fq "2) claude     $older_id" "$selection_log" || fail "picker did not list the older cwd session second"
rg -Fq "Claude Code $older_id -> Codex ($canonical_project)" "$selection_log" || fail "picker did not use the selected source"
rg -Fq "Created Codex session" "$selection_log" || fail "selected PTY teleport created no target"
if rg -Fq "Claude Code $newer_id -> Codex" "$selection_log"; then
  fail "picker used the unselected source"
fi

invalid_log="$logs/invalid-selection.log"
export AGENTSWAP_PTY_LOG="$invalid_log"
set +e
(cd "$canonical_project" && expect -c '
  set timeout 20
  log_file -noappend $env(AGENTSWAP_PTY_LOG)
  spawn -noecho env CLAUDE_CONFIG_DIR=$env(AGENTSWAP_PTY_CLAUDE_HOME) CODEX_HOME=$env(AGENTSWAP_PTY_CODEX_HOME) $env(AGENTSWAP_PTY_BINARY) teleport codex --from claude
  expect {
    -re {Choose \[1-2\]: $} { send "not-a-number\r" }
    timeout { puts stderr "timed out waiting for the native session picker"; exit 124 }
    eof { puts stderr "picker exited before prompting"; exit 125 }
  }
  expect eof
  set status [wait]
  exit [lindex $status 3]
')
invalid_status=$?
set -e
[[ $invalid_status -ne 0 ]] || fail "invalid PTY selection unexpectedly succeeded"
rg -Fq "invalid session selection" "$invalid_log" || fail "invalid PTY selection did not return a clear error"

target_count=$(find "$codex_home/sessions" -type f -name '*.jsonl' | wc -l | tr -d ' ')
[[ $target_count == 1 ]] || fail "expected exactly one target after valid and invalid selections, found $target_count"

{
  printf '%s\n' "agentswap real PTY picker acceptance"
  printf 'project=%s\n' "$canonical_project"
  printf 'selected_source=%s\n' "$older_id"
  printf '%s\n' 'real_terminal_detection=PASS' 'cwd_default_discovery=PASS' \
    'interactive_selection=PASS' 'invalid_selection=PASS' 'invalid_selection_no_write=PASS'
} >"$logs/acceptance-summary.txt"

echo "PASS: a real PTY displayed and accepted the cwd-scoped session picker"
echo "PASS: the selected non-latest source alone produced a target"
echo "PASS: invalid interactive input failed without writing a target"
echo "acceptance summary: $logs/acceptance-summary.txt"
echo "acceptance artifacts retained at $acceptance_root"
