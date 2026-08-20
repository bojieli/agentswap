# Accounts, subscriptions, keys, and providers

This guide answers the most common setup question: “How do I make all the
capacity I already pay for available to agentswap without losing the provider
settings my CLIs use?”

The short version is:

```sh
agentswap import --dry-run  # inspect first
agentswap import            # copy native logins and active provider overrides
agentswap list               # check the pool
agentswap install            # route Claude Code and Codex through it
```

The importer preserves a native subscription and an active same-protocol
provider as separate entries. For example, a Codex login and a Krill AI
OpenAI-compatible provider can both be available to the OpenAI lane.

Related pages: [command reference](commands.md#getting-credentials-in),
[configuration](configuration.md), and [troubleshooting](troubleshooting.md).

## The whole picture

Everything you can put in the pool, and every way to put it there:

| What | How it gets in | How you change it |
| --- | --- | --- |
| A subscription you are signed in to | `agentswap import` | `agentswap login --id NAME` |
| The active Claude/Codex third-party provider | `agentswap import` | `agentswap set NAME --base-url URL` |
| A subscription you are not signed in to yet | `agentswap login` | `agentswap login --id NAME` |
| An API key, official or third-party | `agentswap add-key LANE` | `agentswap set NAME --key -` |
| Which upstream an account talks to | `add-key --base-url` | `agentswap set NAME --base-url URL` |
| The order accounts are tried in | `add-key --priority` | `agentswap set NAME --priority N` |
| A name you will recognise | `--label` on either | `agentswap set NAME --label TEXT` |
| Whether it is used at all | — | `agentswap disable` / `enable` / `remove` |

Everything else is settings rather than credentials, and lives in one file you
edit:

```sh
agentswap config          # where everything lives, and the values in effect
agentswap config --write  # save those values as a file to edit
```

Nothing here needs you to open `accounts.json`.

## A whole setup, start to finish

```sh
### 1. Adopt the login you already have
agentswap import

### 2. Pool a second and third account
agentswap login --id work
agentswap login --id side-project

### 3. Add a metered key as the last resort
agentswap add-key anthropic --id backup

### 4. Add a company gateway that speaks the same protocol
agentswap add-key anthropic --id corp \
  --base-url https://llm.corp.example.com --priority 150

### 5. Point your CLIs at agentswap, and run it
agentswap install
agentswap serve

### 6. Check it
agentswap list
agentswap doctor
```

Subscriptions are always tried before API keys, so 1–2 are spent before 3–4
are touched. Within each group, lower `--priority` goes first.

If a CLI currently selects a third-party provider, step 1 adopts that too. The
override and the vendor login become separate pool entries; importing one no
longer hides the other.

## Where things live, and why it is not one file

Three kinds of state, split by **who writes them**:

| File | Written by | Hand-edit? |
| --- | --- | --- |
| `config.json` | you | yes — that is its purpose |
| `accounts.json` | agentswap | possible, rarely worth it |
| `state.json` | agentswap | no; it is derived data |

The obvious design is one file with everything in it. It is worth saying why
that is a trap here.

**Credentials are rewritten while you are not looking.** OAuth tokens rotate:
agentswap refreshes an access token in the middle of a session, and both
upstreams hand back a new refresh token when it does. A file that you edit and
a daemon rewrites has an editor-versus-process race in it — you open the file,
the daemon renews a token, you save, and the renewed token is gone. Splitting
by writer removes the race instead of documenting it.

**Secrets want to be boring to handle.** A file people are told to open is a
file that ends up pasted into issues, screenshots and commits.
`agentswap add-key` never puts a key on screen, in your shell history, or in
the process list — that is what `--key -`, `AGENTSWAP_API_KEY` and the prompt
are for. Editing a JSON file has none of those affordances.

**Settings are the opposite.** `config.json` holds no secrets, changes rarely,
and is worth reading, diffing and copying between machines. Hand-editing is
exactly right for it.

So: settings are a file, credentials are commands. `accounts.json` stays
readable and hand-editable for scripted setup — only `id`, `lane`, `kind` and
the credential are required, and an absent `enabled` means enabled — but you
should not need to open it.

## Pooling several accounts

agentswap has no login of its own. Only `claude` and `codex` can mint a
credential, so pooling an account is always: sign in with the CLI, then adopt
what it stored — from `~/.claude/.credentials.json` or, on macOS, from the
Keychain where Claude Code puts it instead; and from `~/.codex/auth.json`,
which holds either a ChatGPT token set or a plain API key. `agentswap import`
also reads Claude Code's active `ANTHROPIC_BASE_URL` plus token/key and Codex's
selected `[model_providers.*]` table. It binds each credential to that provider
instead of silently treating a third-party key as an official-vendor key.
`agentswap login` removes every part of the login dance except the signing in.

```sh
agentswap login                 # pool another account
agentswap login --id work       # ...and name it
```

It works out whether you need to sign in at all, tells you what to run, waits
for the credential to appear, and adopts it. Sign in however you like —
another terminal, a browser, a device code — it watches for the result rather
than driving your CLI.

If you signed in before running it, there is nothing to wait for and it adopts
immediately.

Some Codex setups replace the ChatGPT login in `auth.json` when an API-key
provider is activated, so both credentials are not simultaneously discoverable
there. They can still coexist in agentswap: import while the provider key is
active, run `codex login`, then import again. The first key is already safe in
the pool and the second import adds the native subscription without deleting
it.

**One login is only ever pooled once.** Running `import` or `login` twice
against the same account updates it in place rather than adding a second row.
Two entries holding one credential would look like failover and be refused in
the same instant, which is the worst possible way to find out.

To check what you have:

```
$ agentswap list
ID                 LANE       KIND           PRI  STATE      CREDENTIAL
personal           anthropic  subscription   0    available  max
work               anthropic  subscription   0    available  pro
official-key       anthropic  api key        100  available  sk-ant-api…1234
gateway            anthropic  api key        110  available  …5678  → llm.corp.example.com
```

## When a credential is rejected

The upstream can revoke a login at any time, and it is the one failure that
does not fix itself: waiting does nothing, and rotating only spends the next
account. agentswap tells you in the place you are actually looking.

**In your agent**, which is where most people are when it happens:

```
your anthropic account "work" was rejected (refresh failed with 401
Unauthorized). Sign in again with `agentswap login --id work`.
```

**In `agentswap status`**:

```
work was rejected: refresh failed with 401 Unauthorized
  sign in again:  agentswap login --id work
```

Then sign in as that account and run the command it gave you. The credential is
replaced in place, keeping the id, priority and label, and the account goes
straight back into rotation.

Note that `agentswap import` is *not* the fix: it re-reads the credential the
upstream just refused, so it looks like the fix did nothing.

### Why a login gets rejected

Usually one of:

- **The CLI renewed its own session.** This is the common one, and it is worth
  understanding — see below.
- **You signed out**, in that CLI or on the web.
- **The upstream expired the session**, which it may do whenever it likes.

## An imported credential has a shelf life

`agentswap import` copies the credential your CLI is *currently* using. You now
have two holders of one credential, and OAuth refresh tokens rotate: renewing
returns a new refresh token and retires the old one. Whichever side renews
first keeps working, and the other is holding something the server has thrown
away.

Measured on Claude Code: the access token lasts **eight hours**, and the CLI
renews it lazily — when it is expired or nearly so, not on every invocation. So
an imported copy is good until roughly the next eight-hour boundary, and then
one side loses. When it is agentswap that loses, you get the rejected-credential
message above; your CLI is unaffected.

Note that this only affects the account your CLI is *currently signed in as*.
Every other account in the pool is agentswap's alone — nothing else renews
them, so they keep working indefinitely.

### The way out: a long-lived token

A long-lived token is a separate credential. Your CLI never renews it, because
it is not your CLI's session, so there is nothing to race:

```sh
claude setup-token                 # issue one (needs a Claude subscription)
agentswap add-token anthropic      # paste it here; --token - reads a pipe
```

It is pooled as a subscription, so it is still spent before any metered key,
and it shows in `list` as `long-lived`. This is the right way to pool the
account you use day to day; `import` is the quick way to get started.

For the openai lane the equivalent is an ordinary API key, which does not
rotate either.

## API keys

Keys are tried after every subscription is spent, because subscriptions are
already paid for.

```sh
agentswap add-key anthropic                                  # prompts, echo off
echo "$ANTHROPIC_API_KEY" | agentswap add-key anthropic --key -
AGENTSWAP_API_KEY=sk-ant-... agentswap add-key anthropic
```

Avoid `--key sk-ant-...` on the command line: it lands in your shell history
and is visible in the process list while it runs. The three forms above exist
so you never have to.

A third-party provider that speaks the same protocol needs a `--base-url`, and
is otherwise an ordinary member of the pool. `agentswap import` carries the
active Claude/Codex override over automatically; use `add-key` for another one:

```sh
agentswap add-key anthropic --key - --id gateway \
  --base-url https://llm.corp.example.com
```

That key is sent to the host you name, which is the point of the feature and
your judgement to make.

To replace a key, `agentswap set NAME --key -`. To order several, `--priority`
(lower is preferred). The same key is never added twice.

If `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` is set in your environment,
`agentswap import` mentions it and shows the command to adopt it. It will not
adopt it for you: pooling a credential you never named is a surprise, and this
one holds your money.

## Base URLs

Every account can talk to a different upstream. That is how a third-party
provider joins the pool, and it works for subscriptions as well as keys — a
gateway in front of your own account is an ordinary member.

```sh
agentswap add-key anthropic --key - --base-url https://llm.corp.example.com
agentswap set corp --base-url https://other.example.com   # move it
agentswap set corp --base-url ""                             # back to the vendor
```

The lane decides the *protocol*, not the vendor: everything in the `anthropic`
lane speaks the Messages API, everything in `openai` speaks the Responses API.
A provider that speaks one of those is a base URL away from being poolable. One
that does not is not supported, because translating between protocols would
mean owning a mapping that breaks whenever either vendor ships a feature.

Defaults, when no base URL is set:

| Lane | Kind | Upstream |
| --- | --- | --- |
| anthropic | either | `https://api.anthropic.com` |
| openai | subscription | `https://chatgpt.com/backend-api/codex` |
| openai | api key | `https://api.openai.com/v1` |

## Settings, as opposed to credentials

```sh
agentswap config
```

shows the config directory (and which environment variable put it there), each
file and whether it exists, whether the daemon is running, the CLIs' own files
that `agentswap install` edits, and every setting currently in effect.

Because every setting has a default, an absent `config.json` tells you nothing
about the values behind it. To start editing from a complete file:

```sh
agentswap config --write     # writes config.json with the effective values
```

Do not redirect `--json` into that path yourself: the shell truncates the file
before agentswap reads it. (An empty `config.json` is treated as absent for
exactly this reason, but `--write` is the command that works.)

## Taking an account out without deleting it

```sh
agentswap disable work     # keeps the credential, stops using it
agentswap enable work
agentswap remove work      # forgets it entirely
```

`disable` is the right one for "this account is for something else this week".
