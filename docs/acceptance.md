# Acceptance record

This is maintainer evidence, not a prerequisite for ordinary installation.
For the user-facing setup, start with the [README](../README.md). For the
cross-harness behavior this record checks, see [sessions.md](sessions.md).

This is the evidence record for the credential-pooling, failure-handling,
supervisor, and session-teleport features. It separates deterministic tests
from requests made against real providers; a simulated quota response is not
presented as a real exhausted paid account.

The verified snapshot is 2026-08-19 on macOS 26.6.2/arm64 with Go 1.25.7.
Linux runtime coverage used Docker 27.3.1 on Linux/arm64 and the Go 1.23
toolchain declared by `go.mod`.

## Deterministic suite

`make check` passes and includes a clean build, `go vet`, formatting and
dependency checks, and every unit and compiled-binary E2E test under the race
detector. The repository currently has 392 top-level test or fuzz entry points,
many with table-driven subtests.

The suite directly covers the credential lifecycle and request engine:

- importing Claude and Codex credentials; duplicate detection; refreshed
  credentials; API keys, long-lived tokens, labels, priorities, replacement,
  enable/disable, removal, validation, file permissions, and atomic persistence;
- subscription-before-key and explicit-priority selection, sticky conversation
  routing, predictive quota draining, and all-accounts-drained fallback;
- short 429 waits on the same account, quota-exhaustion rotation, 401/403 refresh
  and rejection, other 4xx passthrough, overload retry/backoff, and cancellation;
- parking until reset, maximum-hold handoff with `Retry-After`, actionable empty
  or rejected-pool errors, resume-ticket freshness, and exact Claude/Codex
  supervisor resume arguments;
- client-credential replacement, header and request-body fidelity across a
  retry, incremental streaming, client disconnects, loopback host checks, DNS
  rebinding refusal, daemon hot reload, status watching, concurrent requests,
  shutdown persistence, and stale daemon metadata;
- additive/idempotent install and uninstall behavior, backup preservation,
  timeout derivation, macOS LaunchAgent generation, and Linux systemd-user unit
  generation.

The session suite covers every reader and writer, canonical validation, exact
cwd and symlink matching, latest/active/exact selection, dry runs, native
resume commands, handoff argument passthrough, launch failure retention, source
immutability, and rollback.
It also includes:

- a 250-turn mixed history with Unicode, reasoning, MCP-style tool calls, tool
  errors, five plan revisions, and dangling calls that require interrupted
  results;
- a JSONL safety-limit case larger than 64 MiB;
- image-media round trips plus fail-closed cases for unsupported media,
  malformed and unknown conversation-bearing blocks, duplicate/orphaned
  results, and empty canonical records;
- delegated agent runs: Claude and Kimi branch round trips including a nested
  run, the split of sidechain records an older Claude inlined into the main
  log, per-branch validation as an independent tool-call namespace, OpenCode
  delegation retained as text, and the warnings Codex and the Python-era Kimi
  layout raise for runs they cannot keep;
- post-publication rollback for legacy Kimi metadata corruption and no-artifact
  guarantees when a target data root is unusable;
- full round trips for Claude JSONL, Codex rollouts, current Kimi wire logs, and
  Python-era Kimi context/wire logs, plus OpenCode import/export fixtures;
- compaction: a session that already fits is left untouched and gets no
  archive; every rung of the reduction ladder produces a session that still
  validates; a truncated payload keeps both ends and names a shard that exists
  and matches its recorded checksum; a collapse never separates a tool result
  from its call; the source is byte-identical afterwards; delegated runs are
  neither counted against the budget nor archived; source-controlled call ids
  and filenames cannot escape the archive directory; a dry run writes nothing;
  a failed target write removes the archive it had already written; the
  oversized-transfer warning fires without compacting on its own; and
  the archive defaults into the project, carries a `.gitignore` matching
  everything so it cannot be committed by accident, and makes every marker
  resolve inside the project, while `--archive-dir` moves it and leaves the
  default location unused.

The most stateful packages (`internal/session`, `internal/engine`, and
`internal/store`) passed 20 consecutive runs. The compiled-binary E2E suite
passed five consecutive runs under the race detector in 184 seconds.

Three persistent fuzz targets exercise canonical session validation, the
bounded JSONL reader, and compaction. A 30-second run of each processed
approximately 5.6 million canonical inputs, 71,000 file inputs, and 9.0 million
compaction inputs without a crash or invariant violation. The compaction target
asserts the property every writer depends on: any session `Validate` accepts is
still valid after being compacted at any budget. Fuzz seeds remain part of every
ordinary test run.

Merged unit and subprocess instrumentation reports 79.7% statement coverage;
the command package reaches 80.0% after E2E coverage is merged. Coverage is a
navigation aid, not the acceptance criterion.

