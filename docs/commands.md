# Command reference

Every command, with the flags that matter. `agentswap <command> -h` prints the
same flags at the terminal.

If you are setting up for the first time, use this order:

```sh
agentswap import
agentswap install
agentswap service install
agentswap doctor
```

For recovery examples, read [Keep a session moving](sessions.md). For account
choices, read [Accounts, subscriptions, keys, and providers](accounts.md).

Commands that read or write the pool use the config directory, which is
`AGENTSWAP_HOME`, else `XDG_CONFIG_HOME/agentswap`, else `~/.config/agentswap`.

## Getting credentials in

### `agentswap import`
Adopts the credentials and active provider overrides `claude` and `codex` have
already stored. On macOS this includes the Keychain, where Claude Code keeps
its native login instead of on disk. A native subscription and a configured
third-party provider are imported as separate accounts, with the provider's
base URL and authentication style preserved.

| Flag | Meaning |
| --- | --- |
| `--id NAME` | name the account (default: `<lane>-1`, `<lane>-2`, …) |
| `--label TEXT` | human-readable name shown in `status` |
| `--dry-run` | discover and show the accounts without writing pool files |

A lane with no usable login or configured provider is skipped, not an error. A
credential already in the pool is refreshed in place rather than added twice. If
`ANTHROPIC_API_KEY` or `OPENAI_API_KEY` is set in your environment, it says so
without adopting it.

### `agentswap login`
Adds an account, or replaces the credential of one that was rejected. agentswap
has no OAuth flow of its own, so this guides the sign-in and adopts the result —
watching for the credential rather than driving your CLI, so signing in from
another terminal or a browser works.

| Flag | Meaning |
| --- | --- |
| `--id NAME` | the account to add or replace |
| `--label TEXT` | human-readable name |
| `--lane anthropic\|openai` | which CLI; inferred when it can be |
| `--timeout DURATION` | how long to wait for the sign-in (default 10m) |

Ctrl-C stops waiting. Replacing keeps the account's id, priority and label, and
clears the rejected state so it is tried again.

### `agentswap add-key <anthropic|openai>`
Adds an API key. Keys are tried after every subscription is spent.

| Flag | Meaning |
| --- | --- |
| `--key KEY` | the key; `-` reads it from stdin |
| `--base-url URL` | a third-party provider speaking the same protocol |
| `--id NAME`, `--label TEXT` | naming |
| `--priority N` | lower is preferred within a lane |

With no `--key`, it reads `AGENTSWAP_API_KEY`, and failing that prompts with
the echo off. Avoid putting the key in the command line: it lands in your shell
history and the process list. The same key is never added twice.

### `agentswap add-token <anthropic|openai>`
Pools a long-lived bearer token — one that is not tied to your CLI's session,
and so does not go stale when the CLI renews its own credential.

| Flag | Meaning |
| --- | --- |
| `--token TOKEN` | the token; `-` reads it from stdin |
| `--id NAME`, `--label TEXT` | naming |
| `--base-url URL` | override the upstream |
| `--priority N` | order within the lane |

Get one with `claude setup-token`. It is stored as a subscription, so it is
spent before any metered key. An imported credential, by contrast, is a copy of
what your CLI is using and lasts only until one of you renews — see
[accounts.md](accounts.md).

## Changing what is there

### `agentswap set <account-id>`
Changes an account already in the pool. The id stays the same, so its observed
quota and the conversations pinned to it are kept.

| Flag | Meaning |
| --- | --- |
| `--base-url URL` | where this account talks to; `""` restores the default |
| `--priority N` | order within the lane |
| `--label TEXT` | display name |
| `--key -` | replace an API key (subscriptions use `login`) |

### `agentswap enable <id>` / `agentswap disable <id>`
Takes an account out of rotation, or puts it back, without forgetting the
credential.

### `agentswap remove <id>`
Forgets the account and its observed quota.

## Looking at it

