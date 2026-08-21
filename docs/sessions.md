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
