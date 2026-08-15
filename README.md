# agentswap

[![ci](https://github.com/bojieli/agentswap/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/agentswap/actions/workflows/ci.yml)
[![go reference](https://pkg.go.dev/badge/github.com/bojieli/agentswap.svg)](https://pkg.go.dev/github.com/bojieli/agentswap)
[![dependencies: none](https://img.shields.io/badge/dependencies-none-brightgreen)](#security)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**Gives your coding agent a second wind.**

Claude Code and Codex both stop dead when the upstream says no. `agentswap` is a
small local proxy that makes three failures invisible to the agent:

1. **One account runs out of quota** → rotate to another, mid-session, without
   losing context or logging in again.
2. **Everything runs out** → park the request, wait for the earliest reset, and
   carry on. Past a configurable ceiling, hand off to `agentswap run`, which
   waits and resumes the session for you.
3. **The server is overloaded (429 / 529)** → retry until it answers, instead of
   stranding the agent mid-task.

It is not a fork and not a harness. Both CLIs already support pointing at a
different API base URL, so `agentswap` is pure configuration to them and keeps
working as they change.

> **Status: early.** The core works and is covered by tests, but this has not
> been through a long soak against real accounts yet. Treat it as beta.

## Install

```sh
go install github.com/bojieli/agentswap/cmd/agentswap@latest
```

Or take a binary — the script verifies its checksum and refuses to install
without one:

```sh
curl -fsSL https://raw.githubusercontent.com/bojieli/agentswap/main/install.sh | sh
```

Binaries for Linux, macOS, Windows and FreeBSD are attached to each
[release](https://github.com/bojieli/agentswap/releases), with `SHA256SUMS`.

## Quick start

```sh
agentswap import      # adopt the logins already on this machine
agentswap install     # point Claude Code and Codex at agentswap
agentswap serve       # run the daemon
```

Then use your CLIs normally:

```sh
claude                          # picks up the settings automatically
codex --profile agentswap       # Codex needs the profile flag
```

To pool more accounts:

```sh
agentswap login --id work       # tells you what to sign in to, then adopts it
```

`login` waits for the sign-in and adopts whatever your CLI stored, however you
did it. The same account is never pooled twice — two rows holding one
credential would look like failover and be refused in the same instant.

API keys, including same-protocol third-party providers, are tried after every
subscription is spent:

```sh
agentswap add-key anthropic                                   # prompts, echo off
echo "$ANTHROPIC_API_KEY" | agentswap add-key anthropic --key -
agentswap add-key anthropic --key - --base-url https://your-gateway.example
```

Avoid putting a key in the command line itself: it lands in your shell history
and the process list. See [docs/accounts.md](docs/accounts.md).

## Watching it work

```
$ agentswap status
ACCOUNT            LANE       STATE       5H/PRI  7D/SEC  RECOVERS
personal           anthropic  exhausted   100%    64%     1h 59m
work               anthropic  active      37%     12%     -
anthropic-key-1    anthropic  available   -       -       -
chatgpt-main       openai     available   41%     -       -

anthropic: 2/3 ready   openai: 1/1 ready
```

`agentswap doctor` checks each link in the chain in the order a request travels
it, so the first failure it reports is the first thing to fix.

An upstream can revoke a login whenever it likes, and that is the one failure
waiting cannot fix. When it happens, the error your agent shows names the
account and the command:

```
your anthropic account "work" was rejected (refresh failed with 401
Unauthorized). Sign in again with `agentswap login --id work`.
```

Sign in as that account, run that command, and the credential is replaced in
place — same id, same priority, straight back into rotation.

## How it decides

Three failures all arrive as a 429 or a 5xx and each needs the opposite
response. Getting them confused is why naive rotation performs badly:

| Signal | What it means | What agentswap does |
| --- | --- | --- |
| 429, short `Retry-After` | per-minute throttle | wait, **same account** |
| 429, quota status rejected | window is spent | mark exhausted, **rotate now** |
| 529 / 5xx / `overloaded_error` | upstream capacity | **retry indefinitely**, backoff + jitter |
| 401 / 403 | stale token | refresh once, then retire the account |
| other 4xx | your request is wrong | pass straight through |

Two decisions worth knowing about:

**Rotation is not free, so it is not the default.** Prompt caches are
per-account. Rotating on a throttle that lifts in twenty seconds turns cache
hits into full-price misses. `agentswap` keeps a conversation on one account
(matched by a hash of the request prefix, which is very nearly the cache key)
and only moves when that account is genuinely spent.

**Quota is read from successful responses, not just failures.** Anthropic
returns `anthropic-ratelimit-unified-*` headers on every response, so an account
can be retired at 98% utilization rather than after it starts refusing. Codex
reports its richer quota snapshot inside the SSE stream rather than in headers,
so that lane is reactive for now — it learns an account is spent from the 429.

## Waiting without breaking the client

When every account is spent, the request is parked rather than failed. By
default the connection is held with **no bytes written** until quota returns.
The client is on loopback, so nothing in between can time out an idle socket,
and writing nothing means never committing to a status code that might have to
be taken back. `agentswap install` raises the client's own timeout accordingly.

Past `park.max_hold` (30 minutes by default) it gives up and returns an
actionable 503 with `Retry-After`, on the theory that holding a socket for five
hours is worse than telling you when to come back. It also drops a resume
ticket, which is what makes the next section work.

## Surviving a five-hour wait

Holding a connection is fine for minutes and wrong for hours. For waits that
long, run your CLI under the supervisor:

```sh
agentswap run -- claude "refactor the parser"
agentswap run -- codex exec "fix the failing tests"
```

If the pool runs dry for longer than `park.max_hold`, `agentswap run` waits for
the reset and then restarts the CLI with `claude --continue` or
`codex resume --last`, so the agent picks up where it stopped instead of
repeating the original instruction. Ctrl-C during the wait cancels it.

It also sets the environment itself, so a supervised run works even on a
machine you never ran `agentswap install` on — and it adds `--profile agentswap`
to Codex invocations, since without it Codex silently bypasses the proxy.

There is a `park.keepalive = "ping"` mode that emits SSE pings while waiting.
It is off by default and strictly riskier: once the status line is sent it
cannot be retracted.

## Configuration

```sh
agentswap config          # where everything lives, and the values in effect
agentswap config --write  # save those values as a file to edit
```

`~/.config/agentswap/config.json` — every field has a working default.

```json
{
  "addr": "127.0.0.1:8420",
  "rotation": { "drain_above": 98, "sticky": true, "sticky_ttl": "30m" },
  "retry":    { "burst_cutoff": "2m", "overload_initial": "1s",
                "overload_max": "1m", "rotate_after": 3 },
  "park":     { "enabled": true, "buffer": "1m", "max_hold": "30m",
                "keepalive": "silent" }
}
```

`buffer` is added to every observed reset time, because server and client clocks
disagree and retrying one second early wastes the whole wait.

Accounts are not configured here: they are commands, because OAuth tokens are
rewritten under you as they refresh. `agentswap set` changes an account's
upstream, priority or label; see [docs/accounts.md](docs/accounts.md) for the
whole picture.

Every field, and what it costs to get it wrong, is in
[docs/configuration.md](docs/configuration.md).

## Files it touches

| Path | What |
| --- | --- |
| `~/.config/agentswap/accounts.json` | the credential pool, mode 0600 |
| `~/.config/agentswap/state.json` | observed quota and health |
| `~/.claude/settings.json` | an `env` block, merged key by key |
| `~/.codex/config.toml` | an additive, delimited provider + profile block |

The last two follow `CLAUDE_CONFIG_DIR` and `CODEX_HOME` when you have set them,
since the CLIs themselves do.

Both CLI files are backed up before any change and restored exactly by
`agentswap uninstall`, which removes only values it recognises as its own.

## Security

`agentswap` holds live OAuth tokens, so it has **no third-party dependencies** —
nothing in this process comes from outside the Go standard library, and CI fails
if `go.sum` stops being empty. It listens on loopback only. Credential files are
written 0600 via atomic replace.

The proxy discards whatever credential the client sends and substitutes one from
the pool, so a token cannot leak from one configured CLI into another lane.

It also checks the `Host` header. A page you visit can point its own domain at
127.0.0.1 and post to a local server — DNS rebinding — and agentswap would
otherwise answer with real credentials. What rebinding cannot do is forge the
`Host` header, so unrecognised names are refused. Set `allowed_hosts` if you
reach agentswap by another name on purpose.

Full threat model and how to report something: [SECURITY.md](SECURITY.md).

## Terms of service — read this

Using several subscriptions to increase throughput may violate Anthropic's or
OpenAI's terms, and accounts have reportedly been banned over unusual OAuth
patterns.

`agentswap` is **failover-only by design**: exactly one account is ever in
flight, and it rotates only when the current one is genuinely refused. It never
runs accounts in parallel to multiply throughput. That is a more defensible
posture than round-robin pooling, but it is not legal advice, and whether your
usage is permitted is your call about your own accounts.

## Limitations

- A stream that fails **after** partial output cannot be transparently retried;
  those errors reach the client.
- Predictive rotation is Anthropic-only (see above).
- Codex needs `--profile agentswap`; it has no equivalent of Claude Code's
  automatic settings pickup.
- Automatic resume needs `agentswap run`. A bare `claude` gets the 503 and
  stops, because nothing is supervising it.
- Adding an account means signing in with the CLI itself; agentswap has no
  OAuth flow of its own, so `agentswap login` guides and adopts rather than
  signing you in.

## Prior art

[CC-Router](https://github.com/VictorMinemu/CC-Router),
[claude-swap](https://github.com/realiti4/claude-swap),
[teamclaude](https://github.com/KarpelesLab/teamclaude),
[cux](https://github.com/inulute/cux),
[llmux](https://github.com/2lab-ai/llmux) and
[coding_agent_account_manager](https://github.com/Dicklesworthstone/coding_agent_account_manager)
all do account rotation, and the last covers Codex and Gemini too. What is
different here is waiting through a reset instead of surfacing it, retrying
overload without a bound, reading quota before failure rather than after, and
keeping conversations pinned for cache affinity.

## Documentation

- [Commands](docs/commands.md) — every command and flag, in one place
- [Accounts and keys](docs/accounts.md) — pooling logins, re-signing in, where
  keys live and why it is not one file
- [Configuration](docs/configuration.md) — every field, and what getting it
  wrong costs
- [Architecture](docs/architecture.md) — how a request flows, and how to add a
  third lane
- [Troubleshooting](docs/troubleshooting.md) — symptoms, causes, fixes
- [Contributing](CONTRIBUTING.md) — the one hard rule is no dependencies
- [Security](SECURITY.md) — threat model, and how to report privately

## License

MIT
