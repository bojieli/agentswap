# Command reference

Every command, with the flags that matter. `agentswap <command> -h` prints the
same flags at the terminal.

Commands that read or write the pool use the config directory, which is
`AGENTSWAP_HOME`, else `XDG_CONFIG_HOME/agentswap`, else `~/.config/agentswap`.

## Getting credentials in

### `agentswap import`
Adopts the credentials `claude` and `codex` have already stored. On macOS this
includes the Keychain, where Claude Code keeps them instead of on disk.

| Flag | Meaning |
| --- | --- |
| `--id NAME` | name the account (default: `<lane>-1`, `<lane>-2`, …) |
| `--label TEXT` | human-readable name shown in `status` |

A lane whose CLI is not signed in is skipped, not an error. A credential
already in the pool is refreshed in place rather than added twice. If
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
`install` on, and adds `--profile agentswap` to Codex invocations.

## Wiring your CLIs

### `agentswap install`
Points Claude Code and Codex at agentswap: an `env` block merged key by key
into the Claude settings, and an additive, delimited block in the Codex config.
Both are backed up first.

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
anything. Codex reads a config file rather than the environment, so it still
needs `install` and `--profile agentswap`.

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
