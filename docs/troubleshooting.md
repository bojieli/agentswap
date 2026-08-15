# Troubleshooting

Start with `agentswap doctor`, and `agentswap config` when the question is
"where is it even reading that from". It walks the chain in the order a request
travels it, so the first failure it reports is the first thing to fix.

```
$ agentswap doctor
[ok  ] anthropic lane has accounts (2)
[    ] openai lane has no accounts
       fine if you do not use it; otherwise `agentswap add-key openai --key ...`
[ok  ] daemon is listening on 127.0.0.1:8420
[ok  ] Claude Code is pointed at agentswap
[ok  ] Codex is pointed at agentswap
```

`[    ]` is a note, not a problem. Only `[FAIL]` counts, and only `[FAIL]`
makes the exit code non-zero.

---

## Nothing seems to go through agentswap

**`agentswap status` shows no requests, and the daemon logs nothing.**

For Codex, the usual cause is a missing profile. Codex has no equivalent of
Claude Code's automatic settings pickup, so `codex` on its own quietly uses
your original account and bypasses the proxy entirely:

```sh
codex --profile agentswap
```

`agentswap run -- codex ...` adds this for you.

For Claude Code, check that the settings file was actually picked up. Anything
that sets `ANTHROPIC_BASE_URL` in your shell overrides what `install` wrote,
because the environment beats `settings.json`:

```sh
env | grep ANTHROPIC
```

## `doctor` says a CLI is pointed at a different address

The daemon is listening somewhere other than what the CLIs were configured
with — usually because it was started with `serve --addr`. Either start it
without the override, or set `addr` in `config.json` and re-run
`agentswap install` so both ends agree.

## An account was rejected, and says it needs a new sign-in

The upstream revoked the login. This is the one failure that does not fix
itself: waiting does nothing, and rotating only spends the next account.

```
work was rejected: refresh failed with 401 Unauthorized
  sign in again:  agentswap login --id work
```

Sign in as that account, then run exactly that. The credential is replaced in
place — same id, same priority, same label — and the account goes straight back
into rotation.

`agentswap import` is *not* the fix. It re-reads the credential the upstream
just refused, so it looks like the fix did nothing.

Why it happened, usually:

- **You used that account outside agentswap.** Both upstreams rotate the
  refresh token when it is used, so whichever side refreshes second is holding
  one the server has retired. Running your CLIs through agentswap avoids it.
- You signed out, in the CLI or on the web.
- The upstream expired the session, which it may do whenever it likes.

## Requests fail with `no_accounts`

The lane has no usable account. `agentswap status` says which:

- **`needs login`** — the credential was rejected; see the section above.
- **`disabled`** — `agentswap enable <id>`.
- **all `exhausted`** — everything is genuinely spent. The summary line says
  when the first one comes back.

If the pool is empty entirely, `agentswap import` adopts whatever logins are
already on the machine.

## Requests hang for a long time and then fail

That is parking working as designed up to `park.max_hold`, then giving up. The
503 says when to come back.

If you want the session continued instead of failed, run it supervised:

```sh
agentswap run -- claude "refactor the parser"
```

If requests fail *before* `max_hold` with a client-side timeout, the CLI's own
deadline is shorter than the daemon's. `agentswap install` derives it from
`park.max_hold`, so re-run `install` after changing that setting.

## A 421 in the logs

The `Host` header named something agentswap does not recognise. If that was
you — reaching it through a tunnel or from a container — add the name to
`allowed_hosts` in `config.json`. If it was not you, a page in your browser
tried to use your subscription, and the refusal is the feature working.

## An account is talking to the wrong upstream

`agentswap list` shows each account's provider. To move one, or to put it back
on the vendor's own API:

```sh
agentswap set corp --base-url https://llm.corp.example.com/v1
agentswap set corp --base-url ""
```

The same command changes a priority or a label. The account keeps its id, so
its observed quota and pinned conversations survive the change.

## Rotation happens too often and responses cost more

Every rotation abandons a warm prompt cache. Check that `rotation.sticky` is
`true`, and that `sticky_ttl` is at least as long as your upstream's cache
lifetime.

If accounts rotate on short throttles, `retry.burst_cutoff` is too low: a 429
that clears in twenty seconds should be waited out, not rotated away.

## Rotation happens too late — requests still hit 429s

Lower `rotation.drain_above`. The default of 98 assumes the account reports
utilization finely enough to see 98% before it refuses. Codex is reactive here
regardless: it reports its real quota inside the SSE stream rather than in
headers, so that lane learns an account is spent from the 429.

## A stream fails partway through

Cannot be retried transparently, and agentswap does not pretend otherwise. Once
bytes have reached the client, replaying the request would duplicate output.
Those errors reach the CLI, which will usually offer to continue.

## The daemon stops when I close the terminal

That is what a foreground process does. To keep it running, and start it again
at login:

```sh
agentswap service install
agentswap service status
```

On Linux a systemd user unit also stops at logout unless lingering is enabled:
`sudo loginctl enable-linger $USER`. On Windows there is no per-user service
manager agentswap writes for; `agentswap service install` prints the Task
Scheduler and Startup-folder options.

## A third-party provider returns 404 for everything

Check the base URL for a doubled path. Claude Code sends `/v1/messages`, so an
anthropic-lane base URL ending in `/v1` reaches the provider as
`/v1/v1/messages`:

```sh
agentswap set corp --base-url https://llm.corp.example.com     # not .../v1
```

The openai lane is the other way round: its own default ends in `/v1`, because
Codex sends a bare `/responses`.

## The daemon will not start

**`address already in use`** — an older daemon is still running. Find it with
`agentswap status` (which reports the address it published) or
`lsof -i :8420`.

**A config error** — the message names the file and the field. `config.json` is
validated on load precisely so a typo fails at startup rather than behaving
strangely under load.

## Starting over

```sh
agentswap uninstall              # restores the CLI config files
rm -rf ~/.config/agentswap       # forgets the pool and its state
```

`uninstall` only removes values it recognises as its own, and every edit it
ever made was preceded by a timestamped backup next to the original file.

## Getting help

Open an issue with the full `agentswap doctor` output, which is usually the
whole answer. If the problem involves a credential being leaked or wrongly
accepted, report it privately instead — see [SECURITY.md](../SECURITY.md).
