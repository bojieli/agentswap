class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.4.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.4.0/agentswap_v0.4.0_darwin_arm64.tar.gz"
      sha256 "f9d1c403ec5841877abaac20bbd7403e40ccd47be17d67c404a36a224f0b7c78"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.4.0/agentswap_v0.4.0_darwin_amd64.tar.gz"
      sha256 "65f73b83808effc5628661f6e263fb84b9e72f7355f000624b3f1b6983ef58bf"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.4.0/agentswap_v0.4.0_linux_arm64.tar.gz"
      sha256 "801cf72eba68bc236c5f9e18b96b51f184fed241a1abbf74c39f1d3d4152536b"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.4.0/agentswap_v0.4.0_linux_amd64.tar.gz"
      sha256 "2d4accf4b70bf6662ff59395bf229c08a169621be73a8cd19b32ffaa9c9b6198"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
