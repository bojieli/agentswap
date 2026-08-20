# agentswap documentation

Start with the question that matches what you are trying to do. The
[root README](../README.md) explains the problem agentswap solves; these pages
provide the details you need at the terminal.

## I want to get started

- [README quick start](../README.md#five-minute-setup) — install, import,
  configure, and start the daemon.
- [Accounts and provider overrides](accounts.md) — import native logins,
  preserve an active Krill AI or other same-protocol provider, add keys, and
  pool another account.
- [Troubleshooting](troubleshooting.md) — diagnose wiring, credentials,
  quota waits, and provider errors with `agentswap doctor`.

## I want to keep a task moving

- [Session recovery, teleport, and handoff](sessions.md) — choose between
  waiting in the current harness and continuing in another one.
- [Command reference](commands.md) — every command, flag, and environment
  variable.
- [Configuration](configuration.md) — tune rotation, retries, parking, and
  allowed hosts.

## I want to understand or extend the project

- [Architecture](architecture.md) — request flow, account selection, session
  translation, and how to add a provider lane.
- [Acceptance record](acceptance.md) — deterministic, live-provider,
  cross-platform, and session-continuation evidence.
- [Live teleport acceptance](teleport-live-acceptance.md) — the real-harness
  compatibility matrix and its safety requirements.
- [Contributing](../CONTRIBUTING.md) — tests, design rules, and how to submit a
  bug fix or new harness adapter.
- [Releases and Homebrew](releases.md) — tag a release and publish verified
  archives and the formula.

## I need the project policies

- [Security](../SECURITY.md) — credential handling, local threat model, and
  private vulnerability reporting.
- [Code of conduct](../CODE_OF_CONDUCT.md)
- [Changelog](../CHANGELOG.md)

## Quick command map

```text
agentswap import             discover existing logins and provider overrides
agentswap install            wire Claude Code and Codex to the proxy
agentswap service install    run the proxy at every login
agentswap run -- ...         wait and resume in the same harness
agentswap teleport A B       create a native B session from A
agentswap handoff A B        create and launch that target session
agentswap doctor             find the first broken link in the setup
```

The command order for transfer is always **source, then target**. For example,
`agentswap handoff claude codex` means “continue the Claude session in Codex.”
