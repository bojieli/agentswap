#!/bin/sh
# Install agentswap from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/bojieli/agentswap/main/install.sh | sh
#
# Piping a script from the internet into a shell is a thing you should think
# about rather than do reflexively. The alternatives, both of which build from
# source you can read:
#
#   go install github.com/bojieli/agentswap/cmd/agentswap@latest
#   git clone https://github.com/bojieli/agentswap && cd agentswap && make install
#   brew install --formula https://github.com/bojieli/agentswap/releases/latest/download/agentswap.rb
#
# This script verifies the SHA-256 checksum of what it downloads against the
# published SHA256SUMS, and refuses to install if it cannot.

set -eu

REPO="bojieli/agentswap"
BIN="agentswap"
INSTALL_DIR="${AGENTSWAP_INSTALL_DIR:-}"
VERSION="${AGENTSWAP_VERSION:-latest}"

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*" >&2; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

need uname
need tar

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
	fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	fetch_stdout() { wget -qO- "$1"; }
else
	die "curl or wget is required"
fi

# --- what are we running on -------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux|darwin|freebsd) ;;
	*) die "unsupported OS '$os'. Try: go install github.com/$REPO/cmd/$BIN@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64|amd64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) die "unsupported architecture '$arch'. Try: go install github.com/$REPO/cmd/$BIN@latest" ;;
esac

# --- where does it go -------------------------------------------------------

if [ -z "$INSTALL_DIR" ]; then
	# Prefer somewhere already on PATH that we can write without sudo.
	for candidate in "$HOME/.local/bin" "/usr/local/bin" "$HOME/bin"; do
		if [ -d "$candidate" ] && [ -w "$candidate" ]; then
			INSTALL_DIR="$candidate"
			break
		fi
	done
fi
[ -n "$INSTALL_DIR" ] || INSTALL_DIR="$HOME/.local/bin"
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable. Set AGENTSWAP_INSTALL_DIR to somewhere you own."

# --- which version ----------------------------------------------------------

if [ "$VERSION" = "latest" ]; then
	info "Finding the latest release..."
	VERSION=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
	[ -n "$VERSION" ] || die "could not determine the latest version. Set AGENTSWAP_VERSION to a tag."
fi

archive="${BIN}_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "Downloading $BIN $VERSION for $os/$arch..."
fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"

# --- verify -----------------------------------------------------------------
#
# A binary that is about to hold your OAuth tokens is worth one checksum.

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "no sha256sum or shasum available to verify the download"
fi

fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "could not download SHA256SUMS"
expected=$(grep -F "$archive" "$tmp/SHA256SUMS" | cut -d' ' -f1 | head -1)
[ -n "$expected" ] || die "$archive is not listed in SHA256SUMS"

actual=$(sha256 "$tmp/$archive")
if [ "$expected" != "$actual" ]; then
	die "checksum mismatch for $archive
  expected $expected
  got      $actual
Refusing to install."
fi
info "Checksum verified."

# --- install ----------------------------------------------------------------

tar -xzf "$tmp/$archive" -C "$tmp"
extracted=$(find "$tmp" -type f -name "$BIN" -perm -u+x | head -1)
[ -n "$extracted" ] || die "the archive did not contain a $BIN binary"

# Replace via a temp file in the destination so an in-use binary is swapped
# rather than truncated.
staged="$INSTALL_DIR/.$BIN.new.$$"
cp "$extracted" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$INSTALL_DIR/$BIN"

info ""
info "Installed $BIN $VERSION to $INSTALL_DIR/$BIN"

case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		info ""
		info "$INSTALL_DIR is not on your PATH. Add this to your shell profile:"
		info "    export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
esac

info ""
info "Next:"
info "    $BIN import      # adopt the logins already on this machine"
info "    $BIN install     # point Claude Code and Codex at agentswap"
info "    $BIN serve       # run the daemon"
