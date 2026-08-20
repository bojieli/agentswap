# Security and privacy

agentswap holds live OAuth tokens for your Anthropic and OpenAI accounts. That
shapes everything below.

Read this before exposing the daemon beyond loopback, adding a third-party
base URL, or sharing a teleported session. For normal setup, start with the
[README](README.md); for the transfer-specific data model, see
[docs/sessions.md](docs/sessions.md).

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

**CodeQL** runs on every push and weekly, with the `security-and-quality`
query set. It is skipped while the repository is private, because uploading
results needs code scanning. It turns on when the repository is published.

**No third-party dependencies.** `go.sum` is empty and CI fails if it stops
being. A dependency in this process is code with access to your tokens,
published by someone who could change it tomorrow.

**Loopback only.** The default listen address is `127.0.0.1:8420`.
Binding anywhere else warns loudly, because anything that can reach the port
can spend your subscriptions.

**Host header checking.** A web page can point its own domain at `127.0.0.1`
and send requests to a local server — DNS rebinding. It cannot forge the
`Host` header, so agentswap refuses names it does not recognise.

Loopback names and the configured address are accepted. Add another deliberate
entry to `allowed_hosts` when reaching the proxy through a container or tunnel.

**Credentials are replaced, never forwarded.** Whatever token the client sends
is discarded and one from the pool is substituted. A token cannot leak from one
configured CLI into another lane, and the placeholder in Claude Code settings
is not a secret.

**Files are 0600 via atomic replace.** `accounts.json` and `state.json` are
written to a temporary file in the same directory, fsynced, and renamed. An
interrupted write cannot leave a half-file. The config directory is 0700, and
an existing directory with looser permissions is tightened on open.

**Credentials are never logged.** Accounts appear through `Account.Display()`,
which is an id or human label — never a token. Upstream error bodies are read
into memory for classification, capped at 64 KiB, and never logged.

**A teleported session is sensitive local data.** It can contain prompts,
proprietary source fragments, shell output, tool inputs, and error messages.
File-backed target sessions are created in the target CLI's private session
tree with 0600 files and staged before publication. The source is never opened
for writing. Temporary OpenCode import payloads are 0600 and removed after the
native importer returns.

**OpenCode owns its database.** agentswap does not link a SQLite driver or
write OpenCode tables. It executes the configured local `opencode` binary for
session list/export/import, passes no agentswap credentials, checks the exact
new session id in the import confirmation, and attempts to remove only that id
if import is not confirmed. `AGENTSWAP_OPENCODE_BIN` is executable-code trust:
setting it to an untrusted program gives that program the exported conversation
and your normal process environment.

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
- Teleportation cannot redact a conversation without changing its semantics.
  The target contains the same recorded sensitive content as the source. Its
  native CLI and any plugins loaded on resume receive that history.
- Hidden model state, credentials, approvals, running tools and provider-bound
  encrypted reasoning are not copied. Treating their absence as a security
  boundary would be unsafe; the resumed target is a new process with its own
  configuration and permissions.
- Text-file attachments and edited-file context become visible conversation
  text in targets that have no attachment representation. Binary media and
  branched subagent transcripts are rejected rather than silently omitted.

## Not a vulnerability

- Using several subscriptions to increase throughput may violate a provider's
  terms of service. That is a policy question, covered in the README, not a
  security issue.
- Reaching the proxy from another process running as you. See above.
