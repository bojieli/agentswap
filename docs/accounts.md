# Managing accounts and keys

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
what it stored. `agentswap login` removes every part of that except the signing
in.

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

- **You used the account outside agentswap.** Both upstreams rotate the refresh
  token when it is used, so whichever side refreshes second is holding a token
  the server has retired. Running your CLIs through agentswap avoids this;
  running one of them directly, occasionally, will cost you a re-login.
- **You signed out**, in that CLI or on the web.
- **The upstream expired the session**, which it may do whenever it likes.

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
is otherwise an ordinary member of the pool:

```sh
agentswap add-key anthropic --key - --id gateway \
  --base-url https://llm.corp.example.com/v1
```

That key is sent to the host you name, which is the point of the feature and
your judgement to make.

To replace a key, add it again under the same `--id`. To order several, use
`--priority` (lower is preferred). The same key is never added twice.

If `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` is set in your environment,
`agentswap import` mentions it and shows the command to adopt it. It will not
adopt it for you: pooling a credential you never named is a surprise, and this
one holds your money.

## Taking an account out without deleting it

```sh
agentswap disable work     # keeps the credential, stops using it
agentswap enable work
agentswap remove work      # forgets it entirely
```

`disable` is the right one for "this account is for something else this week".