## Real session continuation

The opt-in [`teleport-live-acceptance.sh`](../scripts/teleport-live-acceptance.sh)
created native source sessions in Claude Code 2.1.235, Codex CLI 0.148.0,
Kimi Code 0.37.2, and OpenCode 1.18.18. It then exercised all 12 directed
source-to-different-target pairs.

Every resumed model had to recall a unique token, exact marker, intentional
tool failure, and completed/pending plan state from the imported history, then
make a fresh native file-reading tool call. All 12 passed. Source digests were
unchanged, and all targets were visible in their native cwd-scoped stores. The
exact matrix and source digests are in
[`teleport-live-acceptance.md`](teleport-live-acceptance.md).

Python-era `kimi-cli` 1.49.0 is tested separately by the opt-in
[`teleport-legacy-kimi-acceptance.sh`](../scripts/teleport-legacy-kimi-acceptance.sh).
It uses an isolated `KIMI_SHARE_DIR`, checks the physical cwd recorded in
`kimi.json`, resumes an agentswap-generated `context.jsonl`/`wire.jsonl`
session, requires exact history recall and a fresh `ReadFile` call, then imports
that real legacy session back into Claude and requires another exact resumed
tool turn. Both native source digests must remain unchanged during their
respective teleports.

The non-provider [`teleport-pty-acceptance.sh`](../scripts/teleport-pty-acceptance.sh)
drives the binary through a real pseudo-terminal. It verifies terminal
behavior matches scripts: default current-directory discovery selects the
latest named-source session without prompting, `--session` selects an exact
non-latest source, a missing id writes no target, and Codex output retains the
native `codex resume ID` command without an injected profile.

## Real credential failover

The opt-in [`credential-live-acceptance.sh`](../scripts/credential-live-acceptance.sh)
builds the current binary and creates a completely isolated credential pool. It
puts an intentionally invalid credential before a valid credential in each
lane, then makes real requests through agentswap:

| Client/lane | First upstream result | Retried result | Acceptance |
| --- | --- | --- | --- |
| Claude / Anthropic | real 401, invalid account rejected | valid Anthropic key served the request | PASS |
| Codex / OpenAI-compatible Krill | real 401, invalid account rejected | valid key served the request | PASS |

The client sends a known placeholder credential in both cases. The proxy must
replace it, both invalid accounts must appear as rejected, both valid accounts
must remain available, and the response must contain a unique marker. The
harness removes `accounts.json` even after a failure and scans retained evidence
so provider keys are neither printed nor retained.

## Platform and release checks

- The complete race-enabled suite passed at runtime inside
  `golang:1.23-bookworm` on Linux/arm64.
- Release builds passed for Linux amd64/arm64, macOS amd64/arm64, Windows
  amd64/arm64, and FreeBSD amd64.
- macOS service behavior is exercised with a fake `launchctl`; Linux service
  behavior is exercised with a fake `systemctl`. Acceptance does not install a
  service into the developer's real login session.

## Deliberate limits of the evidence

Compaction was checked live against Claude Code 2.1.238, with the local file the
planted token came from deleted so the archive was its only remaining copy. A
resumed Claude followed the elision marker to the exact shard for both a
truncated tool result and a collapsed run of turns. With the archive outside the
project and ordinary permissions the read was denied — the model named the path,
asked for access, and refused to guess — which is why the archive now defaults
into `<project>/.agentswap`; both cases pass there with no grant. Kimi Code was
not covered: that account had reached its usage limit. The matrix is in
[`teleport-live-acceptance.md`](teleport-live-acceptance.md#compaction-check).

No acceptance run intentionally burns a valid paid account to zero merely to
obtain a real quota-exhaustion 429. Exact Anthropic and OpenAI classification,
rotation, parking, reset timing, 503 handoff, and error text are exercised with
controlled upstream servers; real-provider integration is established by the
invalid-to-valid 401 failovers above. This avoids spending or disabling the
user's active accounts for a destructive test.

Likewise, live OAuth refresh is not performed against the user's current CLI
login because a refresh can rotate and invalidate the credential in active use.
Refresh success, refresh-token rotation, omission, concurrency coalescing,
failure, persistence, and rejection remedies are covered against controlled
OAuth servers.

Session continuation proves the observable native record. No tool can migrate
provider KV caches, hidden or encrypted reasoning, an unrecorded system prompt,
credentials, approvals, a live process, or in-memory plugin state. Unsupported
conversation-bearing records fail closed instead of being silently discarded.
The live checks establish compatibility with the versions listed above; future
harness schema changes remain the reason the parsers validate unknown records
strictly and the acceptance scripts are reusable.
