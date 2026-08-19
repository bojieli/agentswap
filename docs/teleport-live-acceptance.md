# Live teleport acceptance

The opt-in harness at [`scripts/teleport-live-acceptance.sh`](../scripts/teleport-live-acceptance.sh)
exercises real native sessions. It creates one source session in each harness,
teleports every source to each of the other three targets, resumes every target
with the native CLI, and checks the resulting tool call. It does not replace a
transcript with a summary prompt.

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
and `shasum`. It builds the current `agentswap` binary, isolates OpenCode's
XDG state in a temporary directory, retains all sanitized logs for inspection,
and prints the artifact directory and summary path. A failed assertion exits
non-zero and retains the same artifacts. The retained logs are intentionally
raw for debugging; do not publish them because native session logs can contain
conversation content.

The test is an acceptance check for the currently installed CLI versions. It
does not claim that an older Python-era Kimi CLI can perform a live continuation;
the implementation still retains the separate legacy reader/writer and its
offline regression coverage.

During earlier parallel exploratory runs, Claude emitted transient provider HTTP
500 retries and recovered using its normal retry behavior. The final serialized
matrix above completed successfully; a provider retry is operational noise, not
a teleport success criterion.