### `agentswap list`
Every account: lane, kind, priority, state, and which credential it holds — a
masked key, the plan, and the host for a third-party provider.

### `agentswap status`
Live quota per account, what is ready per lane, and what to do about anything
rejected. Reads from the running daemon, which is fresher than the state file.

| Flag | Meaning |
| --- | --- |
| `--watch DURATION` | redraw on an interval |
| `--addr HOST:PORT` | a daemon somewhere other than where it published |

### `agentswap doctor`
Walks the chain in the order a request travels it, so the first failure
reported is the first thing to fix. `[FAIL]` is a problem; `[    ]` is a note,
such as a lane you do not use. Exits non-zero only on a real problem.

### `agentswap config`
Where every file lives, whether the daemon is running, and every setting in
effect — which an absent `config.json` cannot tell you, since each field has a
default.

| Flag | Meaning |
| --- | --- |
| `--json` | print the effective settings and nothing else |
| `--write` | save them to `config.json`, ready to edit |
| `--force` | with `--write`, replace a file that exists |

Do not redirect `--json` into `config.json` yourself: the shell truncates the
file before agentswap reads it. `--write` is the command that works.

## Running it

### `agentswap serve`
The daemon. Serves `/anthropic` and `/openai` on one port, and publishes the
address it actually bound so the other commands can find it.

| Flag | Meaning |
| --- | --- |
| `--addr HOST:PORT` | override `addr` from the config; `:0` picks a free port |
| `-v` | debug logging |

Ctrl-C or SIGTERM shuts it down, flushing observed quota first.

### `agentswap service <install|uninstall|status>`
Runs the daemon in the background and starts it again at login, so nothing
depends on a terminal staying open. Per-user, never system-wide: the daemon
holds one person's credentials.

| Platform | What it writes |
| --- | --- |
| macOS | a LaunchAgent at `~/Library/LaunchAgents/com.github.bojieli.agentswap.plist` |
| Linux | a systemd user unit at `~/.config/systemd/user/agentswap.service` |
| Windows | nothing — it prints the Task Scheduler and Startup-folder options |

`install --dry-run` prints the file instead of writing it. The config
directory is written into the service, so it reads the same pool the CLI
edits — a service manager's environment is not your shell's.

On Linux a user unit stops when you log out unless lingering is on:
`sudo loginctl enable-linger $USER`.

### `agentswap run -- <command> [args...]`
Runs a CLI against agentswap and, if the whole pool runs dry for longer than
`park.max_hold`, waits for the reset and resumes the session rather than
repeating the original instruction.

| Flag | Meaning |
| --- | --- |
| `--max-resumes N` | how many times to resume (default 10) |

It also sets the environment itself, so it works on a machine you never ran
`install` on. Codex arguments remain native; its configured provider is used
for the first invocation and any automatic resume.

### `agentswap teleport <source> <target>`

Creates a new native session for `claude`, `codex`, `opencode`, or `kimi` from
the newest current-directory session belonging to another one of those agents.
Both agents are positional and the order is always source, then target:

```sh
agentswap teleport claude codex
```

Teleport stops after creating the target and prints its native resume command.
This is always a user command: pool exhaustion never chooses a different
harness automatically.

### `agentswap handoff <source> <target>`

Performs the same validated teleport and then launches the target coding agent
with the exact new session id:

```sh
agentswap handoff claude codex
agentswap handoff claude codex --dangerously-bypass-approvals-and-sandbox
agentswap handoff codex claude --dangerously-skip-permissions
```

`handoff` is the short interactive path. It accepts `--session` and `--cwd`;
every other argument is appended unchanged to the native target resume
command. This supports the target's normal model, approval, sandbox, prompt,
and display options without Agent Swap having to mirror them. Use a `--`
separator when the target itself needs a flag named `--session` or `--cwd`:

```sh
agentswap handoff claude codex --cwd ./project -- --cwd ./target-view
```

