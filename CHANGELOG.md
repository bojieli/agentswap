# Changelog

Notable changes, newest first. This project follows
[semantic versioning](https://semver.org/spec/v2.0.0.html); until 1.0 the minor
version moves for anything that changes behaviour.

## Unreleased

### Fixed

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
- A client that hung up mid-upload was reported as a 413; one that hung up
  mid-wait was logged at error level and answered with a 502 nobody would read.

### Added

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

### Changed

- The config directory is tightened to 0700 if it already existed with looser
  permissions. `MkdirAll` only applies its mode when it creates.
- The CLIs' own config directories are created 0700 rather than 0755 when
  `install` has to create them. They hold the CLIs' credentials.
