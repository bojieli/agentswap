class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.8.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.8.0/agentswap_v0.8.0_darwin_arm64.tar.gz"
      sha256 "aeb9d6f617a9c8591a29c897d1e31f9120c9e1bfaaa86d4846cb25f99c5215f1"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.8.0/agentswap_v0.8.0_darwin_amd64.tar.gz"
      sha256 "be50e95c7a42528046d68dbbbda527cdffb23b4a93afb50ca2f46f69f1e158cb"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.8.0/agentswap_v0.8.0_linux_arm64.tar.gz"
      sha256 "3345eada17091ff38fa5203573f2c967fe55085f6b2cf4f8e9ca85585d3f47c1"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.8.0/agentswap_v0.8.0_linux_amd64.tar.gz"
      sha256 "6c27975a8b24d5b36358bbc6f8cf40cf4be37adb48e4a50ae0afd9e636b88f5b"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
