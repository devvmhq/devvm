#!/bin/bash
# Build a release's files in dist/ with GoReleaser (.goreleaser.yaml), from git (only what's committed):
#   devvm-<version>-<os>-<arch>.tar.gz  the binary and the files it reads, for darwin and linux, arm64 and amd64
#   devvm-<version>.tar.gz              the source, for whoever builds devvm themselves
#   SHA256SUMS                          their checksums and install.sh's, which install.sh checks
#   SHA256SUMS.minisig                  SHA256SUMS signed with the release key (minisign), which install.sh checks
#   install.sh                          the install script, set to install this release and to trust the key
#   devvm.rb                            the Homebrew formula for devvmhq/homebrew-tap: each platform's archive
# Nothing is published. The version is VERSION's: x.y.z, or a prerelease's x.y.z-rc.N (any lowercase word,
# such as beta or alpha, then a number). The public key is packaging/minisign.pub (committed); the secret key
# is the maintainer's, at DEVVM_MINISIGN_KEY (default ~/.minisign/devvm.key), never in the repo.
#   scripts/release.sh          from the tag v<version>, which must exist and be signed (git tag -s)
#   scripts/release.sh --head   from HEAD, for testing (DEVVM_RELEASE_URL: where the archives will be;
#                               DEVVM_MINISIGN_KEY: a test key, else nothing is signed;
#                               DEVVM_MINISIGN_PUB: its public key, else packaging/minisign.pub)
#   scripts/release.sh --check  only the tag's checks and what the build needs, then prints the tag's commit
#                               (scripts/publish.sh runs it first); nothing is built
#   scripts/release.sh --formula-from DIR
#                               only dist/devvm.rb, for VERSION's published release, from its SHA256SUMS
#                               and SHA256SUMS.minisig downloaded into DIR, checked against minisign.pub
# Needs goreleaser and minisign, pinned in mise.toml: mise exec -- scripts/release.sh.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
die() { echo "release: $*" >&2; exit 1; }

v=$(tr -d '[:space:]' < VERSION)
# A release, x.y.z, or a prerelease of one, x.y.z-<word>.N (such as 0.2.0-rc.1 or 0.2.0-beta.2).
[[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z]+\.[0-9]+)?$ ]] || die "VERSION isn't x.y.z or x.y.z-rc.N: '$v'"
ref=v$v
check=false

# formula TEMPLATE SUMS URL VERSION: the Homebrew formula, from its template, with each platform's
# archive's URL and its checksum from SHA256SUMS (on lines that aren't comments).
formula() {
  awk -v sums="$2" -v u="$3" -v v="$4" '
    BEGIN {
      while ((getline line < sums) > 0) { split(line, f, " "); sha[f[2]] = f[1] }
      split("darwin linux", oses, " "); split("arm64 amd64", arches, " ")
    }
    !/^#/ {
      for (i in oses) for (j in arches) {
        p = toupper(oses[i] "_" arches[j]); a = "devvm-" v "-" oses[i] "-" arches[j] ".tar.gz"
        if (index($0, "@SHA256_" p "@")) {
          if (sha[a] !~ /^[0-9a-f]+$/ || length(sha[a]) != 64) { print "SHA256SUMS has no " a > "/dev/stderr"; bad = 1 }
          gsub("@SHA256_" p "@", sha[a])
        }
        gsub("@URL_" p "@", u "/" a)
      }
      if (/@[A-Z0-9_]+@/) { print "the formula has a placeholder left: " $0 > "/dev/stderr"; bad = 1 }
    }
    { print } END { exit bad }' "$1"
}

