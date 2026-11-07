#!/bin/bash
# Publish the release VERSION names, after git tag -s v<version> on main's HEAD:
#   scripts/publish.sh            check, build dist/ (scripts/release.sh, which asks for the minisign
#                                 password), show what it publishes, ask once, then publish
#   scripts/publish.sh --dry-run  everything up to the question: nothing is pushed or published, and the
#                                 tap gets no commit
#   scripts/publish.sh --help     this
# It pushes main and the tag to origin, makes the GitHub release devvmhq/devvm v<version> with the archives,
# the source tarball, SHA256SUMS, SHA256SUMS.minisig and install.sh, its notes CHANGELOG.md's entry, then
# commits dist/devvm.rb to the tap's Formula/devvm.rb and pushes the tap. Each step skips what's already
# done, so after a failure, fix it and run it again.
# A prerelease (VERSION x.y.z-rc.N, such as 0.2.0-rc.1) gets a GitHub release marked as a prerelease, which
# GitHub's latest release, and so install.sh's default and devvm upgrade, skips, and no tap commit: the tap,
# and so Homebrew, stays on the last release, and no tap checkout is needed.
# DEVVM_TAP_DIR: the checkout of devvmhq/homebrew-tap, clean and on main (default ../homebrew-tap, next to
# this repo). DEVVM_MINISIGN_KEY: the release key (scripts/release.sh's).
# Needs gh, logged in (gh auth login), and release.sh's goreleaser and minisign: mise exec -- scripts/publish.sh.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
repo=devvmhq/devvm
die() { echo "publish: $*" >&2; exit 1; }
usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$root/scripts/publish.sh"; }

dry=false
for a in "$@"; do
  case $a in
    --dry-run) dry=true ;;
    -h | --help) usage; exit 0 ;;
    *) echo "publish: unknown argument '$a'" >&2; usage >&2; exit 2 ;;
  esac
done

