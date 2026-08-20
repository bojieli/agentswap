#!/bin/sh
# Offline smoke test for the generated Homebrew formula.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	archive="agentswap_v0.0.0-test_${target}.tar.gz"
	printf 'fixture for %s\n' "$target" >"$tmp/$archive"
done

"$root/scripts/generate-homebrew-formula.sh" v0.0.0-test "$tmp" "$tmp/agentswap.rb"

for expected in \
	'agentswap_v0.0.0-test_darwin_arm64.tar.gz' \
	'agentswap_v0.0.0-test_darwin_amd64.tar.gz' \
	'agentswap_v0.0.0-test_linux_arm64.tar.gz' \
	'agentswap_v0.0.0-test_linux_amd64.tar.gz'; do
	grep -F "$expected" "$tmp/agentswap.rb" >/dev/null || {
		echo "formula is missing $expected" >&2
		exit 1
	}
done

grep -F 'class Agentswap < Formula' "$tmp/agentswap.rb" >/dev/null
grep -F 'sha256 "' "$tmp/agentswap.rb" >/dev/null
echo "release formula smoke test passed"
