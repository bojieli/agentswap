# Releases and Homebrew publishing

This page is for maintainers. Users only need the Homebrew command in the
[README](../README.md#install) after a public release exists.

The repository already has broad CI, and the release workflow now covers the
same supply-chain path users rely on:

- `ci.yml` tests Linux, macOS, and Windows with the minimum and stable Go
  toolchains; runs race-enabled shuffled tests, vet, formatting, dependency
  policy, vulnerability scanning, linting, a 75% merged coverage floor, and all
  release-target cross-builds; and smoke-tests the shell scripts and formula.
- `release.yml` runs for `v*` tags or a manually selected existing tag. It tests
  the tag, builds archives for Linux, macOS, Windows, and FreeBSD, publishes
  `SHA256SUMS`, generates a Homebrew formula, and attaches build-provenance
  attestations.

## Install with Homebrew

The repository doubles as a Homebrew tap. The explicit URL works even though
the source repository is named `agentswap` rather than the conventional
`homebrew-agentswap`:

```sh
brew tap bojieli/agentswap https://github.com/bojieli/agentswap.git
brew install bojieli/agentswap/agentswap
```

Homebrew chooses Apple Silicon versus Intel (and Linux arm64 versus amd64)
automatically. The formula installs the release binary only. Account import,
CLI wiring, and the daemon remain explicit:

```sh
agentswap import
agentswap install
agentswap service install
```

The formula is committed at [`Formula/agentswap.rb`](../Formula/agentswap.rb)
and is also generated as a release asset. Its version and four platform
archive checksums must change together for every tag; CI and the release
workflow verify the generated file before publishing.

GitHub does not serve private release assets anonymously. If this repository is
private, users need repository access and should download the formula and
archive with authenticated GitHub tooling. The tap is intended for this public
repository and its public releases.

## Create a release

Update `CHANGELOG.md`, commit it, and push an annotated semantic-version tag:

```sh
git tag -a v0.2.0 -m 'agentswap v0.2.0'
git push origin v0.2.0
```

The workflow can be rerun for the same tag. It updates the GitHub release and
uploads assets with `--clobber`; no generated release files are committed to
the source branch. Verify downloaded archives with:

```sh
sha256sum -c SHA256SUMS
```

Public repositories also receive a GitHub build-provenance attestation for all
release assets. GitHub skips that optional step for private repositories where
artifact attestations are unavailable.
