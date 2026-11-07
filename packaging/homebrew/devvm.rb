# The formulas for the devvmhq/homebrew-tap repo: devvm, a release's, and devvm-rc, the newest
# prerelease's or release's. scripts/release.sh fills in @CLASS@ and @CONFLICTS@ for each formula, and
# each platform's @URL_<OS>_<ARCH>@ and @SHA256_<OS>_<ARCH>@ from the release's SHA256SUMS, and writes the
# result to dist/devvm.rb or dist/devvm-rc.rb. It installs the release's binary, which SHA256SUMS.minisig
# signs. Homebrew reads the version from the URLs (devvm-<version>-<os>-<arch>.tar.gz).
class @CLASS@ < Formula
  desc "Isolated Lima VM per project folder, with shared read-only toolchains"
  homepage "https://github.com/devvmhq/devvm"
  license "MIT"

  depends_on "lima"

  on_macos do
    on_arm do
      url "@URL_DARWIN_ARM64@"
      sha256 "@SHA256_DARWIN_ARM64@"
    end
    on_intel do
      url "@URL_DARWIN_AMD64@"
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    on_arm do
      url "@URL_LINUX_ARM64@"
      sha256 "@SHA256_LINUX_ARM64@"
    end
    on_intel do
      url "@URL_LINUX_AMD64@"
      sha256 "@SHA256_LINUX_AMD64@"
    end
  end

  conflicts_with "@CONFLICTS@", because: "both install the devvm binary"

  def install
    bin.install "devvm"
  end

  test do
    assert_equal version.to_s, shell_output("#{bin}/devvm --version").strip
  end
end
