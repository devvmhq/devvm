#!/bin/bash
# Publish the release VERSION names, after git tag -s v<version> on main's HEAD:
#   scripts/publish.sh            check, build dist/ (scripts/release.sh, which asks for the minisign
#                                 password), show what it publishes, ask once, then publish
#   scripts/publish.sh --dry-run  everything up to the question: nothing is pushed or published, and the
#                                 tap gets no commit
#   scripts/publish.sh --help     this
# It pushes main and the tag to origin, makes the GitHub release devvmhq/devvm v<version> with the archives,
# the source tarball, SHA256SUMS, SHA256SUMS.minisig and install.sh, its notes CHANGELOG.md's entry, then
# commits dist/devvm.rb to the tap's Formula/devvm.rb, and dist/devvm-rc.rb to its Formula/devvm-rc.rb when
# the tap's devvm-rc is older, so devvm-rc's users get the release too, and pushes the tap. Each step skips
# what's already done, so after a failure, fix it and run it again.
# A prerelease (VERSION x.y.z-rc.N, such as 0.2.0-rc.1) gets a GitHub release marked as a prerelease, which
# GitHub's latest release, and so install.sh's default and devvm upgrade, skips. Its tap commit,
# devvm-rc <version>, changes only Formula/devvm-rc.rb, when the tap's devvm-rc is older, so the formula
# devvm stays on the last release.
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
# formula_version FILE: the release the formula FILE installs, from its first URL
# (releases/download/v<version>/); "" for no file or none it names as release.sh writes versions.
formula_version() {
  local got
  [[ -f $1 ]] || return 0
  got=$(sed -n 's|.*/releases/download/v\([^/"]*\)/.*|\1|p' "$1" | head -n1)
  [[ $got =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z]+\.[0-9]+)?$ ]] && echo "$got"
  return 0
}
# newer A B: whether version A is newer than version B, both x.y.z or x.y.z-<word>.N (release.sh's
# forms): numbers as numbers, a prerelease before its release, words as text, then their numbers.
newer() {
  local a b i ap=0 bp=0
  IFS=.- read -r -a a <<<"$1"
  IFS=.- read -r -a b <<<"$2"
  for i in 0 1 2; do
    ((10#${a[i]} == 10#${b[i]})) || { ((10#${a[i]} > 10#${b[i]})); return; }
  done
  [[ $1 == *-* ]] && ap=1
  [[ $2 == *-* ]] && bp=1
  ((ap == bp)) || { ((ap < bp)); return; }
  ((ap)) || return 1
  [[ ${a[3]} == "${b[3]}" ]] || { [[ ${a[3]} > ${b[3]} ]]; return; }
  ((10#${a[4]} > 10#${b[4]}))
}

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

# The tap.
tap=${DEVVM_TAP_DIR:-$(dirname "$root")/homebrew-tap}
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
# The tap's formulas this release writes: devvm.rb for a release, and devvm-rc.rb unless the tap's
# devvm-rc is newer than this, so devvm-rc only moves forward, to a prerelease or a release.
rc_was=$(formula_version "$tap/Formula/devvm-rc.rb")
rc_moves=true
if [[ -n $rc_was ]] && newer "$rc_was" "$v"; then rc_moves=false; fi
tap_files=()
$pre || tap_files+=(devvm.rb)
! $rc_moves || tap_files+=(devvm-rc.rb)
tap_msg="devvm $v"
$pre && tap_msg="devvm-rc $v"

# The build, from the tag. minisign asks for the password.
scripts/release.sh || die "scripts/release.sh failed: nothing published"
for f in "${files[@]}" ${tap_files[@]+"${tap_files[@]}"}; do [[ -f dist/$f ]] || die "dist/ has no $f: nothing published"; done
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
if ((${#tap_files[@]})); then
  done_tap=true
  [[ $tap_origin == "$tap_head" ]] || done_tap=false
  for f in "${tap_files[@]}"; do cmp -s "$tap/Formula/$f" "dist/$f" || done_tap=false; done
else
  done_tap=true
fi
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
echo "Tap, $tap:"
$pre && echo "Formula/devvm.rb: stays on the last release, as this is a prerelease."
for f in ${tap_files[@]+"${tap_files[@]}"}; do
  if [[ $f == devvm-rc.rb && $rc_was != "$v" ]] && ! $pre; then
    if [[ -n $rc_was ]]; then
      echo "Formula/devvm-rc.rb, which moves from $rc_was to this release too:"
    else
      echo "Formula/devvm-rc.rb, which the tap doesn't have yet, for this release too:"
    fi
  else
    echo "Formula/$f:"
  fi
  old=$tap/Formula/$f
  [[ -f $old ]] || old=/dev/null
  git -C "$tap" diff --no-index -- "$old" "$root/dist/$f" || true
  if cmp -s "$old" "dist/$f"; then echo "  (already this release's)"; fi
done
$rc_moves || echo "Formula/devvm-rc.rb: stays at $rc_was, which is newer than $v."
echo
echo "Steps:"
echo "  1. git push origin main: $(state $done_main "to do")"
echo "  2. git push origin $tag: $(state $done_tag "to do")"
todo="to do"
[[ $release != draft ]] || todo="to do, the draft's files replaced"
$pre && todo="$todo, marked as a prerelease"
echo "  3. the GitHub $kind $tag: $(state $done_release "$todo")"
if ((${#tap_files[@]})); then
  echo "  4. the tap's commit, $tap_msg, and git push: $(state $done_tap "to do")"
else
  echo "  4. the tap: nothing to change"
fi
echo

check() {
  local formula=devvm
  $pre && formula=devvm-rc
  if $pre && ! $rc_moves; then
    echo "Check it with the install script, or devvm upgrade --to $v:"
  else
    echo "Check it with Homebrew, brew update && brew upgrade $formula (or brew trust --formula devvmhq/tap/$formula, then brew install devvmhq/tap/$formula), or the install script:"
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
  $done_tap && { echo "the tap is already at $tap_msg"; return 0; }
  local f changed=()
  for f in "${tap_files[@]}"; do
    cmp -s "$tap/Formula/$f" "dist/$f" && continue
    mkdir -p "$tap/Formula" && cp "dist/$f" "$tap/Formula/$f" || return 1
    changed+=("Formula/$f")
  done
  if ((${#changed[@]})); then
    git -C "$tap" add "${changed[@]}" && git -C "$tap" commit -q -m "$tap_msg" || return 1
  fi
  git -C "$tap" push origin main
}
names=(main tag release)
labels=("main on origin" "the tag $tag on origin" "the GitHub $kind $tag")
if ((${#tap_files[@]})); then
  names+=(tap)
  labels+=("the tap's Formula/${tap_files[0]}${tap_files[1]:+ and Formula/${tap_files[1]}}")
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
