class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.3.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.0/agentswap_v0.3.0_darwin_arm64.tar.gz"
      sha256 "ae5b71038ceabcab4f31d8773d6578dc9147e40249b65cac737a1310b42f0274"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.0/agentswap_v0.3.0_darwin_amd64.tar.gz"
      sha256 "719c193d0499aeb93bbd1aca5909758cade762702ce8587f87b7959fba114905"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.0/agentswap_v0.3.0_linux_arm64.tar.gz"
      sha256 "06083f547e91948ebc5dcb2b507612d399086d08fe5c14b70f7075169b3a38fd"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.0/agentswap_v0.3.0_linux_amd64.tar.gz"
      sha256 "b38ed0a7dd72f1d99f2a4fd865f7f8a8ef3d05d745c460dcd1f16be8031a62a4"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
