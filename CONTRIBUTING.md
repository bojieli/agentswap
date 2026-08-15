# Contributing

Thanks for looking. Bug reports and small, well-argued patches are the most
useful things you can send.

## Getting set up

```sh
git clone https://github.com/bojieli/agentswap
cd agentswap
make check          # build, vet, test with the race detector, gofmt
```

There is nothing else to install: agentswap has no third-party dependencies,
so a Go toolchain is the whole toolchain. `make lint` additionally runs
[golangci-lint](https://golangci-lint.run) if you have it.

Run the daemon against a scratch pool rather than your real one:

```sh
AGENTSWAP_HOME=/tmp/agentswap-dev go run ./cmd/agentswap serve -v
```

`AGENTSWAP_HOME` moves the accounts, config and state files together, so a
development daemon cannot touch the credentials you actually use.

## The one hard rule: no dependencies

`go.sum` must stay empty, and CI fails if it is not. This process holds live
OAuth tokens for your Anthropic and OpenAI accounts. Every dependency is
another maintainer who could publish a version that reads them, and the
standard library has been enough so far.

If you genuinely cannot do something without a dependency, open an issue
before writing the code — the answer may be that the feature is not worth it.

## What good change looks like

**Explain the failure, not the fix.** The comments in this codebase say why a
decision was made, because the "what" is already in the code. A comment like
`// retry 3 times` is noise; `// Overload is rarely account-specific, so absorb
a few on the same account before spending another one's quota on it` is the
reason nobody has to rediscover.

**Bring a test that fails first.** Especially for anything touching rotation,
retry or parking: those paths are hard to reach by hand and easy to break
invisibly. The existing tests are written to be readable as a specification —
`TestBurstLimitStaysOnSameAccount` says what the system promises. Aim for that.

**Keep failover honest.** agentswap is failover-only by design: exactly one
account is ever in flight, and it moves only when the current one is genuinely
refused. Patches that run accounts in parallel to multiply throughput will be
declined — see the terms-of-service section of the README for why.

**Do not log credentials.** Not at debug level, not in an error message, not
in a struct dump. `Account.Display()` exists so that logging an account is
safe by default.

## Testing conventions

- Table tests where the cases are genuinely parallel; separate named tests
  where each case is its own story.
- No real network. Every upstream in the test suite is an `httptest.Server`.
- No real clock for anything longer than milliseconds. The engine takes a
  `now func() time.Time` and a `Waiter` precisely so that a five-hour park is
  a fast test — see `fakeClock` in `internal/engine/engine_test.go`.
- No writes outside `t.TempDir()`. Tests that touch the config directory set
  `AGENTSWAP_HOME`, `CODEX_HOME` or `CLAUDE_CREDENTIALS_PATH` with `t.Setenv`.

Run `go test -race -shuffle=on ./...` before sending; CI runs it on Linux,
macOS and Windows, against the oldest supported Go and the current stable one.

## Commit messages

Present tense, and say what changes for the user rather than what changed in
the code. If a commit fixes something subtle, the body should be the
explanation you would want to find in `git blame` a year from now.

## Reporting a bug

`agentswap doctor` output is the single most useful thing to include; it walks
the chain in the order a request travels it. Please also say which CLI, which
version of it, and whether the accounts involved are subscriptions or API keys.

If the bug involves a credential being rejected or leaked, do not open a public
issue — see [SECURITY.md](SECURITY.md).
