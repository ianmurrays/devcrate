# Seed for Formula/devcrate.rb in ianmurrays/homebrew-devcrate. The tap's copy
# is the live one: .github/workflows/release.yml bumps its url and sha256 on
# every v* tag, so this file's sha256 is only a placeholder.
class Devcrate < Formula
  desc "Run dev Environments on cloud VMs that stop when idle"
  homepage "https://github.com/ianmurrays/devcrate"
  url "https://github.com/ianmurrays/devcrate/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  head "https://github.com/ianmurrays/devcrate.git", branch: "main"

  depends_on "go" => :build
  depends_on "jq"

  PROVIDERS = %w[digitalocean vultr].freeze

  def install
    # devcrate finds the Agents and Providers next to its resolved executable,
    # so everything goes in libexec and only devcrate is linked into bin.
    system "go", "build", *std_go_args(output: libexec/"devcrate"), "./cmd/devcrate"
    %w[amd64 arm64].each do |arch|
      with_env(CGO_ENABLED: "0", GOOS: "linux", GOARCH: arch) do
        system "go", "build", *std_go_args(output: libexec/"devcrate-agent-linux-#{arch}"), "./cmd/devcrate-agent"
      end
    end
    PROVIDERS.each { |p| libexec.install "providers/#{p}/devcrate-provider-#{p}" }
    bin.install_symlink libexec/"devcrate"
  end

  def caveats
    <<~EOS
      Set up a Provider before running `devcrate up`:
        https://github.com/ianmurrays/devcrate#install
      The DigitalOcean Provider also needs doctl (`brew install doctl`).
    EOS
  end

  test do
    %w[devcrate-agent-linux-amd64 devcrate-agent-linux-arm64].each { |f| assert_path_exists libexec/f }
    PROVIDERS.each { |p| assert_path_exists libexec/"devcrate-provider-#{p}" }
    assert_equal "[]", shell_output("#{bin}/devcrate list --json").strip
  end
end
