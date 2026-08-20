class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.2.3"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.2.3/agentswap_v0.2.3_darwin_arm64.tar.gz"
      sha256 "507ec5a104de2394cf212ea8c7a7b6f67a67fc8d643bc907fe13f7961d7832cf"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.2.3/agentswap_v0.2.3_darwin_amd64.tar.gz"
      sha256 "a55358749fe6342126bf21d135473295b0df3ce98168734ebe5330aafd7b11ec"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.2.3/agentswap_v0.2.3_linux_arm64.tar.gz"
      sha256 "be56feff09de6e00ad66929f09b8cbe4662f457ae8cb2e1b403240e49aca56ab"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.2.3/agentswap_v0.2.3_linux_amd64.tar.gz"
      sha256 "c10acd9d00aeb011b6b817fc0e06fc2bd66331c34549085b8b73cf39c92167ab"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
