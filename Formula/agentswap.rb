class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.7.1"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.1/agentswap_v0.7.1_darwin_arm64.tar.gz"
      sha256 "b94208d7209d2a3f2070ee22fd19d0e42a1752dce6e5dbf6f4ac2840633dd87f"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.1/agentswap_v0.7.1_darwin_amd64.tar.gz"
      sha256 "7741a6eee54c035544ae38951f935b836a80ddfd2486a3a56d14bda45a0ddb9e"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.1/agentswap_v0.7.1_linux_arm64.tar.gz"
      sha256 "b9c991ac6d1af9a3f08ce8d28662551027479530a878858228f2e8e8ffa4bc18"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.7.1/agentswap_v0.7.1_linux_amd64.tar.gz"
      sha256 "ef1b84c82356fd8941904768e97d501b59a6617d546415727a00ff15fd2d78b8"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