case ${1:-} in
  --head) ref=HEAD ;;
  --check) check=true ;;
  --formula-from)
    # The formula for a published release, from its downloaded SHA256SUMS and SHA256SUMS.minisig in DIR,
    # checked against packaging/minisign.pub; nothing is built.
    dir=${2:-}
    [[ -n $dir && -f $dir/SHA256SUMS && -f $dir/SHA256SUMS.minisig ]] || die "--formula-from DIR needs DIR/SHA256SUMS and DIR/SHA256SUMS.minisig"
    command -v minisign >/dev/null || die "needs minisign (mise exec -- scripts/release.sh)"
    minisign -Vm "$dir/SHA256SUMS" -x "$dir/SHA256SUMS.minisig" -p packaging/minisign.pub >/dev/null ||
      die "$dir/SHA256SUMS doesn't verify with packaging/minisign.pub"
    mkdir -p dist
    formula packaging/homebrew/devvm.rb "$dir/SHA256SUMS" "https://github.com/devvmhq/devvm/releases/download/v$v" "$v" \
      > dist/devvm.rb || die "couldn't write the formula"
    echo "==> dist/devvm.rb for devvm $v, from $dir/SHA256SUMS"
    exit 0 ;;
  "") ;;
  *) die "unknown argument '$1' (--head, --check, --formula-from DIR)" ;;
esac
if [[ $ref == HEAD ]]; then obj=HEAD; else obj=$(git rev-parse -q --verify "refs/tags/$ref") || die "no tag $ref: tag the release first (git tag -s $ref)"; fi
# A release comes from a tag the maintainer signed, not just any tag with the right name. The tag
# object checked is the one built from: its commit is resolved once, and used for everything below.
[[ $ref == HEAD ]] || git verify-tag "$obj" >/dev/null 2>&1 ||
  die "tag $ref isn't signed by a key git trusts (make it with git tag -s; see docs/content/docs/contributing/maintaining.md)"
# And by the release signer, not just any key git trusts: its fingerprint is pinned in this checkout's
# packaging/release-signer (one per line; # comments), never the tagged commit's, which the tag brings.
if [[ $ref != HEAD ]]; then
  signers=$root/packaging/release-signer
  [[ -s $signers ]] || die "no packaging/release-signer: write the tag-signing key's fingerprint there (see docs/content/docs/contributing/maintaining.md)"
  raw=$(git verify-tag --raw "$obj" 2>&1) || true
  # GPG: VALIDSIG <signing key> … <primary key>; SSH: "… key SHA256:…".
  fps=$(awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { print $3; print $NF }' <<<"$raw"
        grep -oE 'SHA256:[A-Za-z0-9+/]{43}' <<<"$raw" || true)
  fps=$(sort -u <<<"$fps")
  ok=false
  while IFS= read -r fp; do
    [[ -n $fp ]] && grep -v '^#' "$signers" | tr -d ' \t' | grep -xiF "$fp" >/dev/null && ok=true
  done <<<"$fps"
  $ok || die "tag $ref is signed by $(paste -sd' ' - <<<"$fps"), not a key in packaging/release-signer"
fi
commit=$(git rev-parse -q --verify "$obj^{commit}") || die "no commit for $ref"
[[ "$(git show "$commit:VERSION" | tr -d '[:space:]')" == "$v" ]] || die "$ref's VERSION isn't $v"
pubkey=$(git show "$commit:packaging/minisign.pub" 2>/dev/null | sed -n 2p) || true
# A test key's public key, for HEAD only: a release verifies with the committed one.
if [[ $ref == HEAD && -n ${DEVVM_MINISIGN_PUB:-} ]]; then
  pubkey=$(sed -n 2p "$DEVVM_MINISIGN_PUB") || die "can't read $DEVVM_MINISIGN_PUB"
fi
key=${DEVVM_MINISIGN_KEY:-$HOME/.minisign/devvm.key}
sign=true
if [[ $ref == HEAD && -z ${DEVVM_MINISIGN_KEY:-} ]]; then sign=false
else
  [[ -n $pubkey ]] || die "no packaging/minisign.pub in $ref: create the release key first (see docs/content/docs/contributing/maintaining.md)"
  [[ -f $key ]] || die "no release key at $key (DEVVM_MINISIGN_KEY)"
  command -v minisign >/dev/null || die "needs minisign to sign the release (mise exec -- scripts/release.sh)"
