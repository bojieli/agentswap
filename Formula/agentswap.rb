class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.6.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.6.0/agentswap_v0.6.0_darwin_arm64.tar.gz"
      sha256 "3c0c9263e8f22a17086d9db1632c27d1ae8d7a144641f1b729ae45941c4fa0d0"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.6.0/agentswap_v0.6.0_darwin_amd64.tar.gz"
      sha256 "ea5b66b55172932b660f7de419a81f9d07f79f41b3a457ecc112d0a07f3eb3e5"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.6.0/agentswap_v0.6.0_linux_arm64.tar.gz"
      sha256 "4ea78b635529658001f5afe64075fb285a29c6b23e50d91aa491cebb68f52727"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.6.0/agentswap_v0.6.0_linux_amd64.tar.gz"
      sha256 "21186fcad63932f96fba9260a41709cb1d204b4855e609f699f148246e84e84d"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
