# Configuration

**Most people never open this file.** Every field has a working default, and
the defaults are chosen for a laptop with two or three pooled accounts.

Start with the effective configuration instead of editing a blank file:

```sh
agentswap config
agentswap config --write
```

For account credentials and provider URLs, use the commands in
[accounts.md](accounts.md). For a symptom-based explanation of a setting,
start with [troubleshooting.md](troubleshooting.md).

The three settings that people do change, and why:

| Setting | Change it when |
| --- | --- |
| `park.max_hold` | you would rather wait longer than be handed a 503. Re-run `agentswap install` afterwards, so your CLI's own timeout follows. |
| `rotation.drain_above` | requests still hit a 429 before rotating — lower it |
| `retry.burst_cutoff` | short throttles are rotating away your prompt cache — raise it |

Everything else is below, in the order it appears in the file.

`~/.config/agentswap/config.json`. A file that sets one field keeps the
defaults for all the others.

Durations are written the way people say them: `"30m"`, `"90s"`, `"1h30m"`. A
bare number means seconds.

Because every field has a default, an absent file tells you nothing about the
values in effect. To see them, and to start editing from a complete file rather
than a blank one:

```sh
agentswap config           # the values in effect, and where every file lives
agentswap config --write   # save them to config.json, ready to edit
```

Do not redirect `agentswap config --json` into `config.json` yourself: the
shell truncates the file before agentswap reads it.

```json
{
  "addr": "127.0.0.1:8420",
  "allowed_hosts": [],
  "rotation": { "drain_above": 98, "sticky": true, "sticky_ttl": "30m" },
  "retry":    { "burst_cutoff": "2m", "overload_initial": "1s",
                "overload_max": "1m", "rotate_after": 3,
                "auth_refresh_attempts": 1 },
  "park":     { "enabled": true, "buffer": "1m", "max_hold": "30m",
                "keepalive": "silent", "keepalive_interval": "15s" }
}
```

Where the files live is controlled by the environment, not by this file:
`AGENTSWAP_HOME` wins, then `XDG_CONFIG_HOME/agentswap`, then
`~/.config/agentswap`. `teleport --compact` writes its session archives into an
`archives/` subdirectory of that same location.

The CLIs' own files are found the way the CLIs find them: `CLAUDE_CONFIG_DIR`
and `CODEX_HOME` are honoured, so `agentswap install` writes where the CLI
will actually read.

## Top level

### `addr`
Default `127.0.0.1:8420`, or `AGENTSWAP_ADDR` when that is set.

Where the daemon listens, and where every other command looks for one. Loopback
is the point: anything that can reach this port gets answers paid for with your
subscriptions. Binding elsewhere warns at startup.

`AGENTSWAP_ADDR` moves that default the way `AGENTSWAP_HOME` moves the config
directory, which is what a second installation on one machine needs: without
it, two daemons with no `addr` in their config files both fall back to the same
port, and each one's commands reach whichever daemon bound it first. This file
still wins over the variable, and `serve --addr` still wins over both.

Changing this means re-running `agentswap install`, since the CLIs are
configured with the address baked in. `agentswap serve --addr` overrides it for
one run; `status` and `doctor` will still find the daemon, because it publishes
its actual address on startup.

### `allowed_hosts`
Default empty.

Extra `Host` header values the proxy will answer to. Loopback names and
`addr`'s own host are always accepted.

This exists because of DNS rebinding: a page you visit can point its own domain
at 127.0.0.1 and send requests to a local server, which agentswap would
otherwise answer with real credentials. It cannot forge the `Host` header, so
unrecognised names are refused. Add a name here if you reach agentswap
deliberately by some other one — from a container, or across a tunnel.

## `rotation`

### `drain_above`
Default `98`. Percent, in (0, 100].

Retire an account once an observed quota window crosses this utilization,
instead of waiting for it to start refusing. This is what the quota headers
buy: without it, the first request of every window is spent discovering the
limit.

Lower it if you see requests still hitting 429s before rotation — some accounts
report utilization coarsely. Raise it toward 100 to squeeze out the last of a
window at the cost of the occasional wasted request.

