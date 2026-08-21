# Keep a session moving

This guide explains the two kinds of recovery agentswap provides:

- **resume in the same harness** after a quota wait; and
- **continue in another harness** when a different provider still has
  capacity.

The commands never move a session without your instruction. Choose the
recovery path that matches what you want to preserve.

## Which command should I use?

| Situation | Command | Result |
| --- | --- | --- |
| The current account is throttled briefly | Use Claude Code or Codex normally through the proxy. | The request waits on the same account. |
| The current account is spent, but another account can answer | Use the CLI through `agentswap`. | The request rotates to the next eligible account. |
| All accounts are waiting for a reset | `agentswap run -- <command>` | The session resumes in the same harness after the wait. |
| Another harness has usable capacity | `agentswap handoff <source> <target>` | A target session is created and launched. |
| You want to inspect the target before launching it | `agentswap teleport <source> <target>` | A target session is created; the source is unchanged. |
| You only want to validate compatibility | `agentswap teleport ... --dry-run` | The source is read and validated without writing a target. |
| The session is too large for the target's context window | `agentswap handoff <source> <target> --compact` | The thread is abridged to fit, and the full history is archived where the target can read it. |

## Resume in the same harness

Run long tasks under the supervisor:

```sh
agentswap run -- claude "refactor the parser"
agentswap run -- codex exec "fix the failing tests"
```

The proxy first handles short throttles and account rotation. If every account
is exhausted and the wait is longer than `park.max_hold`, the proxy returns a
resume ticket. `agentswap run` waits for the reset and invokes the harness's
native continuation command.

The supervisor preserves the command's normal arguments. Codex continues to
use its configured provider and profile; Claude Code continues with its own
resume behavior. Set `--max-resumes` when a task may cross more than the
default ten quota windows:

```sh
agentswap run --max-resumes 20 -- codex exec "finish the migration"
```

Automatic resume needs the supervisor. A bare `claude` or `codex` process can
receive a `503` after the configured maximum hold time because no parent
process is available to resume it.

## Continue in another harness

Suppose Claude Code has exhausted its subscriptions and provider fallback,
but Codex still has credits. From the same project directory:

```sh
agentswap handoff claude codex
```

agentswap selects the newest Claude session for that exact directory, validates
the complete recorded history, writes a native Codex rollout, and launches:

```text
codex resume <new-session-id>
```

The reverse direction works the same way:

```sh
agentswap handoff codex claude
```

The supported harnesses are Claude Code, Codex, OpenCode, and current or legacy
Kimi Code. Source and target are always written in that order:

```text
agentswap handoff SOURCE TARGET
```

### `handoff` versus `teleport`

`handoff` performs a teleport and immediately opens the target CLI. It is the
short path when you want to continue now:

```sh
agentswap handoff claude codex
agentswap handoff claude codex --dangerously-bypass-approvals-and-sandbox
```

`teleport` only creates the target. It prints the native resume command so you
can inspect the result, change target options, or launch it later:

```sh
agentswap teleport claude codex
agentswap teleport claude opencode
```

Use `--dry-run` to validate a source without creating anything:

```sh
agentswap teleport claude codex --dry-run
```

### Selecting a source session

The default is the newest source session in the requested directory. Select an
exact session when several tasks are open:

```sh
agentswap handoff claude codex --session <source-id>
```

Use another directory when the shell is not currently in the project:

```sh
agentswap teleport codex claude --cwd ~/src/project
```

Discovery compares canonical, symlink-aware paths. It does not fall back to a
shared Git repository, so separate worktrees and monorepo packages stay
separate. Put `--` before target arguments if the target itself needs a
`--session` or `--cwd` option:

```sh
agentswap handoff claude codex --cwd ./source -- --cwd ./target-view
```

All other arguments are passed through to the target CLI, including model,
provider, permission, sandbox, prompt, and display options.

## What the transfer preserves

agentswap translates the recorded event structure instead of building a new
summary prompt. Depending on what the source recorded, the target retains:

- user and assistant messages;
- recorded reasoning and model metadata;
- tool calls, results, errors, and call ids;
- plans and timestamps;
- supported text and inline media; and
- delegated agent runs, where the destination has a place to keep them.

The source remains read-only. Validation completes before a target is written.
Unknown conversation-bearing records fail closed rather than producing a session
that only looks resumable.

OpenCode's session database is accessed through its own `export` and `import`
commands. agentswap does not write OpenCode's SQLite schema directly.

### Delegated agent runs

When a session spawns subagents — Claude's `Task`, Kimi's `Agent` and
`AgentSwarm` — the delegated run is a separate transcript. The main model never
read it: it saw only the delegating tool call and the result the harness
recorded for that call. The main thread is therefore complete on its own, and
the delegated transcript travels as archived detail linked to that call.

Claude Code and Kimi Code both have a native place for one, so a transfer
between them keeps every run:

| Harness | Where a delegated run lives |
| --- | --- |
| Claude Code | `<session-id>/subagents/agent-*.jsonl` plus a `.meta.json` naming the spawning `toolUseId` |
| Kimi Code | `agents/agent-*/wire.jsonl`, an entry in `state.json`, and a task record under the agent that spawned it |

Codex rollouts and the OpenCode import boundary have no equivalent, and neither
does the Python-era Kimi layout. Those targets receive the complete main thread
and a warning naming each run that could not come with it.

Two limits are worth knowing. A delegated run is readable in the target but
cannot be resumed there, because resuming one needs live harness state that no
transfer carries; a Kimi run still marked running is recorded as failed rather
than left for Kimi to poll. And Kimi records no spawning call for a swarm
member, so agentswap matches it by the swarm item it was given — if one item is
claimed by more than one `AgentSwarm` call, that branch moves unattached and
says so.

