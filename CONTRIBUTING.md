# Contributing

Bug fixes, tests, documentation improvements, and adapters for additional
coding-agent harnesses are welcome. The most useful contribution starts with a
real user problem and makes the expected behavior easy to verify.

Before changing code, read the user-facing [README](README.md) and the relevant
guide:

- account or provider behavior: [docs/accounts.md](docs/accounts.md);
- recovery and session transfer: [docs/sessions.md](docs/sessions.md);
- command behavior: [docs/commands.md](docs/commands.md);
- design boundaries: [docs/architecture.md](docs/architecture.md).

## Set up a development checkout

```sh
git clone https://github.com/bojieli/agentswap
cd agentswap
make check
```

The only required toolchain is Go. agentswap has no third-party Go
dependencies. `make lint` additionally runs
[golangci-lint](https://golangci-lint.run) when it is installed.

Run development commands against a disposable configuration directory:

```sh
AGENTSWAP_HOME=/tmp/agentswap-dev go run ./cmd/agentswap serve -v
```

`AGENTSWAP_HOME` moves accounts, configuration, and state together so a test
daemon cannot touch your real credentials.

## What to contribute

### Bug fixes

Describe the failure in user terms: what command ran, what was expected, and
what happened instead. Add a regression test that fails before the fix,
especially for rotation, retry, parking, importing, or session translation.

### New harness adapters

Session adapters should read the native format, validate the complete history,
and write through the target's safest native interface. Keep the source
read-only. Do not replace a conversation with a summary prompt when the native
format can preserve structured events.

### New provider lanes

Implement the `lane.Lane` interface, register the lane in
`cmd/agentswap/common.go`, and add the proxy path in `internal/proxy`. The
engine should not need provider-specific changes.

Document how the provider distinguishes a short throttle, exhausted quota,
overload, authentication failure, and an invalid request. Incorrect
classification either wastes prompt-cache value by rotating too early or
leaves a session stuck on an account that cannot recover.

### Documentation and examples

Keep pages task-oriented. Start with the user's goal, use short paragraphs,
show a copyable command, and link to the exact reference section for advanced
flags. Update the README and [docs/README.md](docs/README.md) when adding a
new user-visible capability.

## The security rule: no dependencies

`go.sum` must stay empty, and CI fails if it is not. This process holds live
OAuth tokens for Anthropic- and OpenAI-compatible accounts. Every dependency is
another party with access to the process and another supply-chain update to
trust.

If a change seems to require a dependency, open an issue first. The standard
library has been sufficient so far, and a small local implementation may be a
safer answer.

## Testing conventions

- Use `httptest.Server` rather than a real upstream in ordinary tests.
- Use the engine's fake clock and waiter instead of sleeping through long
  quota windows.
- Write files only under `t.TempDir()`; set `AGENTSWAP_HOME`, `CODEX_HOME`, or
  `CLAUDE_CREDENTIALS_PATH` with `t.Setenv`.
- Never print, persist, or compare a live credential. Use clearly fake fixture
  values and verify that logs mask them.
- Keep comments focused on why a non-obvious decision exists.

The repository has two complementary suites:

```sh
make unit       # fast unit and command tests
make e2e        # compiled-binary subprocess tests
make test       # race-enabled full suite
make cover      # merged unit and subprocess coverage
make check      # build, vet, format, dependency policy, and tests
```

Run the full race-enabled suite before sending a change:

```sh
go test -race -shuffle=on ./...
```

The CI matrix runs it on Linux, macOS, and Windows against the minimum and
stable supported Go toolchains. CI also checks Shell scripts, release targets,
the Homebrew formula, vulnerabilities, lint, and a coverage floor.

## Pull requests

Keep a pull request focused and explain:

1. the user-facing failure or need;
2. the behavior that changes;
3. the tests or acceptance evidence that cover it; and
4. any compatibility or provider terms-of-service considerations.

The pull request checklist also asks you to confirm that no credential can
reach a log line and that `go.sum` remains empty.

## Releases

Update `CHANGELOG.md`, commit the change, and push an annotated semantic
version tag:

```sh
git tag -a v0.2.0 -m 'agentswap v0.2.0'
git push origin v0.2.0
```

The release workflow reruns tests, builds the supported archives, publishes
`SHA256SUMS`, generates the Homebrew formula, and attaches build provenance for
public repositories. See [docs/releases.md](docs/releases.md).

## Commit messages and bug reports

Use a present-tense subject that explains what changes for a user. Put the
reasoning for subtle fixes in the body so it remains discoverable in history.

For a bug report, include:

- `agentswap doctor` output;
- the CLI and version involved;
- whether the accounts are subscriptions, API keys, or third-party providers;
- the command that failed and the relevant sanitized logs.

If a report involves a rejected or leaked credential, do not open a public
issue. Follow [SECURITY.md](SECURITY.md) instead.