Draining is a preference, not a prohibition: if every account is above the
threshold and none has actually been refused, the request is still sent.

### `sticky`, `sticky_ttl`
Default `true`, `"30m"`.

Keep a conversation on the account that last served it, matched by a hash of
the request prefix — which is very nearly the prompt-cache key.

Prompt caches are per-account. Rotating for its own sake converts cache hits
into full-price misses, so this is on by default and there is rarely a reason
to turn it off. `sticky_ttl` should exceed your upstream's cache lifetime;
below it, you pay for rotation you gained nothing from.

## `retry`

### `burst_cutoff`
Default `"2m"`.

The line between "you are going too fast" and "this window is spent". A 429
telling us to wait less than this is a per-minute throttle: wait and reuse the
same account. Longer than this means rotate.

Getting this wrong is expensive in both directions. Too low and short throttles
rotate away a warm cache; too high and a spent account is waited on for minutes
before moving to one that would have answered immediately.

### `overload_initial`, `overload_max`
Default `"1s"`, `"1m"`.

Exponential backoff with full jitter for 529s, 5xx and transport failures.
Retries are unbounded on purpose: an overloaded upstream is temporary, and
surfacing it is what strands the agent.

The jitter matters when several agents share one pool. Without it they retry in
lockstep and re-overload the upstream the moment it recovers.

### `rotate_after`
Default `3`.

How many consecutive overload responses to absorb on one account before trying
another. Overload is rarely account-specific, so the first few are worth
absorbing rather than spending a second account's quota on them — but a fault
that is account-scoped should not pin the request forever.

### `auth_refresh_attempts`
Default `1`.

How many times a request may renew one account's token in response to a 401
before that account is treated as genuinely rejected. More than one attempt
almost never helps: if a freshly minted token is refused, the problem is the
account.

The budget is per account, not per request. A request that rotates through
several accounts gives each one the same allowance, so an account is never
condemned unrefreshed because an earlier one had already used the budget up.

## `park`

### `enabled`
Default `true`.

When every account is spent, hold the request until quota returns instead of
returning an error the agent would treat as fatal. Turning this off makes
agentswap fail fast, which is only what you want if something upstream of it
does its own waiting.

### `buffer`
Default `"1m"`.

Added to every observed reset time. Server and client clocks disagree, and
retrying one second early wastes the entire wait.

### `max_hold`
Default `"30m"`.

How long a request may be parked before agentswap gives up and returns a 503
with `Retry-After`, on the theory that holding a socket for five hours is worse
than telling you when to come back. It also writes a resume ticket, which is
what makes `agentswap run` able to continue the session.

`agentswap install` derives the CLI's own request timeout from this, so raising
it means re-running `install` — otherwise the client gives up before the daemon
does, and a wait that was about to succeed fails anyway.

### `keepalive`
Default `"silent"`. Either `"silent"` or `"ping"`.

`silent` holds the connection with no bytes written. It is the safe choice: the
client is on loopback, so nothing in between can time out an idle socket, and
writing nothing means never committing to a status code that might have to be
taken back.

`ping` commits to a `200 text/event-stream` and emits SSE pings while waiting.
It is strictly riskier — once the status line is sent it cannot be retracted,
so a later failure has to be reported as an in-band SSE error event instead of
an HTTP status. Use it only if a client refuses to wait without seeing bytes.

### `keepalive_interval`
Default `"15s"`. Only used by `keepalive: "ping"`.

## Per-account settings

These live in `accounts.json`, not `config.json`, and are set by the commands
rather than edited by hand.

| Field | What |
| --- | --- |
| `priority` | Lower is preferred within a lane. `agentswap add-key --priority` |
| `enabled` | `agentswap disable` / `enable` |
| `base_url` | Override the upstream for one account, for a same-protocol third-party provider. `agentswap add-key --base-url` |
| `auth_style` | Optional `bearer` or `x-api-key`; import sets it when a provider uses a non-native header style. |

Subscriptions are always tried before API keys regardless of priority, because
they are already paid for.

The file is safe to edit by hand. Only `id`, `lane`, `kind` and the credential
are required; an absent `enabled` means enabled, since disabling is the
deliberate act.
