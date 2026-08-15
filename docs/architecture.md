# Architecture

agentswap is a local HTTP proxy with a credential pool behind it. The whole
design follows from one constraint: the client is a coding agent that treats
any error as a reason to stop, so the interesting work is deciding what *not*
to pass through.

## The path of a request

```
claude / codex
      │  http://127.0.0.1:8420/anthropic/v1/messages
      ▼
 proxy.Server ────────── Host check, body buffered (replay needs it)
      │
      ▼
 engine.Execute ──────── the loop: select → send → classify → decide
      │      ▲
      │      └────────── retry / rotate / park, invisibly
      ▼
 lane.Lane ───────────── protocol: authorize, read quota, classify a failure
      │
      ▼
 api.anthropic.com  /  chatgpt.com/backend-api/codex
```

Everything above `lane.Lane` is protocol-agnostic. Everything below it is one
vendor's wire format. That seam is what makes adding a third lane a contained
change.

## Packages

| Package | Holds |
| --- | --- |
| `cmd/agentswap` | The CLI. One file per command group; no logic worth testing lives here except argument shaping. |
| `internal/proxy` | HTTP front end, streaming relay, parked-connection keepalive, Host checking. |
| `internal/engine` | Selection, retry, rotation, parking. The decisions. |
| `internal/lane` | The protocol interface, plus header parsing shared by lanes. |
| `internal/lane/anthropic`, `internal/lane/openai` | One adapter each. |
| `internal/store` | The credential pool and observed health, and their files. |
| `internal/config` | Settings, defaults, validation. |
| `internal/install` | Editing the CLIs' own config files, reversibly. |
| `internal/importer` | Reading credentials the CLIs already wrote. |
| `internal/supervisor` | `agentswap run`: resuming a session after a wait too long to hold a socket. |
| `internal/daemon` | Where a running daemon published its address. |

## The engine loop

`engine.Execute` runs until it has a response worth returning — a success, or a
client error that would fail identically anywhere. Everything else is absorbed:

1. **Select.** The conversation's previous account if it is still usable
   (prompt caches are per-account), otherwise the store's order: subscriptions
   before API keys, then priority, then id. Accounts already tried this request
   are skipped, as are accounts whose observed utilization is above
   `drain_above` — unless that leaves nothing, in which case a drained account
   is better than parking.

2. **Send.** Client headers forwarded verbatim minus hop-by-hop ones, then the
   lane replaces the credential. Whatever the client sent is discarded; routing
   is decided by the pool, not by the caller.

3. **Classify.** The lane turns a response into one of five actions. This is
   where the three failure modes get separated, which is the hard part: they
   all arrive as a 429 or a 5xx and each wants the opposite response.

   | Action | When | Effect |
   | --- | --- | --- |
   | `Relay` | success | return it, streaming |
   | `RetrySame` | short throttle, or overload | wait, same account, warm cache |
   | `Rotate` | window spent | mark exhausted until reset, next account |
   | `RefreshAuth` | 401/403 | renew the token, retry |
   | `Fatal` | any other 4xx | hand the client its own error |

4. **Park.** When nothing is selectable, wait for the earliest reset — but only
   up to `park.max_hold`. Past that, a 503 with `Retry-After` and a resume
   ticket for `agentswap run`.

The loop carries per-request state (which accounts were tried, the overload
streak) and mutates per-account state through the store. That split is
deliberate: a retry budget belongs to a request, an exhausted window belongs to
an account and outlives it.

## Concurrency

The store hands out **clones**, never pointers into the pool. A request holds
an account for its whole life while another goroutine may be renewing that
account's token, and sharing the struct would be a data race.

Token refresh is **coalesced per account**. Both upstreams rotate the refresh
token when it is used, so two requests that both notice the same expired token
and both post it mean the second presents a credential the server has already
retired — which retires the account. Concurrent refreshes wait on one exchange,
and a caller that arrives just after one lands picks up the new token instead
of spending another.

Health is mutated under lock on the hot path and flushed to disk on a ticker,
so no request ever blocks on I/O to record that it succeeded.

## Time

Nothing in the engine calls `time.Now` directly; it takes `now func() time.Time`
and every deadline it acts on comes from that one source. Waiting goes through a
`Waiter`, which the proxy implements as "hold the connection" and tests
implement as "advance the clock". That is why a test covering a five-hour park
runs in microseconds.

## The two test suites

`internal/...` and `cmd/...` cover decisions, with a fake clock and a fake
upstream, so a five-hour park is a fast test.

`e2e/` compiles the binary and drives it as a subprocess — argv, exit codes,
files on disk, HTTP. It imports no internal package on purpose, so a refactor
that breaks the product cannot be hidden by a refactor of the tests. Several
bugs here were only ever visible from there: health lost on shutdown, a
four-second quota window waited out for a minute, a beta header silently
dropped.

## What is deliberately absent

**No request translation.** A lane is a wire protocol, and any account in a
lane can serve any request in it without touching the body. Translating between
protocols would mean owning a mapping that breaks every time either vendor ships
a feature.

**No parallelism across accounts.** Exactly one account is ever in flight.
Running several to multiply throughput is both a different product and a much
worse position to be in with respect to a provider's terms.

**No dependencies.** This process holds live OAuth tokens; every dependency is
another party with access to them.

## Adding a lane

Implement `lane.Lane` — six methods — and register it in `buildLanes()` in
`cmd/agentswap/common.go`, then add a path prefix in `internal/proxy`. The
engine needs no changes.

The work that is not obvious is `Classify` and `Observe`: what this vendor's
"you are going too fast" looks like versus its "you are out of quota", and
where it reports remaining quota on a *successful* response. Get those wrong and
rotation is either too eager, which throws away prompt caches, or too late,
which is what everything else already does.
