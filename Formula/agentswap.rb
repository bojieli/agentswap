class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.7.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.0/agentswap_v0.7.0_darwin_arm64.tar.gz"
      sha256 "22ac9050e677f7fc9bd3d8bb9b3f30b4df95cb049dfaad6c29e1fff824f9048c"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.0/agentswap_v0.7.0_darwin_amd64.tar.gz"
      sha256 "a01aa1fe89ee0855e1ce7f91d8c5ace77ed994c6170ad2cc85693b415d9e3902"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.0/agentswap_v0.7.0_linux_arm64.tar.gz"
      sha256 "4f726d117d29c9365a75ffe08030ad42b878b4f83538fae285d520b02759905c"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.0/agentswap_v0.7.0_linux_amd64.tar.gz"
      sha256 "8f3621803bf361b898da709980d43ce9708f4c8814f045272b9bb53f79c654f5"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
