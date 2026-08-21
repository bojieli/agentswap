# Live teleport acceptance

This is an opt-in, credit-consuming compatibility test for maintainers. It is
not required for normal `teleport` or `handoff` use. Read the user guide in
[sessions.md](sessions.md) first.

The opt-in harness at [`scripts/teleport-live-acceptance.sh`](../scripts/teleport-live-acceptance.sh)
exercises real native sessions. It creates one source session in each harness,
teleports every source to each of the other three targets, resumes every target
with the native CLI, and checks the resulting tool call. It does not replace a
transcript with a summary prompt. It does not cover `--compact`, which is
checked separately below.

## Verified run

The following run completed on 2026-08-19 against the Krill AI
OpenAI-compatible endpoint. The provider key was supplied through
`KRILL_API_KEY`; no key was written to the repository or included in the
captured output.

| Harness | Version | Role |
| --- | --- | --- |
| Claude Code | 2.1.235 | source and target |
| Codex CLI | 0.148.0 | source and target |
| Kimi Code | 0.37.2 | source and target |
| OpenCode | 1.18.18 | source and target |

The run used a temporary project directory and a unique marker file. Each
source model was required to create its native plan/todo state, read the marker
with its own file tool, run an intentionally failing `ls`, preserve that failed
result, and report a unique recall token. Each resumed target had to recall all
of that state exactly and then make a fresh native read of the marker.

| Source → target | Result |
| --- | --- |
| Claude → Codex | PASS |
| Claude → Kimi | PASS |
| Claude → OpenCode | PASS |
| Codex → Claude | PASS |
| Codex → Kimi | PASS |
| Codex → OpenCode | PASS |
| Kimi → Claude | PASS |
| Kimi → Codex | PASS |
| Kimi → OpenCode | PASS |
| OpenCode → Claude | PASS |
| OpenCode → Codex | PASS |
| OpenCode → Kimi | PASS |

All 12 directions completed a real resumed model turn. The target-specific
native calls observed were Claude `Read`, Codex command execution, Kimi `Read`,
and OpenCode `read`. The source transcripts remained byte-for-byte unchanged;
the acceptance harness compares a digest before and after the complete matrix.

The same canonical project directory was also verified in each native store:

- Claude targets were present under the Claude project key used by its current
  cwd picker.
- Codex target `session_meta` records contained the canonical cwd and were
  visible to the cwd-filtered resume picker.
- Kimi target `state.json` records contained the canonical cwd and appeared in
  the native session picker.
- OpenCode `session list --format json` returned every generated target with the
  canonical directory.

The source digests from this run were:

| Native source | SHA-256 digest |
| --- | --- |
| Claude JSONL | `cb49d58e14d6195ae81dd6e14580018a70473484243fbc51910bb7b7ca2bec8b` |
| Codex JSONL | `c00c44c572b8fb45a947f351579325e5d31592c95d79d16e1102d8d1bec5f49d` |
| Kimi session tree | `84012b7a8052c0c7876be90197fa35fc32c20e8300358a5eaf8ee9fdb66ae69c` |
| OpenCode export | `265b1b113ca099df2a71227892ca409751173cb1454990cd0483415ceb1d3f27` |

These are evidence values, not identifiers used for resumption. The script
records both the before and after values in its retained
`logs/acceptance-summary.txt` file.

## Running it

This test intentionally consumes provider credits and creates 16 real sessions
(four sources plus twelve targets). It is disabled unless the explicit safety
variable is set. OpenCode receives a secret-free JSON config whose API key is
resolved from the environment:

```sh
AGENTSWAP_LIVE_ACCEPTANCE=1 \
AGENTSWAP_OPENCODE_MODEL='krill/gpt-5.6-sol' \
OPENCODE_CONFIG_CONTENT='{"model":"krill/gpt-5.6-sol","provider":{"krill":{"npm":"@ai-sdk/openai","options":{"baseURL":"https://api.krill-ai.net/codex/v1","apiKey":"{env:KRILL_API_KEY}"},"models":{"gpt-5.6-sol":{"reasoning":true}}}}}' \
scripts/teleport-live-acceptance.sh
```

The script requires `claude`, `codex`, `kimi`, `opencode`, `go`, `jq`, `rg`,
and `shasum`. Before running it, use `agentswap install`, put at least one
account in the OpenAI lane, and start `agentswap serve`; Codex target resumes
use the target installation's native provider configuration. It builds the current `agentswap`
binary, isolates OpenCode's XDG state in a temporary directory, retains all
sanitized logs for inspection, and prints the artifact directory and summary
path. A failed assertion exits non-zero and retains the same artifacts. The
retained logs are intentionally raw for debugging; do not publish them because
native session logs can contain conversation content.

The test is an acceptance check for the currently installed CLI versions.

## Compaction check

`--compact` has one property no unit test can settle: whether a resumed agent
actually *opens* the archive when the transcript tells it to. The reduction
itself is covered offline — `internal/session` asserts that a compacted session
still validates at any budget, that markers name files that exist, and that the
archive holds the complete original — but a model ignoring a pointer looks
exactly like a model that had no pointer.

