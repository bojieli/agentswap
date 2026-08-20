#!/bin/sh
# Generate the Homebrew formula shipped as a GitHub release asset.
#
# Usage: generate-homebrew-formula.sh TAG [DIST_DIR] [OUTPUT]
#
# The formula installs the already-built release archive rather than compiling
# Go on the user's machine. This keeps `brew install` fast and gives it the
# same checksum-verified artifact as the curl installer.

set -eu

tag=${1:?usage: generate-homebrew-formula.sh TAG [DIST_DIR] [OUTPUT]}
dist=${2:-dist}
output=${3:-${dist}/agentswap.rb}

case "$tag" in
	v*) ;;
	*)
		echo "formula: tag must start with v: $tag" >&2
		exit 1
		;;
esac

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	echo "formula: sha256sum or shasum is required" >&2
	exit 1
fi

archive_sha() {
	archive=$1
	file="$dist/$archive"
	if [ ! -f "$file" ]; then
		echo "formula: missing release archive $file" >&2
		exit 1
	fi
	sha256 "$file"
}

version=${tag#v}
base="https://github.com/bojieli/agentswap/releases/download/${tag}"
mac_arm="agentswap_${tag}_darwin_arm64.tar.gz"
mac_intel="agentswap_${tag}_darwin_amd64.tar.gz"
linux_arm="agentswap_${tag}_linux_arm64.tar.gz"
linux_intel="agentswap_${tag}_linux_amd64.tar.gz"

mkdir -p "$(dirname "$output")"
cat >"$output" <<EOF
class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "${version}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "${base}/${mac_arm}"
      sha256 "$(archive_sha "$mac_arm")"
    else
      url "${base}/${mac_intel}"
      sha256 "$(archive_sha "$mac_intel")"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "${base}/${linux_arm}"
      sha256 "$(archive_sha "$linux_arm")"
    else
      url "${base}/${linux_intel}"
      sha256 "$(archive_sha "$linux_intel")"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
EOF

chmod 0644 "$output"
