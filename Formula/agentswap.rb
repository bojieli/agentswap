class Agentswap < Formula
  desc "Local failover proxy for Claude Code and Codex"
  homepage "https://github.com/bojieli/agentswap"
  version "0.9.0"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.9.0/agentswap_v0.9.0_darwin_arm64.tar.gz"
      sha256 "6ea0710e19ea6c8127b471a05574951ca666ae173e151b391278dce42955e8e2"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.9.0/agentswap_v0.9.0_darwin_amd64.tar.gz"
      sha256 "3b272b3764c2aaccd9c90307593553ffc65f05e2dd7449470c489a90bcecc028"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/bojieli/agentswap/releases/download/v0.9.0/agentswap_v0.9.0_linux_arm64.tar.gz"
      sha256 "16717c16072bd91bd5a08eae0d9b29a03a75677b55eb1486b94c73fc570aff80"
    else
      url "https://github.com/bojieli/agentswap/releases/download/v0.9.0/agentswap_v0.9.0_linux_amd64.tar.gz"
      sha256 "e12b283910eb1bdb7ff1382cb091e0301e802789fbad660e32dd8d6ab2656091"
    end
  end

  def install
    bin.install "agentswap"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agentswap version")
  end
end
