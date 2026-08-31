class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.5.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.5.0/agentswap_v0.5.0_darwin_arm64.tar.gz"
      sha256 "9c80096b59699221f59941b169c3efb1b28568ee64c67bc64d541a6fa865f92a"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.5.0/agentswap_v0.5.0_darwin_amd64.tar.gz"
      sha256 "68e9bf0fd1c2cc1ae56b20a0f48f49d07ab7397298b8b6c7ddfef7c96c56fda8"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.5.0/agentswap_v0.5.0_linux_arm64.tar.gz"
      sha256 "de726ea0129db6f19bd199b24126bc3a90115a857d56ad0c17b96c79521f6f6a"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.5.0/agentswap_v0.5.0_linux_amd64.tar.gz"
      sha256 "259c7f9371485d228fa883ead45c225fe76a627126f945f8431ba3b7f169e800"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
