# agentswap

[![CI](https://github.com/bojieli/agentswap/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/agentswap/actions/workflows/ci.yml)
[![Go reference](https://pkg.go.dev/badge/github.com/bojieli/agentswap.svg)](https://pkg.go.dev/github.com/bojieli/agentswap)
[![Dependencies: none](https://img.shields.io/badge/dependencies-none-brightgreen)](#security-and-privacy)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**Keep coding when an AI subscription runs out, a provider becomes flaky, or
you need to continue in a different coding-agent harness.**

## Why agentswap exists

A coding-agent subscription can be expensive and still run out in the middle
of a long refactor. A second subscription may also reach its weekly limit, and
pay-as-you-go API usage can cost much more than the subscription.

Third-party providers such as Krill AI can be a useful lower-cost fallback,
but any provider can be intermittent. When a request fails, repeatedly typing
`continue` is a poor way to recover the context you already built.

There is another common dead end: Claude Code has no usable capacity, while
Codex still has credits (or the other way around). Starting over in the other
harness throws away the session history.

agentswap is built for these moments. It keeps your credentials local, makes
recovery explicit, and preserves the recorded conversation when you move.

## Three ways it keeps work moving

| Need | What agentswap does | Main command |
| --- | --- | --- |
| More capacity in the same harness | Pools subscriptions, API keys, and active same-protocol provider overrides; fails over only when an account is actually unavailable. | `agentswap import`, `agentswap run` |
| A temporary limit or outage | Waits through a known reset, retries transient overload, and resumes a session after a wait longer than a client socket should remain open. | `agentswap run -- ...` |
| Another harness still has capacity | Validates and translates the native session into a new Claude Code, Codex, OpenCode, or Kimi Code session. | `agentswap teleport`, `agentswap handoff` |

agentswap never changes harnesses silently. You decide when a session should
move from Claude Code to Codex, from Codex to Claude Code, or to another
supported harness.

## Install

### Homebrew on macOS or Linux

After the first public release, Homebrew can download the checksum-pinned
formula directly from GitHub:

```sh
brew install --formula https://github.com/bojieli/agentswap/releases/latest/download/agentswap.rb
```

Apple Silicon, Intel, and Linux architectures select their matching archive
automatically. See [the release guide](docs/releases.md) for how the formula
is generated and published.

### Go toolchain

```sh
go install github.com/bojieli/agentswap/cmd/agentswap@latest
```

### Verified binary installer

The installer downloads the archive and verifies its SHA-256 checksum before
placing the binary in a user-writable directory:

```sh
curl -fsSL https://raw.githubusercontent.com/bojieli/agentswap/main/install.sh | sh
```

For a private repository, GitHub does not serve release assets anonymously.
Use authenticated downloads or wait until the repository and its releases
are public.

## Five-minute setup

1. Make sure you are signed in to the CLIs you want to use.
2. Preview what agentswap can discover:

   ```sh
   agentswap import --dry-run
   ```

3. Import the credentials and active provider settings:

   ```sh
   agentswap import
   ```

4. Point Claude Code and Codex at the local proxy:

   ```sh
   agentswap install
   ```

5. Keep the daemon running in the background:

   ```sh
   agentswap service install
   ```

6. Check the wiring and the account pool:

   ```sh
   agentswap doctor
   agentswap list
   ```

The existing CLIs continue to be invoked normally:

```sh
claude
codex
```

Run `agentswap serve` in a terminal when you want to watch the proxy instead
of installing a per-user service.

## Import subscriptions and provider fallbacks

`agentswap import` reads the credentials already stored by Claude Code and
Codex. It does not create a new OAuth flow and it never guesses at an
environment variable that you did not explicitly add.

When a CLI has an active provider override, agentswap imports both sides as
separate entries:

- the native subscription login; and
- the configured Anthropic-compatible or OpenAI-compatible provider, including
  its base URL and authentication style.

That means a setup containing a native Claude subscription and a Krill AI
provider is represented as two choices, rather than one replacing the other.
The same rule applies to Codex and any other provider that speaks the lane's
protocol.

Preview and import safely:

```sh
agentswap import --dry-run
agentswap import
agentswap status
```

Add another signed-in account by logging in through its own CLI, then running:

```sh
agentswap login --id work
```

For a provider key or a long-lived Claude token, use a prompt or stdin so the
secret does not land in shell history:

```sh
agentswap add-key anthropic --id krill --base-url https://provider.example
agentswap add-token anthropic
```

The complete account and provider guide is [docs/accounts.md](docs/accounts.md).

## Choose your recovery path

### Keep the current harness running

`agentswap run` supervises a CLI. It lets the proxy park a request while a
quota window resets, then resumes the native session if the wait outlives the
client's normal socket timeout:

```sh
agentswap run -- claude "refactor the parser"
agentswap run -- codex exec "fix the failing tests"
```

The first command is still a Claude Code session and the second is still a
Codex session. The supervisor does not change models or provider profiles.

### Continue in another harness

Use this when the current provider is unavailable but another harness still
has capacity. For example, if Claude Code has exhausted both subscriptions
and its provider fallback, continue in Codex without starting from a blank
prompt:

```sh
agentswap handoff claude codex
```

`handoff` validates the newest session in the current directory, creates a
native Codex session, and immediately launches `codex resume <id>`.

Use `teleport` when you want to create the target session but inspect or launch
it yourself:

```sh
agentswap teleport claude codex
agentswap teleport claude codex --dry-run
```

The source and target are always positional and always mean **source →
target**. You can select an exact source session or directory:

```sh
agentswap handoff claude codex --session <source-id>
agentswap teleport codex opencode --cwd ~/src/project
```

See the user guide in [docs/sessions.md](docs/sessions.md) and the exact flag
reference in [docs/commands.md](docs/commands.md).

## What transfers, and what does not

Teleportation preserves the recorded conversation rather than reducing it to
a summary prompt. It carries messages, reasoning that the source records,
tool calls and results, call ids, plans, timestamps, model metadata, and
supported inline media.

It deliberately does not move credentials, provider KV caches, hidden or
encrypted runtime state, approvals, live shell processes, background tasks,
or in-memory plugin state. The target is a new native process with its own
permissions and provider configuration.

The source is read-only. Validation happens before the target is written, and
unsupported conversation-bearing records fail closed. OpenCode sessions are
read and written through OpenCode's own `export` and `import` commands.

Supported source and target harnesses are:

| Harness | Native session support |
| --- | --- |
| Claude Code | JSONL read/write and native resume |
| Codex | rollout read/write and `codex resume` |
| OpenCode | native `export`/`import` boundary |
| Kimi Code | current and legacy session formats |

## Reliability boundaries

agentswap distinguishes failures that look similar to a client:

| Situation | Response |
| --- | --- |
| Short per-minute throttle | Wait on the same account to preserve its prompt cache. |
| Quota window exhausted | Retire the account and try the next eligible account. |
| Overloaded server or transient 5xx | Retry with backoff and jitter. |
| Stale login | Refresh once, then explain exactly which account needs `agentswap login`. |
| Invalid request | Return the provider's error without rotating accounts. |

When every account is spent, the default behavior is to park the request for
up to 30 minutes. After that, agentswap returns a `503` with `Retry-After` and
writes the ticket used by `agentswap run`.

## Security and privacy

agentswap holds live OAuth tokens, so it has no third-party Go dependencies.
The proxy listens on loopback by default, checks the `Host` header, substitutes
credentials from the pool instead of forwarding the client's credential, and
writes sensitive files with private permissions and atomic replacement.

Session files can contain source code, prompts, and tool output. Treat both
source and target session stores as sensitive local data.

Read the full [security model](SECURITY.md) before exposing the daemon or
using a third-party base URL.

## Terms of service

agentswap is failover-only: exactly one account is in flight at a time, and
rotation occurs only when the current account is genuinely unavailable. It is
not a parallel throughput multiplier.

Using multiple subscriptions or a third-party provider may still be governed
by that provider's terms. You are responsible for deciding whether your use is
allowed; agentswap is not legal advice.

## Limitations

- A stream that fails after partial output cannot be transparently retried.
- Codex learns some quota information from a rejected stream rather than
  successful response headers.
- Automatic wait-and-resume requires `agentswap run`; a bare CLI receives the
  proxy's error when its request cannot be held any longer.
- Imported OAuth credentials are copies of the CLI's current session. Use a
  long-lived token or an API key for an account that agentswap should own.
- OpenCode must be installed for OpenCode discovery or a non-dry-run target.
- Teleportation cannot move hidden provider state, active processes,
  credentials, approvals, or unsupported media.

See [docs/troubleshooting.md](docs/troubleshooting.md) when a command does not
behave as expected.

## Scope and prior art

Several projects rotate coding-agent accounts. agentswap focuses on the gaps
around rotation: waiting through a reset, preserving a warm conversation,
resuming after a long hold, and moving a structured session between harnesses.

If you only need account rotation, compare projects such as
[CC-Router](https://github.com/VictorMinemu/CC-Router),
[claude-swap](https://github.com/realiti4/claude-swap),
[teamclaude](https://github.com/KarpelesLab/teamclaude),
[llmux](https://github.com/2lab-ai/llmux), and
[coding_agent_account_manager](https://github.com/Dicklesworthstone/coding_agent_account_manager).
The scope here stays deliberately narrow: one account in flight, no live
protocol translation, and no third-party dependencies.

## Contributing

Bug fixes, clearer documentation, tests, and adapters for additional coding
agents are welcome. A useful contribution usually starts with a reproducible
failure and a test that describes the behavior before the fix.

The project deliberately has no third-party Go dependencies because it holds
live credentials. Read [CONTRIBUTING.md](CONTRIBUTING.md) before adding a
provider lane, a session adapter, or a dependency.

Please report credential leaks and other security vulnerabilities privately via
the process in [SECURITY.md](SECURITY.md), never in a public issue.

## Documentation map

- [Getting started and documentation index](docs/README.md)
- [Accounts, subscriptions, keys, and provider overrides](docs/accounts.md)
- [Session recovery, teleport, and handoff](docs/sessions.md)
- [Complete command reference](docs/commands.md)
- [Configuration](docs/configuration.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Architecture and adding a lane](docs/architecture.md)
- [Acceptance and compatibility evidence](docs/acceptance.md)
- [Live teleport acceptance](docs/teleport-live-acceptance.md)
- [Releases and Homebrew publishing](docs/releases.md)
- [Contributing](CONTRIBUTING.md)
- [Security](SECURITY.md)
- [Code of conduct](CODE_OF_CONDUCT.md)
- [Changelog](CHANGELOG.md)

## Project status

The core proxy, importer, supervisor, and session adapters are covered by unit
and end-to-end tests. Compatibility with real provider and harness versions is
recorded in the acceptance documents, but this is still early software: test
it with a disposable account pool before relying on it for unattended work.

## License

MIT