OpenCode is the exception on the read side. It keeps a delegated run in a
separate child session, and neither `opencode export` nor
`opencode session list` exposes the link to it. The delegation itself is
retained as visible text, with a warning; the child session stays where it is.

## When the target cannot hold the whole history

Harnesses do not share a context window. A session Claude Code carried
comfortably can be more than the target can load, and the transfer then looks
successful right up to the moment the resumed session fails. agentswap measures
what it is about to hand over and says so:

```text
warning: the transferred thread is about 384k tokens, which is more than Codex
is likely to hold; re-run with --compact to abridge it and archive the full
history
```

The warning is only a warning. Nothing is abridged unless you ask:

```sh
agentswap teleport claude codex --compact
agentswap handoff claude codex --compact --budget 80k
```

`--budget` sets the token budget for the replayed conversation and implies
`--compact`. Without it, each target gets a default budget that sits well below
any supported harness's window, because the target also needs room for its
system prompt, its tool definitions, and the work you are resuming it to do.
The token count is an estimate: agentswap cannot know which model the target
will select, so it counts conservatively and lets you override the number.

### What compaction does

The reduction is mechanical and offline. Nothing asks a model to summarize the
history, which would make a transfer non-deterministic, put it back on the
network, and give it a credential. agentswap works down a ladder instead,
stopping at the first step that fits:

1. recorded reasoning, which the target cannot use anyway;
2. long tool results, truncated to their opening and closing lines;
3. long pasted messages, truncated the same way;
4. inline attachments, replaced by a pointer to the file holding them;
5. whole earlier turns, replaced by one note in the place they were removed.

Most sessions stop at step 2 or 3, because nearly all of a coding session's
bytes are tool output rather than conversation. Delegated agent runs are left
alone at every step: the main model never read them and resuming the main
thread does not load them, so removing one would cost fidelity and save
nothing.

What replaces a summary is a digest placed in front of the abridged thread,
derived entirely from the recorded events: the original request, the files the
tool calls wrote, the commands they ran, the latest plan, and any tool call
that never came back.

### The archive

Everything removed is written to `<project>/.agentswap/<id>/`:

| File | What it holds |
| --- | --- |
| `INDEX.md` | what was removed, and which file holds each piece |
| `transcript.txt` | the entire original conversation as plain text |
| `history.json` | the same conversation, machine readable |
| `manifest.json` | the source and target sessions, the reduction, and a checksum per file |
| `events/`, `media/` | the full payload behind each inline marker |

The archive is plain files on purpose. A resumed agent can open a file in its
own working process; it cannot always be granted permission to run a new
command. Wherever content was removed the transcript carries an inline marker
naming the exact file:

```text
[agentswap:elided 12k bytes, about 402 lines; full text:
 /Users/you/src/project/.agentswap/25a5.../events/0002-tool-result-call-000.txt]
```

The archive is written before the target session and removed again if writing
the target fails, so a target never points at an archive that is not there.
`--dry-run` reports the reduction and writes neither.

### Why the archive lives in the project

A coding agent is normally confined to its working directory. An archive kept
anywhere else has to be granted access before the target can follow a marker —
and a non-interactive resume has nobody to ask. Live checks against Claude Code
showed exactly that: with the archive outside the project and ordinary
permissions, the resumed session read the digest, found the marker, tried the
file, was denied, named the path it needed, and declined to guess. Inside the
project, the same question was answered without any grant.

Each archive gets its own directory under `.agentswap/`, so several can
coexist, and each carries a `.gitignore` matching everything — itself included
— so the whole directory stays out of version control without agentswap editing
a `.gitignore` you maintain.

`--archive-dir` moves the parent somewhere else, which is useful for keeping
archives out of a working tree entirely:

```sh
agentswap handoff claude codex --compact --archive-dir ~/agentswap-archives
```

That is a deliberate trade: `teleport` prints a hint saying the target will now
have to be granted access before it can read anything the transfer removed.

Treat an archive as sensitive wherever it lives: it holds whatever the tool
output contained. Nothing removes one automatically.

### Compacting in the source harness instead

If you would rather have a written summary than a mechanical reduction, ask the
source harness for one before transferring — `/compact` in Claude Code, or the
target's own equivalent — and then teleport the shortened session. That uses
the source model's own summarizer, which agentswap deliberately does not do on
your behalf.

## What the transfer cannot preserve

A target is a new process. It does not receive:

- credentials or provider API keys;
- provider KV caches;
- hidden or encrypted reasoning that the source did not expose;
- approvals, permission state, or sandbox state;
- running shell processes, background jobs, or plugin memory;
- a resumable delegated agent run, even where its transcript moved; or
- a provider's internal system prompt when it was not recorded.

Text-file context may become visible conversation text in a target that has no
attachment representation. Treat the target transcript as sensitive data and
review its permissions before sharing it.

## Provider choice after a handoff

The target harness uses its own installed configuration. A handoff does not
copy the source credential or silently choose a provider. For Codex, the
generated rollout records the target installation's configured provider and
resumes with native `codex resume <id>` syntax.

To route both Claude Code and Codex through agentswap before a handoff, run:

```sh
agentswap install
```

To inspect the exact target and source state without writing anything, use
`teleport --dry-run` first. The full compatibility evidence is in
[the live acceptance record](teleport-live-acceptance.md).

## Related references

- [Command reference](commands.md#running-it)
- [Managing accounts and provider overrides](accounts.md)
- [Configuration](configuration.md#park)
- [Troubleshooting](troubleshooting.md)
- [Security model](../SECURITY.md)