v=$(tr -d '[:space:]' < VERSION)
tag=v$v
# A prerelease's version has a -suffix (release.sh checks its form): its release is marked as one.
pre=false pre_flag=()
if [[ $v == *-* ]]; then pre=true pre_flag=(--prerelease); fi
# remote_ref DIR REF: the object REF names on DIR's origin, "" when it has none.
remote_ref() {
  local out
  out=$(git -C "$1" ls-remote origin "$2") || return 1
  awk -v r="$2" '$2 == r { print $1 }' <<<"$out"
}
sha256() { { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"; } | cut -c1-64; }

# The preconditions, before anything asks for the password.
[[ -z $(git status --porcelain) ]] || die "the working tree isn't clean: commit or remove what git status lists"
command -v gh >/dev/null || die "needs gh, GitHub's command line (brew install gh)"
gh auth status >/dev/null 2>&1 || die "gh isn't logged in to GitHub: run gh auth login"
command -v minisign >/dev/null || die "needs minisign (mise exec -- scripts/publish.sh)"
# The tag: it exists, is signed, and by a key in packaging/release-signer; release.sh's own checks.
commit=$(scripts/release.sh --check) || exit 1
main=$(git rev-parse -q --verify refs/heads/main) || die "no branch main"
[[ $main == "$commit" ]] || die "tag $tag is at ${commit:0:12}, not main's HEAD (${main:0:12}): tag main's HEAD"
tagobj=$(git rev-parse "refs/tags/$tag")
origin_main=$(remote_ref . refs/heads/main) || die "can't read origin (git ls-remote origin)"
if [[ -n $origin_main && $origin_main != "$commit" ]]; then
  git merge-base --is-ancestor "$origin_main" "$commit" 2>/dev/null ||
    die "origin's main has commits main lacks: pull, then tag main's HEAD again"
fi
origin_tag=$(remote_ref . "refs/tags/$tag") || die "can't read origin (git ls-remote origin)"
[[ -z $origin_tag || $origin_tag == "$tagobj" ]] ||
  die "origin has another tag $tag: this one is $tagobj, origin's $origin_tag"

files=()
for os in darwin linux; do
  for arch in arm64 amd64; do files+=("devvm-$v-$os-$arch.tar.gz"); done
done
files+=("devvm-$v.tar.gz" SHA256SUMS SHA256SUMS.minisig install.sh)
# The release: none yet, a draft (a run that stopped while uploading: its files are replaced), or
# published (a run that stopped later: it needs every file, and to be a prerelease just when this is one).
release=none
if out=$(gh release view "$tag" --repo "$repo" --json isDraft,isPrerelease,assets --jq '(.isDraft | tostring), (.isPrerelease | tostring), .assets[].name' 2>&1); then
  release=published
  [[ $(head -n1 <<<"$out") != true ]] || release=draft
  if [[ $release == published ]]; then
    for f in "${files[@]}"; do
      tail -n +3 <<<"$out" | grep -qxF "$f" ||
        die "devvm $tag already has a GitHub release, without $f: finish it or delete it by hand (gh release view $tag --repo $repo)"
    done
    marked=$(sed -n 2p <<<"$out")
    if $pre && [[ $marked != true ]]; then
      die "devvm $tag is a prerelease, but its GitHub release isn't marked as one: mark it by hand (gh release edit $tag --repo $repo --prerelease)"
    elif ! $pre && [[ $marked == true ]]; then
      die "devvm $tag's GitHub release is marked as a prerelease: unmark it by hand (gh release edit $tag --repo $repo --prerelease=false)"
    fi
  fi
elif ! grep -qi 'not found' <<<"$out"; then
  die "can't read the GitHub release $tag (gh release view): $out"
fi

# The tap, which a prerelease leaves alone.
tap=${DEVVM_TAP_DIR:-$(dirname "$root")/homebrew-tap}
if ! $pre; then
  [[ -d $tap ]] && top=$(git -C "$tap" rev-parse --show-toplevel 2>/dev/null) && [[ $top == "$(cd "$tap" && pwd -P)" ]] ||
    die "no tap checkout at $tap: clone devvmhq/homebrew-tap there, or set DEVVM_TAP_DIR"
  [[ $(git -C "$tap" symbolic-ref -q --short HEAD) == main ]] || die "the tap at $tap isn't on main"
  [[ -z $(git -C "$tap" status --porcelain) ]] || die "the tap at $tap isn't clean: commit or remove what git status lists"
  tap_origin=$(remote_ref "$tap" refs/heads/main) || die "can't read the tap's origin (git -C $tap ls-remote origin)"
  tap_head=$(git -C "$tap" rev-parse HEAD)
  if [[ -n $tap_origin && $tap_origin != "$tap_head" ]]; then
    git -C "$tap" merge-base --is-ancestor "$tap_origin" "$tap_head" 2>/dev/null ||
      die "the tap's origin has commits its main lacks: git -C $tap pull"
  fi
fi

# The build, from the tag. minisign asks for the password.
scripts/release.sh || die "scripts/release.sh failed: nothing published"
for f in "${files[@]}" devvm.rb; do [[ -f dist/$f ]] || die "dist/ has no $f: nothing published"; done
tmp=$(mktemp -d "${TMPDIR:-/tmp}/devvm-publish.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
notes=$tmp/notes.md
git show "$commit:CHANGELOG.md" | awk -v h="## $v" '
  $0 == h { on = 1; next }
  on && /^## / { exit }
  on && NF { if (started) for (; blank > 0; blank--) print ""; print; started = 1; blank = 0; next }
  on { blank++ }' > "$notes"
[[ -s $notes ]] || die "CHANGELOG.md has no entry ## $v: nothing published"
# A published release is this build's only if its SHA256SUMS is dist/'s.
if [[ $release == published ]]; then
  gh release download "$tag" --repo "$repo" --pattern SHA256SUMS --dir "$tmp/published" >/dev/null ||
    die "can't download the GitHub release's SHA256SUMS: nothing published"
  cmp -s "$tmp/published/SHA256SUMS" dist/SHA256SUMS ||
    die "devvm $tag is already published from another build: its SHA256SUMS isn't dist/'s, so finish it by hand"
fi

done_main=false done_tag=false done_release=false done_tap=false
[[ $origin_main != "$commit" ]] || done_main=true
[[ $origin_tag != "$tagobj" ]] || done_tag=true
[[ $release != published ]] || done_release=true
if $pre || { cmp -s "$tap/Formula/devvm.rb" dist/devvm.rb && [[ $tap_origin == "$tap_head" ]]; }; then done_tap=true; fi
state() { if $1; then echo "done"; else echo "$2"; fi; }

echo
echo "==> devvm $v, to publish"
kind=release
$pre && kind=prerelease
if $pre; then echo "Version: $v, a prerelease"; else echo "Version: $v"; fi
echo "Tag:     $tag, signed, at commit $commit"
echo
echo "Files, from dist/, for the GitHub $kind $repo $tag:"
for f in "${files[@]}"; do printf '  %-34s %10s bytes  %s\n' "$f" "$(wc -c < "dist/$f" | tr -d ' ')" "$(sha256 "dist/$f")"; done
echo
echo "Release notes, CHANGELOG.md's ## $v:"
sed 's/^/  /' "$notes"
echo
if $pre; then
  echo "Tap: skipped, as this is a prerelease: Homebrew stays on the last release."
else
  echo "Tap, $tap, Formula/devvm.rb:"
  old=$tap/Formula/devvm.rb
  [[ -f $old ]] || old=/dev/null
  git -C "$tap" diff --no-index -- "$old" "$root/dist/devvm.rb" || true
  cmp -s "$old" dist/devvm.rb && echo "  (already this release's)"
fi
echo
echo "Steps:"
echo "  1. git push origin main: $(state $done_main "to do")"
echo "  2. git push origin $tag: $(state $done_tag "to do")"
todo="to do"
[[ $release != draft ]] || todo="to do, the draft's files replaced"
$pre && todo="$todo, marked as a prerelease"
echo "  3. the GitHub $kind $tag: $(state $done_release "$todo")"
if $pre; then
  echo "  4. the tap: skipped (a prerelease)"
else
  echo "  4. the tap's commit, devvm $v, and git push: $(state $done_tap "to do")"
fi
echo

check() {
  if $pre; then
    echo "Check it with the install script, or devvm upgrade --to $v:"
  else
    echo "Check it with Homebrew, brew update && brew upgrade devvm (or brew trust --formula devvmhq/tap/devvm, then brew install devvmhq/tap/devvm), or the install script:"
  fi
  echo "  curl -fsSL https://github.com/devvmhq/devvm/releases/download/$tag/install.sh | bash"
}
if $done_main && $done_tag && $done_release && $done_tap; then
  echo "devvm $v is already published: nothing to do."
  check
  exit 0
fi
if $dry; then echo "--dry-run: nothing published."; exit 0; fi

{ : </dev/tty; } 2>/dev/null || die "no terminal to ask on: nothing published"
printf 'Publish devvm %s? [y/N] ' "$v" >/dev/tty
answer=
read -r answer </dev/tty || true
[[ $answer == y || $answer == Y ]] || die "nothing published"

step_main() {
  $done_main && { echo "origin's main is already at ${commit:0:12}"; return 0; }
  git push origin main
}
step_tag() {
  $done_tag && { echo "origin already has $tag"; return 0; }
  git push origin "refs/tags/$tag"
}
step_release() {
  $done_release && { echo "the GitHub release $tag is already published"; return 0; }
  local paths=() f
  for f in "${files[@]}"; do paths+=("dist/$f"); done
  # A draft first, published once every file is uploaded: one that stops halfway is never seen.
  if [[ $release == draft ]]; then
    gh release upload "$tag" --repo "$repo" --clobber "${paths[@]}" || return 1
  else
    gh release create "$tag" --repo "$repo" --title "devvm $v" --notes-file "$notes" --verify-tag --draft \
      ${pre_flag[@]+"${pre_flag[@]}"} "${paths[@]}" || return 1
  fi
  gh release edit "$tag" --repo "$repo" --title "devvm $v" --notes-file "$notes" --draft=false ${pre_flag[@]+"${pre_flag[@]}"}
}
step_tap() {
  $done_tap && { echo "the tap is already at devvm $v"; return 0; }
  if ! cmp -s "$tap/Formula/devvm.rb" dist/devvm.rb; then
    mkdir -p "$tap/Formula" && cp dist/devvm.rb "$tap/Formula/devvm.rb" || return 1
    git -C "$tap" add Formula/devvm.rb && git -C "$tap" commit -q -m "devvm $v" || return 1
  fi
  git -C "$tap" push origin main
}
names=(main tag release tap)
labels=("main on origin" "the tag $tag on origin" "the GitHub release $tag" "the tap's Formula/devvm.rb")
if $pre; then
  names=(main tag release)
  labels=("main on origin" "the tag $tag on origin" "the GitHub prerelease $tag")
fi
for i in "${!names[@]}"; do
  echo "==> ${labels[$i]}"
  if ! "step_${names[$i]}"; then
    {
      echo "publish: ${labels[$i]} failed."
      if ((i > 0)); then printf '  published: %s\n' "${labels[@]:0:i}"; fi
      printf '  not published: %s\n' "${labels[@]:i}"
      echo "Fix what failed, then run scripts/publish.sh again, which skips what's done, or finish by hand (docs/content/docs/contributing/maintaining.md)."
    } >&2
    exit 1
  fi
done
echo
echo "==> devvm $v is published"
check