Because launching is the defining behavior, use `teleport --dry-run` when only
validation is wanted. All target arguments are passed through unchanged,
including Codex profile, provider, model, approval, sandbox, reasoning, and
prompt options.

Run it from the project directory. Discovery compares canonical filesystem
paths, including symlink equivalence, but deliberately does not fall back to
"same Git repository": separate worktrees and monorepo packages must remain
separate session scopes. The new target is given that same canonical directory,
so it appears in the target's native resume picker.

| Flag | Meaning |
| --- | --- |
| `--session ID` | select an exact source id in this directory |
| `--cwd PATH` | use a directory other than the current one |
| `--compact` | abridge the history to fit the target, archiving the full session |
| `--budget N` | token budget for the abridged transcript, such as `120k`; implies `--compact` |
| `--archive-dir PATH` | parent directory for the archive, default `<project>/.agentswap`; implies `--compact` |
| `--dry-run` | with `teleport`, read, validate, and report without writing a target |

`--compact` exists because harnesses do not share a context window: a session
that Claude Code held comfortably can be too large for the target to load. It
reduces the transferred thread mechanically — no model is asked to summarize
anything — and writes everything it removed to an archive under
`<project>/.agentswap/<id>/`. See
[Keep a session moving](sessions.md#when-the-target-cannot-hold-the-whole-history)
for what the archive contains and what the target is told about it.

```sh
agentswap teleport claude codex --compact
agentswap teleport claude codex --compact --dry-run
agentswap handoff claude codex --budget 80k
agentswap handoff claude codex --compact --archive-dir ~/agentswap-archives
```

The archive defaults into the project because a coding agent confined to the
project directory cannot read one outside it without being granted access, and
a non-interactive resume cannot ask. Each archive carries a `.gitignore` that
keeps it out of version control. `--archive-dir` moves them elsewhere;
`teleport` then prints a hint saying the target will need to be granted access.

All three flags belong to agentswap on a `handoff` command line and are not
forwarded to the target CLI.

Selection precedence is an explicit `--session`, an active-session environment
id for the named source harness, then the newest exact-directory source
candidate. There is no cross-agent discovery or interactive picker, so the
same command selects the same session in a terminal and in a script.

For compatibility, `teleport <target> --from <source>` and `teleport --launch`
remain accepted with deprecation warnings. `--latest` is also accepted but is
now redundant. New scripts should use the positional form and `handoff`.

The success output always includes the new id and exact resume command:

```text
Created Codex session 019...
Resume: codex resume 019...
```

The target commands are `claude --resume ID`,
`codex resume ID`,
`opencode --session ID`, current `kimi --session ID --model MODEL`, and legacy
`kimi -r ID`.
`handoff` uses the exact id; it never races against another terminal through
`--last` or `--continue`. Codex's configured provider is copied into the
teleported session metadata so the native resume command can bootstrap it.

Codex target rollouts record the target installation's configured
`model_provider` in `session_meta` because Codex uses that field during resume.
This keeps native metadata aligned with the generated command without forcing
an agentswap-specific provider or profile.

Teleport preserves recorded messages and message order, text, recorded
reasoning, tool names/call ids/JSON inputs/results/error state, plan revisions,
timestamps, title, model, source id, and cwd wherever the destination has a
native representation. It does not summarize or rewrite the history into a
single prompt. Before any target write, the canonical stream is checked for
malformed JSON, duplicate calls/results, orphaned results, empty records, and
unknown conversation-bearing content. An unfinished source tool call gets an
explicit interrupted result because the destination APIs require every call to
be paired.

The source is never modified. Claude, Codex, and both Kimi layouts stage files
and publish them atomically. A failed write removes its newly created target
artifacts. OpenCode is different: `opencode export`, `opencode session list`,
and `opencode import` provide its schema boundary; a failed/unconfirmed import
triggers deletion of only the newly generated target id.

What cannot move is reported or rejected, not hidden: KV/prompt caches,
unrecorded system instructions and hidden reasoning, provider-encrypted or
signed reasoning state, credentials, approvals, live processes, background
jobs, and in-memory plugin/MCP state. Recorded reasoning is retained as visible
content if the target cannot reuse its provider signature. Text-file context is
retained as visible text with a warning. Inline media (including images) is
transferred as native content (remote URLs remain URLs). Unsupported media forms
and unknown conversation-bearing native blocks currently fail closed.

Delegated agent runs travel as branches beside the main thread, each linked to
the tool call that spawned it. Claude Code and Kimi Code write them natively;
Codex, OpenCode, and the Python-era Kimi layout have no equivalent and report
each run they could not keep. A moved run is readable but not resumable in the
target. See [the session guide](sessions.md) for the layouts and the two
sources whose linkage is approximate.

Kimi Code has two incompatible local formats. Current releases use
`~/.kimi-code` with per-session state and versioned wire event logs; the
Python-era CLI uses `$KIMI_SHARE_DIR` or `~/.kimi` with `context.jsonl` and
`wire.jsonl`. Both are readable and writable. Set
`AGENTSWAP_KIMI_FORMAT=legacy` only when deliberately targeting the older CLI;
otherwise the current format is preferred. Current Kimi sessions bind a native
agent profile, including its model and tool policy, at creation time. Imported
history intentionally does not fabricate that provider-specific profile, so
the generated resume command includes Kimi's configured `default_model` and
lets Kimi perform the bind itself. `AGENTSWAP_KIMI_MODEL` overrides that target
model when needed.

## Wiring your CLIs

### `agentswap install`
Points Claude Code and Codex at agentswap: an `env` block merged key by key
into the Claude settings, an additive provider block in Codex's base config,
and an `agentswap.config.toml` profile overlay that selects that provider.
Both Codex files are backed up before they are changed.

| Flag | Meaning |
| --- | --- |
| `--dry-run` | show what would change |
| `--only claude\|codex` | just one of them |

The client's request timeout is derived from `park.max_hold`, so re-run this
after changing that setting.

### `agentswap uninstall`
Removes only what `install` added, leaving a deliberate override of your own
alone.

### `agentswap env`
Prints shell exports that point the current shell at agentswap, touching no
config file:

```sh
eval "$(agentswap env)"
```

Useful for a one-off shell, a CI job, or trying agentswap without changing
anything. Codex reads a config file rather than the environment. If you want
Codex requests routed through agentswap, run `agentswap install --only codex`;
native handoff and resume commands do not add a profile flag.

### `agentswap version`
Prints the version. Release builds carry the tag; a build from source says
`dev` unless you set it with `-ldflags`.

## Environment

| Variable | Effect |
| --- | --- |
| `AGENTSWAP_HOME` | the config directory; wins over everything |
| `XDG_CONFIG_HOME` | `$XDG_CONFIG_HOME/agentswap` |
| `AGENTSWAP_API_KEY` | read by `add-key` when `--key` is absent |
| `CLAUDE_CONFIG_DIR` | where Claude Code's settings and credentials are |
| `CLAUDE_CREDENTIALS_PATH` | a specific credential file; no Keychain fallback |
| `CODEX_HOME` | where Codex's config and auth are |
| `KIMI_CODE_HOME` | current Kimi Code data root (default `~/.kimi-code`) |
| `KIMI_SHARE_DIR` | legacy Kimi CLI data root (default `~/.kimi`) |
| `AGENTSWAP_KIMI_FORMAT` | `modern` or `legacy` target format override |
| `AGENTSWAP_KIMI_MODEL` | target model alias in the generated current-Kimi resume command |
| `AGENTSWAP_OPENCODE_BIN` | alternate `opencode` executable used for native import/export |
| `AGENTSWAP_SESSION_ID` | explicit active source id for `teleport` or `handoff` |