fi
git cat-file -e "$commit:install.sh" 2>/dev/null || die "no install.sh in $ref"
command -v goreleaser >/dev/null || die "needs goreleaser (mise exec -- scripts/release.sh)"
if $check; then echo "$commit"; exit 0; fi
url=${DEVVM_RELEASE_URL:-https://github.com/devvmhq/devvm/releases/download/v$v}

# GoReleaser builds a clone of the commit, so only what's committed goes in, whatever this checkout
# holds. The version is VERSION's, for HEAD too (DEVVM_VERSION, in .goreleaser.yaml's snapshot).
out=$root/dist
tmp=$(mktemp -d "${TMPDIR:-/tmp}/devvm-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
src=$tmp/src
{ git clone -q --no-checkout "$root" "$src" && git -C "$src" checkout -q --detach "$commit"; } ||
  die "couldn't check out $ref"
snap=()
if [[ $ref == HEAD ]]; then snap=(--snapshot); else export GORELEASER_CURRENT_TAG=$ref; fi
(cd "$src" && DEVVM_VERSION=$v goreleaser release --clean --skip=publish ${snap[@]+"${snap[@]}"}) || die "goreleaser failed"
rm -rf "$out"
mv "$src/dist" "$out"

# The built binary for this host says the release's version (GoReleaser's own default sets one devvm
# doesn't read).
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) arch=none ;; esac
for bin in "$out"/devvm*_"${os}_$arch"*/devvm; do
  [[ -x $bin ]] || continue
  got=$("$bin" --version) || die "$bin --version failed"
  [[ $got == "$v" ]] || die "$bin --version says '$got', not $v"
done

$sign || pubkey=""   # an unsigned test build: its install.sh doesn't expect a signature

git show "$commit:install.sh" |
  awk -v v="$v" -v k="$pubkey" '
    /^release_version=""$/ { $0 = "release_version=\"" v "\""; n++ }
    /^minisign_key=""$/ { $0 = "minisign_key=\"" k "\""; m++ }
    { print } END { exit !(n == 1 && m == 1) }' \
  > "$out/install.sh" || die "install.sh needs one release_version=\"\" and one minisign_key=\"\" line"
chmod 755 "$out/install.sh"
git show "$commit:packaging/homebrew/devvm.rb" > "$tmp/devvm.rb" || die "no packaging/homebrew/devvm.rb in $ref"
formula "$tmp/devvm.rb" "$out/SHA256SUMS" "$url" "$v" > "$out/devvm.rb" || die "couldn't write the formula"

# install.sh's checksum goes in SHA256SUMS before it's signed, so you can check the script against the
# release key before you run it.
(cd "$out" && { sha256sum install.sh 2>/dev/null || shasum -a 256 install.sh; } >> SHA256SUMS) ||
  die "couldn't add install.sh to SHA256SUMS"
if $sign; then
  minisign -Sm "$out/SHA256SUMS" -s "$key" -t "devvm $v" || die "couldn't sign SHA256SUMS"
  minisign -Vm "$out/SHA256SUMS" -P "$pubkey" >/dev/null || die "SHA256SUMS.minisig doesn't verify with packaging/minisign.pub"
fi

echo "==> dist/ for devvm $v (from $ref):"
(cd "$out" && ls -l devvm-*.tar.gz SHA256SUMS* install.sh devvm.rb 2>/dev/null) || true
$sign || echo "(not signed: a test build without DEVVM_MINISIGN_KEY)"
if [[ $v == *-* ]]; then
  echo "scripts/publish.sh publishes them: the archives, SHA256SUMS, SHA256SUMS.minisig and install.sh to the GitHub prerelease v$v; devvm.rb stays out of the tap, which keeps the last release."
else
  echo "scripts/publish.sh publishes them: the archives, SHA256SUMS, SHA256SUMS.minisig and install.sh to the GitHub release v$v, devvm.rb to devvmhq/homebrew-tap's Formula/."
fi