The following ran on 2026-08-21 against Claude Code 2.1.238. Each case built a
real Claude Code session, planted a unique token deep inside a tool result,
compacted the transfer hard enough to remove it, **deleted the local file the
token came from** so the archive was the only remaining copy, and then resumed
the target and asked for the token.

Deleting the source file is the part that makes the check mean anything. A
first attempt left it in place, and the resumed model answered correctly by
simply re-running `cat build.log` — a pass that proved nothing.

| Case | Reduction | Archive location | Permissions | Result |
| --- | --- | --- | --- | --- |
| Truncated tool result, Claude → Codex → Claude | 6.9k → 1.1k tokens, one result truncated | outside the project | `bypassPermissions` | PASS — read the exact shard named by the marker |
| Collapsed turns, Claude → Codex → Claude | 7.3k → 1.4k tokens, 12 results truncated and 19 turns collapsed | outside the project | `bypassPermissions` | PASS — followed the collapse brief into the archive |
| Collapsed turns | same | outside the project | default | **Read denied** — the model explained the elision, named the archive path, asked for access, and refused to guess |
| Collapsed turns | same | `<project>/.agentswap` | default | PASS — read the shard with no extra permission |
| Truncated tool result | 6.9k → 1.1k tokens | `<project>/.agentswap` | default | PASS — went straight to the archived shard |

The third row is the finding that shaped the feature, and the reason the archive
now defaults into the project. A coding agent is normally confined to its
working directory, so an archive kept in agentswap's own configuration directory
cannot be read without the user granting access, and a non-interactive resume
has nobody to ask. The behavior on denial was correct and careful — the model
said what was missing, named the file, and declined to invent an answer — but
the answer was still unavailable.

The first two rows were run before that change, with the archive outside the
project and permission checks bypassed; they establish that the marker itself is
followed. The last two are the shipped default. `--archive-dir` still moves the
archive elsewhere for anyone who wants it out of the working tree, and
`teleport` prints a hint saying the target will need a grant when it does.

Codex was used as an intermediate file format only, never resumed, so this
check needs no Codex credentials. Kimi Code could not be included: the account
reached its usage limit for the billing cycle, which is the condition agentswap
is built for and not something the check can work around.

## Python-era Kimi round trip

The separate opt-in
[`teleport-legacy-kimi-acceptance.sh`](../scripts/teleport-legacy-kimi-acceptance.sh)
completed a real bidirectional round trip with Claude Code 2.1.235 and
Python-era `kimi-cli` 1.49.0:

1. Claude created native plan state, read a unique marker, and retained an
   intentional failed tool result.
2. Agentswap wrote a legacy Kimi session and its cwd registry entry.
3. Python Kimi resumed that exact id, recalled every expected field, and made a
   fresh native `ReadFile` call.
4. Agentswap read the now-natively-resumed legacy session and wrote a new
   Claude session.
5. Claude resumed it, recalled the complete history through Kimi, and made a
   fresh native `Read` call.

The run passed in both directions. Its source digests remained identical:

| Native source | Before | After |
| --- | --- | --- |
| Claude JSONL | `c11ee75de57b4b7c474f0204bfec2fd0366e4ff6c6d51fcfe38f47f93fb32d36` | `c11ee75de57b4b7c474f0204bfec2fd0366e4ff6c6d51fcfe38f47f93fb32d36` |
| Legacy Kimi session tree | `507275e9c7b773ac605443989f892ad6622e3a681c2cfcb5c1befdbcd2013507` | `507275e9c7b773ac605443989f892ad6622e3a681c2cfcb5c1befdbcd2013507` |

This run also caught a format detail absent from newly generated fixtures:
after a native resume, Python Kimi materializes `_system_prompt`, `_checkpoint`,
and `_usage` records. They are runtime metadata, not conversation messages. The
reader now ignores those three known roles while continuing to reject an
unknown future role.

The legacy run uses separate Anthropic and OpenAI-compatible chat-completions
keys. Both are supplied only through the environment, and the script scans its
isolated artifacts before declaring success:

```sh
AGENTSWAP_LIVE_ACCEPTANCE=1 \
AGENTSWAP_KIMI_LEGACY_ANTHROPIC_KEY="$ANTHROPIC_API_KEY" \
AGENTSWAP_KIMI_LEGACY_OPENAI_KEY="$KRILL_API_KEY" \
AGENTSWAP_KIMI_LEGACY_OPENAI_BASE_URL='https://api.krill-ai.net/v1' \
AGENTSWAP_KIMI_LEGACY_OPENAI_MODEL='kimi-k2.6' \
scripts/teleport-legacy-kimi-acceptance.sh
```

During earlier parallel exploratory runs, Claude emitted transient provider HTTP
500 retries and recovered using its normal retry behavior. The final serialized
matrix above completed successfully; a provider retry is operational noise, not
a teleport success criterion.
