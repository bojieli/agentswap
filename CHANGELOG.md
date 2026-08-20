# Changelog

Notable changes, newest first. This project follows
[semantic versioning](https://semver.org/spec/v2.0.0.html); until 1.0 the minor
version moves for anything that changes behaviour.

## Unreleased

### Fixed

- **Claude sessions could fail when a later event changed directories.** A
  transcript may legitimately record a process CWD outside its project root;
  event-level CWD metadata is now tolerated during teleport instead of being
  treated as a corrupt session.

Found by creating native sessions and completing real model/tool turns after
every directed teleport among Claude Code, Codex, OpenCode, and Kimi Code:

- **Current Kimi targets loaded but could not continue.** Imported history has
  no provider-specific bound agent profile, and Kimi deliberately restores a
  session's binding instead of applying the global default. The exact resume
  command now includes the configured target model so Kimi performs its own
  native profile bind; `AGENTSWAP_KIMI_MODEL` can override it.
- **Claude targets disappeared from the cwd resume picker when a path contained
  punctuation.** Exact-id resume happened to scan beyond the project index,
  hiding that agentswap preserved dots and underscores in the encoded project
  key while Claude replaces every non-alphanumeric character. Target placement
  now matches Claude's UTF-16 path encoding.
- **Claude's `--max-budget-usd` metadata stopped teleport entirely.** Claude
  2.1.235 records a `budget_usd` attachment; it is runtime accounting rather
  than conversation content and is now safely ignored.

Found by reading the rotation path against the states a pool actually reaches,
and reproduced as a failing test before each was touched:

- **A reload arriving during a token refresh put the old credential back.**
  Everything that changes the pool tells the daemon to reload, and the daemon
  renews a token whenever one expires, so `agentswap login` during a busy
  session was enough to overlap them. The reload read `accounts.json` while the
  refresh was still writing it, then adopted what it read — leaving the daemon
  holding a refresh token the upstream had already retired the moment it issued
  the replacement. The next renewal presented it, was refused, and a healthy
  account was recorded as rejected. This was not a narrow window: the reload's
  read beat the refresh's fsync on the first attempt, every time. A change to
  the pool now moves memory and file together.
- **One account's 401 spent the refresh budget the next account needed.**
  `auth_refresh_attempts` was counted per request rather than per account, so
  once the first account had renewed its token, a second account answering 401
  was marked *rejected* without its own refresh ever being attempted — the one
  verdict that does not heal on its own, and the one that sends the user to
  sign in again for nothing. An imported Codex subscription is stored without
  an expiry, so nothing knows its token is stale until the 401 arrives: the
  accounts most likely to need the budget were the ones being denied it.
- **A sub-second Codex reset was treated as no reset at all.** The upstream's
  `resets_in_seconds` was truncated to whole seconds before becoming a
  duration, so anything under a second became zero — indistinguishable from the
  upstream saying nothing about timing, whose answer is to write the account
  off for five hours. A 0.4-second burst limit cost a five-hour rotation.
  Fractional figures are now kept, in the plan-exhausted path too.

Found by driving the real `claude` and `codex` against real upstreams:

- **A refused API key was sent for a token refresh**, which cannot apply to a
  key. The attempt failed with "not an oauth account", and *that* was recorded
  as the reason and shown to the user — throwing away the upstream's actual
  explanation and recommending a sign-in that does not exist for an API key.
  The remedy now matches the credential, and the upstream's own words survive.
- **A credential fixed from the CLI had no effect until the daemon restarted.**
  The daemon reads the pool once, at startup, so replacing a rejected key or
  signing in again — the fix that `status` and the client's own error both
  recommend — changed nothing, with nothing anywhere saying why. Commands that
  change the pool now tell a running daemon, which reloads and stops holding a
  verdict about a credential that has been replaced.
- **An upstream's error could carry the credential into agentswap's own
  records.** Gateways do echo the key back ("Incorrect API key provided:
  sk-…"), and that text was stored in state.json, logged, and returned to the
  client. The account's own credential is now removed from any message before
  it is kept or shown.
- **A redirect from the upstream was followed, with the credential attached.**
  Go strips `Authorization` across domains but knows nothing about `X-Api-Key`,
  so a 3xx could hand a key to whatever host it named — and Go rewrites POST as
  GET on a 302, which would have turned a message into a silent no-op.
  Redirects are handed back to the client, which is what a proxy should do with
  them.
- **A hand-edited `accounts.json` could say two things at once.** Two accounts
  sharing an id silently halved the pool, because every command and the health
  record address an account by id and only ever reached the first. That is now
  refused. An account with no credential, or an unknown `kind`, is reported by
  `list` and `doctor` and skipped when routing — reported rather than fatal,
  since refusing to load the pool would take away the commands needed to fix it.
- **An anthropic base URL ending in `/v1` now warns.** Claude Code sends
  `/v1/messages`, so the provider saw `/v1/v1/messages` and 404'd everything,
  with nothing in the failure to suggest why. The documentation recommended
  exactly that URL.
- **An empty `config.json` stopped every command.** Zero bytes is what a
  redirect leaves behind, and refusing to start over it stranded the user until
  they worked out that deleting the file was the cure. An empty file now says
  the same thing as no file.
- **The same login could be pooled twice.** Running `import` again without
  switching accounts added a second entry holding the same credential: `status`
  showed two accounts, and both were refused in the same instant. A login is
  now recognised and updated in place, and identity survives a token refresh.
  The same applies to adding one API key twice.
- **Concurrent writes to the same file could fail on Windows.** The data lock
  is released before writing so readers are never blocked on disk I/O, which
  means two goroutines can reach the write at once; on Windows a replace of a
  path another handle has open fails with a sharing violation, so a busy daemon
  would intermittently fail to persist health or a token it had just refreshed.
  Writes are serialized now, and the rename retries briefly for what a lock
  cannot cover.
- **Health was lost on every clean shutdown.** The final flush runs in the
  flusher goroutine while the main path returned as soon as the server did, so
  the process could exit between writing the temp file and renaming it. The
  next start re-probed an account already known to be spent, burning a request
  to rediscover the limit, and left the temp file behind for good. A stale temp
  file from a hard kill is now swept at startup.
- **A quota window that refills in seconds was waited out for a full minute.**
  The livelock guard exists for reset times that have already passed, but it
  was applied as a floor under every reset. It is a fallback now, not a floor.
  The clock-skew allowance had the same shape and is capped at the length of
  the wait, since being early is self-correcting and being late is pure loss.
- **Beta flags the client asked for were silently dropped.** The
  `anthropic-beta` header is legal repeated as well as comma-joined; reading
  only the first line and overwriting the rest discarded whichever features
  were requested in the others.
- **A hand-written `accounts.json` entry was silently inert.** The file is
  documented as hand-editable, but JSON's zero value for a bool meant an entry
  without `"enabled": true` never entered rotation, showing as disabled in
  `list` while `doctor` reported the lane as empty. An absent key now means
  enabled.
- **`serve --addr 127.0.0.1:0`** bound a random port and published the literal
  `:0`, which is useless to every other command.
- **`doctor` told anyone whose accounts were all disabled to run `import`**,
  which would not have helped.
- **Concurrent token refresh could retire an account.** Both upstreams rotate
  the refresh token when it is used, so several in-flight requests noticing the
  same expired token all posted it — and every exchange after the first
  presented a credential the server had already retired, taking the account out
  of the pool. Refreshes now coalesce onto one exchange. This was reachable
  whenever an agent had more than one request in flight at an expiry boundary,
  which is most of them.
- **A data race on every account.** The store handed out pointers into the
  pool, so a token refresh wrote `AccessToken` while other requests read it. It
  now hands out clones, and offers `UpdateAccount` for the read-modify-write
  that refresh needs — which also stops a renewed token from reverting a
  concurrent `agentswap disable`.
- **`agentswap import --id work` named the account `anthropic-work`.** The
  lane prefix was applied unconditionally instead of only when one import turns
  up two credentials.
- **`agentswap run -- codex exec ...` resumed into an interactive session.**
  Exec sessions have their own resume subcommand, so the interactive form found
  nothing to resume — and since a supervised run is unattended, it looked like
  a hang. Claude Code's `-p` is preserved for the same reason.
- **Resuming dropped the Codex profile.** The resume rewrote the user's
  original argv rather than the args actually run, losing the injected
  `--profile agentswap` — so from the second attempt on, Codex silently
  bypassed the proxy.
- **`agentswap status` and `doctor` could not find a daemon started with
  `serve --addr`.** They read the address from the config file and reported a
  healthy daemon as down. The daemon now publishes where it is listening.
- **`doctor` failed on lanes and CLIs you do not use.** A healthy install of
  only Claude Code reported two failures and exited non-zero. Unused lanes and
  CLIs are now notes.
- **A CLI pointed at the wrong address was told to run `install`**, which would
  have rewritten the same value. It now says what the mismatch is.
- **The client timeout `install` writes was a fixed two hours** while
  `park.max_hold` is configurable, so raising `max_hold` past it meant the CLI
  gave up before the daemon did. It is now derived from `max_hold`.
- **`CLAUDE_CREDENTIALS_PATH` pointing at a missing file fell back to the macOS
  Keychain**, importing whichever account happened to be logged in there.
- **`CLAUDE_CONFIG_DIR` was ignored.** Claude Code honours it, so `install`
  wrote to `~/.claude` while the CLI read somewhere else — reporting success
  having configured nothing, with `doctor` then insisting the CLI was not wired
  up however many times you ran it.
- A client that hung up mid-upload was reported as a 413; one that hung up
  mid-wait was logged at error level and answered with a 502 nobody would read.

### Added

- **`agentswap teleport <target>`** moves a structurally preserved, resumable
  session among Claude Code, Codex, OpenCode, and current or legacy Kimi Code.
  It discovers by exact current directory, prompts on ambiguity, accepts an
  exact source id, validates before writing, keeps source sessions untouched,
  reports fidelity limits, supports dry-run and exact-id launch, and rolls back
  incomplete target artifacts. Messages, tool call ids/inputs/results/errors,
  recorded reasoning, plans, timestamps and model metadata cross through an
  ordered canonical event stream rather than a summary prompt. OpenCode uses
  its native import/export CLI instead of direct database writes.

- **`agentswap add-token`**, for a long-lived token — the way out of an
  imported credential going stale. `import` copies the credential your CLI is
  currently using, and OAuth refresh tokens rotate, so whichever of you renews
  first retires the other's copy. Measured on Claude Code, the access token
  lasts eight hours and the CLI renews it lazily, which puts a ceiling on how
  long an imported copy survives. A token from `claude setup-token` is nobody's
  session, so there is nothing to race. It is pooled as a subscription and
  spent before any metered key.
- **`agentswap service`**, which runs the daemon in the background and starts it
  again at login — a LaunchAgent on macOS, a systemd user unit on Linux, and on
  Windows the Task Scheduler and Startup-folder instructions. Everything
  agentswap does assumes the daemon is up, and leaving that to a terminal the
  user must not close was the difference between a tool that works and one that
  works until they reboot. Per-user, never system-wide: the daemon holds one
  person's credentials.
- **`agentswap set`**, which changes an account already in the pool: its
  upstream, priority, label or key. Previously an account's base URL could only
  be set when it was added — and never at all for a subscription — so moving a
  key to a different gateway or reordering the pool meant hand-editing the file
  that the daemon rewrites underneath you.
- **`agentswap config`**, which shows where everything lives and every setting
  in effect. Because every setting has a default, an absent config.json said
  nothing about the values behind it. `--write` saves them as a file to edit.
- **`agentswap login`**, which pools an account or replaces a rejected
  credential. It works out which CLI you mean rather than asking — an unpooled
  credential sitting there is someone who just signed in — and only asks when
  both are equally plausible. agentswap has no OAuth flow of its own, so this guides the
  sign-in and adopts the result — working out whether you need to sign in at
  all, waiting for the credential to appear however you did it, and keeping the
  account's id, label and priority when replacing.
- **Rejected credentials now say what to do about them**, in the error your
  agent shows and in `agentswap status`: which account, why, and the command
  that fixes it. Previously this arrived as "no accounts configured for this
  lane", which was both wrong and pointed at `import` — which re-reads the
  credential the upstream just refused.
- **Keys can be given without putting them in shell history**: `--key -` reads
  a pipe, and a bare `agentswap add-key anthropic` prompts with the echo off.
  `agentswap import` mentions API keys sitting in your environment without
  adopting them.
- **`agentswap list` shows which credential each row holds** — masked key,
  plan, and the host for a third-party provider — so several keys can be told
  apart.
- **Host header checking**, which closes DNS rebinding: a page you visit can
  point its own domain at 127.0.0.1 and spend your subscription, but it cannot
  forge the `Host` header. Loopback names and the configured address are
  accepted; `allowed_hosts` covers reaching agentswap deliberately by another
  name.
- A startup warning when `addr` is not loopback.
- `--addr` on `status` and `doctor`.
- Release binaries for seven platforms with published checksums, an install
  script that verifies them, and `docs/` covering configuration, architecture
  and troubleshooting.
- An end-to-end suite that compiles the binary and drives it as a subprocess —
  argv, exit codes, files on disk and HTTP — covering the CLI, the proxy,
  install and uninstall, doctor and the supervisor. Coverage from it is merged
  with the unit tests, which is the only way the CLI layer's real coverage
  shows at all.
- `govulncheck` in CI, which is the security scan that can run on a private
  repository.

### Changed

- The config directory is tightened to 0700 if it already existed with looser
  permissions. `MkdirAll` only applies its mode when it creates.
- The CLIs' own config directories are created 0700 rather than 0755 when
  `install` has to create them. They hold the CLIs' credentials.
