# Security

agentswap holds live OAuth tokens for your Anthropic and OpenAI accounts. That
shapes everything below.

## Reporting a vulnerability

Report privately through
[GitHub's advisory form](https://github.com/bojieli/agentswap/security/advisories/new).
Please do not open a public issue for anything that could expose a credential.

Include what you did, what happened, and what an attacker gets out of it. A
proof of concept helps but is not required. You should get a first response
within a week; if the report is confirmed, you will be credited in the advisory
unless you would rather not be.

Supported: the latest release. Fixes are not backported.

## The design, and what it is defending

**No third-party dependencies.** `go.sum` is empty and CI fails if it stops
being. A dependency in this process is code with access to your tokens,
published by someone who could change it tomorrow.

**Loopback only.** The default listen address is `127.0.0.1:8420`. Binding
anywhere else warns loudly at startup, because anything that can reach the port
can spend your subscriptions.

**Host header checking.** A web page you visit can point its own domain at
127.0.0.1 and send requests to a local server — DNS rebinding. What it cannot
do is forge the `Host` header, so agentswap refuses requests naming a host it
does not recognise. Loopback names and the configured address are accepted;
add anything else to `allowed_hosts`.

**Credentials are replaced, never forwarded.** Whatever token the client sends
is discarded and one from the pool is substituted. A token cannot leak from one
configured CLI into another lane, and the placeholder in your Claude Code
settings is not a secret.

**Files are 0600 via atomic replace.** `accounts.json` and `state.json` are
written to a temp file in the same directory, fsynced, then renamed, so an
interrupted write cannot leave a half-file. The config directory is 0700, and
an existing directory with looser permissions is tightened on open.

**Credentials are never logged.** Accounts appear in logs through
`Account.Display()`, which is an id or a human label — never a token. Error
bodies from upstream are read into memory to classify them, capped at 64 KiB,
and are not logged.

## Known limits

- Anyone who can already run code as your user can read `accounts.json`. This
  is the same exposure as the credential files the CLIs write themselves, and
  agentswap does not try to improve on it. There is no keyring integration for
  the pool.
- `/_agentswap/status` is unauthenticated. It exposes account ids, labels and
  quota — no tokens — to anything on loopback that passes the Host check.
- The proxy does not inspect or sanitise request bodies. Whatever your CLI
  sends is what the upstream receives.
- A per-account `base_url` sends that account's key to the host you name. That
  is the point of the feature, and it is your responsibility to name a host you
  trust.

## Not a vulnerability

- Using several subscriptions to increase throughput may violate a provider's
  terms of service. That is a policy question, covered in the README, not a
  security issue.
- Reaching the proxy from another process running as you. See above.
