class ClaudeBridge < Formula
  desc "Control Claude Code sessions remotely from WeChat via iLink"
  homepage "https://github.com/coderabbit214/claude-bridge"
  url "https://github.com/coderabbit214/claude-bridge/archive/refs/tags/v0.0.1.tar.gz"
  sha256 "f1fb99af3ce81298e738b447242a27986943ef8eaab036a104c450f7631f8e94"
  license "MIT"
  depends_on :macos
  depends_on "go" => :build
  depends_on "python@3"

  def install
    ENV["CGO_ENABLED"] = "1"
    system "go", "build", *std_go_args(output: bin/"claude-bridge"),
           "-ldflags", "-linkmode external", "./cmd"
    pkgshare.install "scripts", "hooks"
    # Remove compiled Python bytecode; only source files are needed at runtime.
    rm_rf Dir[pkgshare/"**/__pycache__"]
    prefix.install_metafiles
  end

  service do
    run [opt_bin/"claude-bridge", "serve"]
    keep_alive true
    log_path var/"log/claude-bridge.log"
    error_log_path var/"log/claude-bridge.log"
  end

  test do
    assert_match "claude-bridge", shell_output("#{bin}/claude-bridge help")
  end
end
