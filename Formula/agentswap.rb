class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.3.1"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.1/agentswap_v0.3.1_darwin_arm64.tar.gz"
      sha256 "997f79dd4b70f3b1e9483d985f83ec063f547eb5faf24fc19cf04681df113ee0"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.1/agentswap_v0.3.1_darwin_amd64.tar.gz"
      sha256 "c1d3946f6d292feed5d752ea366bdf5fbc2417652df90c110764969879effa4b"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.1/agentswap_v0.3.1_linux_arm64.tar.gz"
      sha256 "40554551a12a8b41c0f800c084a46f2344892fb0b5e8d39ba0f2a3baec49b10a"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.3.1/agentswap_v0.3.1_linux_amd64.tar.gz"
      sha256 "202b8628b1684ba176169c5da1dab86965753678db1beaee608d16e37ed3b34a"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
