#!/bin/bash
# Update the versions and checksums pinned in assets/ (layers/base.sh, layers/docker.sh, layers/nix.sh, features/*.sh,
# shared-tools/build.sh, oci/build.sh) and internal/tools/mise.go (devvm's host mise), and the
# end-to-end tests' own pins (tests/e2e-mise.lock, the --from images' digests in tests/e2e/from_test.go,
# Ubuntu's cloud image release in tests/e2e/sandbox_test.go, and CI's Lima in .github/workflows/e2e.yml).
#   scripts/bump.sh --check     show current and latest for every pin; change nothing
#   scripts/bump.sh [tool...]   update the given tools (default: all) and print a summary
# Tools: docker nix direnv nix-direnv mise rustup crane omb eza tealdeer e2e-tools e2e-from e2e-ubuntu e2e-lima
#   mise: base's, and the one devvm runs on the host to resolve the shared tools' versions
#   e2e-tools: the shared tools' versions every e2e section but tools uses
#   e2e-from: the container images e2e's --from checks start from, by digest
#   e2e-ubuntu: the dated release of Ubuntu's cloud image e2e pins base to
#   e2e-lima: the Lima release CI's e2e workflow installs, and its tarball's checksum
# The shared tools themselves (node, claude, ...) and Ubuntu's cloud image aren't pinned for users:
# devvm resolves them per user. Only e2e pins them, so its cached images stay valid between runs.
# Needs curl and jq. Checksums come from upstream's published sums where there are
# any; otherwise they're computed from the download (said so in the output). Edited scripts change
# image keys, so the affected images rebuild on next use: test with `devvm build <image>`, then
# `devvm gc`.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
all="docker nix direnv nix-direnv mise rustup crane omb eza tealdeer e2e-tools e2e-from e2e-ubuntu e2e-lima"

check=false
if [[ ${1:-} == --check ]]; then check=true; shift; fi
tools=${*:-$all}
for t in $tools; do [[ " $all " == *" $t "* ]] || { echo "bump: unknown tool '$t' (have: $all)" >&2; exit 1; }; done
for c in curl jq; do command -v "$c" >/dev/null || { echo "bump: needs $c" >&2; exit 1; }; done

summary=()
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() { curl -fsSL --retry 3 "$@"; }
gh_latest() { fetch "https://api.github.com/repos/$1/releases/latest" | jq -r .tag_name; }
gh_head() { fetch "https://api.github.com/repos/$1/commits/${2:-master}" | jq -r .sha; }
# sha256 FILE: its SHA-256, with sha256sum (coreutils) or shasum (macOS's; Perl's, which Fedora
# lacks), whichever there is.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -c1-64; }
sha_of() { fetch -o "$tmp/dl" "$1"; sha256 "$tmp/dl"; rm -f "$tmp/dl"; }

# get FILE VAR / set_var FILE VAR VALUE: a `VAR=value` line in a pinned script (a trailing comment
# stays); in a .go file, the quoted value of `const VAR = "…"` (get_const) or of a map entry
# `"VAR": "…",`.
get() { sed -n "s/^$2=\([^ ]*\).*/\1/p" "$root/$1"; }
get_const() { sed -n "s/^const $2 = \"\(.*\)\"$/\1/p" "$root/$1"; }
# get_env FILE VAR: the value of an indented `VAR: value` line in a workflow.
get_env() { sed -n "s/^ *$2: \([^ ]*\)$/\1/p" "$root/$1"; }
# valid VAR VALUE: VALUE comes from upstream and lands in a script that runs (some on the host), so
# only the shape a pin can have: a checksum, a commit, an image digest, a date or a version.
# Else exit.
valid() {
  local re='^[0-9A-Za-z][0-9A-Za-z.+~_-]*$'
  case $1 in
    *_sha256*) re='^[0-9a-f]{64}$' ;;
    *_commit) re='^[0-9a-f]{40}$' ;;
    *_digest | *Digest) re='^sha256:[0-9a-f]{64}$' ;;
    *_release) re='^release-[0-9]{8}$' ;;
    *_built) re='^[0-9]{8}T[0-9]{6}Z$' ;;
  esac
  [[ $2 =~ $re ]] || { printf 'bump: upstream gave %s an unexpected value: %q\n' "$1" "$2" >&2; exit 1; }
}
# set_var stages the edit (checked as KIND, else as VAR); write_staged makes them all once every
# tool's values are in and checked, so a bad or missing one leaves every file as it was.
staged=()
set_var() { valid "${4:-$2}" "$3"; staged+=("$1" "$2" "$3"); }
write_staged() {
  local i
  for (( i = 0; i < ${#staged[@]}; i += 3 )); do
    # The value as it is (awk -v would read backslash escapes, sub() an &).
    if [[ ${staged[i]} == *.go ]]; then
      N=${staged[i + 1]} V=${staged[i + 2]} awk '
        { l = $0; sub(/^[ \t]*/, "", l) }
        index(l, "const " ENVIRON["N"] " = \"") == 1 || index(l, "\"" ENVIRON["N"] "\":") == 1 {
          e = length($0); while (substr($0, e, 1) != "\"") e--
          b = e - 1; while (substr($0, b, 1) != "\"") b--
          $0 = substr($0, 1, b) ENVIRON["V"] substr($0, e)
        }
        { print }' "$root/${staged[i]}" > "$tmp/edit"
    elif [[ ${staged[i]} == *.yml ]]; then
      N=${staged[i + 1]} V=${staged[i + 2]} awk '
        { l = $0; sub(/^[ \t]*/, "", l) }
        index(l, ENVIRON["N"] ": ") == 1 { $0 = substr($0, 1, length($0) - length(l)) ENVIRON["N"] ": " ENVIRON["V"] }
        { print }' "$root/${staged[i]}" > "$tmp/edit"
    else
      N=${staged[i + 1]} V=${staged[i + 2]} awk 'index($0, ENVIRON["N"] "=") == 1 { s = index($0, " "); $0 = ENVIRON["N"] "=" ENVIRON["V"] (s ? substr($0, s) : "") } { print }' \
        "$root/${staged[i]}" > "$tmp/edit"
    fi
    cat "$tmp/edit" > "$root/${staged[i]}"
  done
}

# report TOOL CURRENT LATEST: print it; true when an update is wanted (not --check, and different).
report() {
  if [[ $2 == "$3" ]]; then printf '  %-9s %-14s up to date\n' "$1" "$2"; return 1; fi
  # The latest comes from upstream: no control characters on the terminal.
  local shown
  shown=$(LC_ALL=C tr -d '\000-\037\177' <<<"$3")
  # Upstream's latest can lag behind a pin (a release marker not yet moved): an older version is
  # never offered.
  local ver='^v?[0-9]+(\.[0-9]+)*$'
  if [[ $2 =~ $ver && $3 =~ $ver && $(printf '%s\n%s\n' "${2#v}" "${3#v}" | sort -V | head -1) == "${3#v}" ]]; then
    printf "  %-9s %-14s newer than upstream's latest, %s\n" "$1" "$2" "$shown"; return 1
  fi
  printf '  %-9s %-14s -> %s\n' "$1" "$2" "$shown"
  $check && return 1
  summary+=("$1: $2 -> $3")
}

# A version plus per-arch checksums in one script.
pin3() {   # pin3 FILE PREFIX VERSION SHA_ARM64 SHA_AMD64
  set_var "$1" "${2}_version" "$3"
  set_var "$1" "${2}_sha256_arm64" "$4"
  set_var "$1" "${2}_sha256_amd64" "$5"
}

# The dated release of Ubuntu's cloud image that e2e pins base to (tests/e2e/sandbox_test.go's
# e2eUbuntuRelease), so its images stay cached between runs: the newest that Ubuntu lists. devvm
# checks its signed checksums when e2e pins it, as it does the newest for users.
bump_e2e-ubuntu() {
  local f=tests/e2e/sandbox_test.go new
  new=$(fetch https://cloud-images.ubuntu.com/releases/resolute/ | grep -o 'release-[0-9]\{8\}' | sort -ru | head -1 || true)
  if report e2e-ubuntu "$(get_const $f e2eUbuntuRelease)" "$new"; then set_var $f e2eUbuntuRelease "$new" e2e_ubuntu_release; fi
}

# The Lima release CI's e2e workflow installs (.github/workflows/e2e.yml's LIMA_VERSION), and its
# Linux x86_64 tarball's line in the release's SHA256SUMS (LIMA_SHA256). Lima's release notes print
# SHA256SUMS's own SHA-256, so the file is checked against them before a line of it is used.
bump_e2e-lima() {
  local f=.github/workflows/e2e.yml new want got
  fetch https://api.github.com/repos/lima-vm/lima/releases/latest > "$tmp/lima-release"
  new=$(jq -r .tag_name "$tmp/lima-release"); new=${new#v}
  report e2e-lima "$(get_env $f LIMA_VERSION)" "$new" || return 0
  valid lima_version "$new"
  want=$(jq -r .body "$tmp/lima-release" | sed -n 's/.*sha256sum of the SHA256SUMS file itself is `\([0-9a-f]\{64\}\)`.*/\1/p' | head -1)
  fetch -o "$tmp/lima-sums" "https://github.com/lima-vm/lima/releases/download/v$new/SHA256SUMS"
  got=$(sha256 "$tmp/lima-sums")
  [[ -n $want && $got == "$want" ]] || { echo "bump: Lima $new's SHA256SUMS doesn't match its release notes' sha256" >&2; exit 1; }
  set_var $f LIMA_VERSION "$new" lima_version
  set_var $f LIMA_SHA256 "$(awk -v n="lima-$new-Linux-x86_64.tar.gz" '$2 == n { print $1 }' "$tmp/lima-sums")" lima_sha256
}

# Docker's packages: the newest of each in its repository for base's Ubuntu release, and its signing key.
bump_docker() {
  local f=assets/layers/docker.sh idx p var new sha
  idx=$(fetch https://download.docker.com/linux/ubuntu/dists/resolute/stable/binary-arm64/Packages)
  for p in docker-ce:docker containerd.io:containerd docker-buildx-plugin:buildx \
      docker-compose-plugin:compose docker-model-plugin:model; do
    var=${p#*:}_version
    # "5:29.9.0-1~ubuntu.26.04~resolute" -> 29.9.0
    new=$(awk -v p="${p%%:*}" '/^Package:/ { n = $2 } /^Version:/ && n == p { v = $2; sub(/^[0-9]+:/, "", v); sub(/-[^-]*$/, "", v); print v }' <<<"$idx" |
      sort -V | tail -1)
    if report "${p#*:}" "$(get $f "$var")" "$new"; then set_var $f "$var" "$new"; fi
  done
  sha=$(sha_of https://download.docker.com/linux/ubuntu/gpg)
  if report docker-gpg "$(get $f docker_gpg_sha256 | cut -c1-12)" "$(cut -c1-12 <<<"$sha")"; then
    echo "            (Docker's signing key changed: check it before committing)"
    set_var $f docker_gpg_sha256 "$sha"
  fi
}

# Nix's release tarball for the nix layer: the version nixos.org's installer fetches, with the SHA-256s
# Nix publishes next to each tarball. Nix's newer releases come with another installer
# (artifacts.nixos.org): when nixos.org's no longer names a tarball, this says so, and the layer's pins
# change shape.
bump_nix() {
  local f=assets/layers/nix.sh cur new u
  cur=$(get $f nix_version)
  new=$(fetch https://nixos.org/nix/install | sed -n 's|^ *path=[0-9a-z]*/nix-\([0-9.]*\)-x86_64-linux\.tar\.xz$|\1|p' | head -1)
  [[ -n $new ]] || { echo "bump: nixos.org's installer names no nix-VERSION-x86_64-linux.tar.xz: see the layer's pins" >&2; exit 1; }
  report nix "$cur" "$new" || return 0
  u="https://releases.nixos.org/nix/nix-$new/nix-$new"
  pin3 $f nix "$new" "$(fetch "$u-aarch64-linux.tar.xz.sha256" | cut -c1-64)" "$(fetch "$u-x86_64-linux.tar.xz.sha256" | cut -c1-64)"
}

bump_direnv() {
  local f=assets/layers/nix.sh cur new u
  cur=$(get $f direnv_version); new=$(gh_latest direnv/direnv); new=${new#v}
  report direnv "$cur" "$new" || return 0
  echo "            (checksums computed from the downloads)"
  u="https://github.com/direnv/direnv/releases/download/v$new"
  pin3 $f direnv "$new" "$(sha_of "$u/direnv.linux-arm64")" "$(sha_of "$u/direnv.linux-amd64")"
}

# nix-direnv: one script (direnvrc) at the release's tag.
bump_nix-direnv() {
  local f=assets/layers/nix.sh cur new
  cur=$(get $f nix_direnv_version); new=$(gh_latest nix-community/nix-direnv); new=${new#v}
  report nix-direnv "$cur" "$new" || return 0
  echo "            (checksum computed from the download)"
  set_var $f nix_direnv_version "$new"
  set_var $f nix_direnv_sha256 "$(sha_of "https://raw.githubusercontent.com/nix-community/nix-direnv/$new/direnvrc")"
}

# mise in base, and on the host (devvm's host mise, the same version: it writes the lock base's mise
# installs from), from the release's published checksums.
bump_mise() {
  local f=assets/layers/base.sh cur new sums
  cur=$(get $f mise_version); new=$(gh_latest jdx/mise); new=${new#v}
  report mise "$cur" "$new" || return 0
  sums=$(fetch "https://github.com/jdx/mise/releases/download/v$new/SHASUMS256.txt")
  pin3 $f mise "$new" "$(awk '$2 ~ /-linux-arm64$/ { print $1 }' <<<"$sums")" \
    "$(awk '$2 ~ /-linux-x64$/ { print $1 }' <<<"$sums")"
  set_var $f mise_sha256_arm64_musl "$(awk '$2 ~ /-linux-arm64-musl$/ { print $1 }' <<<"$sums")"
  set_var $f mise_sha256_amd64_musl "$(awk '$2 ~ /-linux-x64-musl$/ { print $1 }' <<<"$sums")"
  local g=internal/tools/mise.go os a
  set_var $g miseVersion "$new" mise_version
  for os in macos linux; do
    for a in arm64 x64; do
      set_var $g "$os-$a" "$(awk -v s="-$os-$a" '$2 ~ s "$" { print $1 }' <<<"$sums")" mise_sha256
    done
  done
}

bump_rustup() {
  local f=assets/shared-tools/build.sh cur new u
  cur=$(get $f rustup_version)
  new=$(fetch https://static.rust-lang.org/rustup/release-stable.toml | sed -n "s/^version = '\(.*\)'/\1/p")
  report rustup "$cur" "$new" || return 0
  u="https://static.rust-lang.org/rustup/archive/$new"
  pin3 $f rustup "$new" "$(fetch "$u/aarch64-unknown-linux-gnu/rustup-init.sha256" | cut -c1-64)" \
    "$(fetch "$u/x86_64-unknown-linux-gnu/rustup-init.sha256" | cut -c1-64)"
  set_var $f rustup_sha256_arm64_musl "$(fetch "$u/aarch64-unknown-linux-musl/rustup-init.sha256" | cut -c1-64)"
  set_var $f rustup_sha256_amd64_musl "$(fetch "$u/x86_64-unknown-linux-musl/rustup-init.sha256" | cut -c1-64)"
}

bump_eza() {
  local f=assets/features/cli-tools.sh cur new u
  cur=$(get $f eza_version); new=$(gh_latest eza-community/eza); new=${new#v}
  report eza "$cur" "$new" || return 0
  echo "            (checksums computed from the downloads)"
  u="https://github.com/eza-community/eza/releases/download/v$new"
  pin3 $f eza "$new" "$(sha_of "$u/eza_aarch64-unknown-linux-gnu.tar.gz")" "$(sha_of "$u/eza_x86_64-unknown-linux-musl.tar.gz")"
}

bump_tealdeer() {   # publishes a .sha256 next to each binary
  local f=assets/features/cli-tools.sh cur new u
  cur=$(get $f tealdeer_version); new=$(gh_latest tealdeer-rs/tealdeer); new=${new#v}
  report tealdeer "$cur" "$new" || return 0
  u="https://github.com/tealdeer-rs/tealdeer/releases/download/v$new"
  pin3 $f tealdeer "$new" "$(fetch "$u/tealdeer-linux-aarch64-musl.sha256" | cut -c1-64)" \
    "$(fetch "$u/tealdeer-linux-x86_64-musl.sha256" | cut -c1-64)"
}

# crane (go-containerregistry), which --from pulls images with, from the release's checksums.txt.
bump_crane() {
  local f=assets/oci/build.sh cur new sums
  cur=$(get $f crane_version); new=$(gh_latest google/go-containerregistry); new=${new#v}
  report crane "$cur" "$new" || return 0
  sums=$(fetch "https://github.com/google/go-containerregistry/releases/download/v$new/checksums.txt")
  pin3 $f crane "$new" "$(awk '$2 == "go-containerregistry_Linux_arm64.tar.gz" { print $1 }' <<<"$sums")" \
    "$(awk '$2 == "go-containerregistry_Linux_x86_64.tar.gz" { print $1 }' <<<"$sums")"
}

bump_omb() {
  local f=assets/features/omb.sh cur new
  cur=$(get $f omb_commit); new=$(gh_head ohmybash/oh-my-bash)
  report omb "${cur:0:12}" "${new:0:12}" || return 0
  set_var $f omb_commit "$new"
}

# lock_version LOCK TOOL: a tool's version in a mise lock, "" for none.
lock_version() {
  awk -v t="[[tools.$2]]" '$0 == t { f = 1; next } f && /^version = / { gsub(/"/, "", $3); print $3; exit }' "$1" 2>/dev/null || true
}
# lock_checksum LOCK TOOL PLATFORM: the tool's download for PLATFORM has a SHA-256 in the lock, or
# for dotnet the SHA-512 Microsoft publishes.
lock_checksum() {
  awk -v h="[tools.$2.\"platforms.$3\"]" '
    $0 == h { f = 1; next }
    f { ok = ($0 ~ /^checksum = "sha256:[0-9a-f]+"$/ && length($0) == 84) || ($0 ~ /^checksum = "sha512:[0-9a-f]+"$/ && length($0) == 148); exit }
    END { exit !ok }' "$1"
}
# dotnet_downloads LOCK PLATFORM...: the lock's dotnet entry, which mise locks without a download,
# gets Microsoft's SDK archive for each platform and its SHA-512, from the channel's releases.json,
# as devvm adds them when it resolves the lock (internal/tools/dotnet.go). Exits when Microsoft has
# none for a platform yet.
dotnet_downloads() {
  local lock=$1 v ch p rid url hash tables=""
  shift
  v=$(lock_version "$lock" dotnet)
  [[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { printf 'bump: mise locked dotnet at an unexpected version: %q\n' "$v" >&2; exit 1; }
  ch=${v%.*}
  fetch -o "$tmp/dotnet-releases.json" "https://builds.dotnet.microsoft.com/dotnet/release-metadata/$ch/releases.json"
  for p in "$@"; do
    case $p in *-musl) rid=${p%-musl}; rid=linux-musl-${rid#linux-} ;; *) rid=$p ;; esac
    read -r url hash < <(jq -r --arg v "$v" --arg rid "$rid" '[.releases[] | (.sdk // empty), (.sdks // [])[]
      | select(.version == $v) | .files[] | select(.rid == $rid and .name == "dotnet-sdk-\($rid).tar.gz")][0]
      | "\(.url) \(.hash)"' "$tmp/dotnet-releases.json")
    hash=$(tr 'A-F' 'a-f' <<<"$hash")
    if [[ $url != https://builds.dotnet.microsoft.com/dotnet/*.tar.gz || ! $hash =~ ^[0-9a-f]{128}$ ]]; then
      echo "bump: dotnet $v has no SDK archive with a SHA-512 for $p yet: try again later" >&2; exit 1
    fi
    tables+=$'\n'"[tools.dotnet.\"platforms.$p\"]"$'\n'"checksum = \"sha512:$hash\""$'\n'"url = \"$url\""$'\n'
  done
  T=$tables awk '{ print } $0 == "[[tools.dotnet]]" { f = 1 } f && /^specifiers = / { printf "%s", ENVIRON["T"]; f = 0 }' \
    "$lock" > "$tmp/dotnet.lock"
  cat "$tmp/dotnet.lock" > "$lock"
}

# The shared tools' versions every e2e section but tools uses (tests/e2e-mise.lock): upstream's newest,
# resolved as devvm resolves them (its pinned host mise, mise lock on assets/shared-tools/mise.toml), for
# both architectures' glibc and musl VMs, with dotnet's downloads added as devvm adds them. Every tool
# must be in it, and each one's downloads but rust's (rustup checks its own) need a SHA-256 (dotnet's a
# SHA-512) on every platform, or this exits: a release without its files
# yet, to try again later. Its first line is the SHA-256 of the mise.toml it was resolved from, which
# e2e checks before it runs.
e2e_lock=""
bump_e2e-tools() {
  local g=internal/tools/mise.go f=tests/e2e-mise.lock d os a v sum p tool cur new want=false
  local platforms="linux-arm64 linux-arm64-musl linux-x64 linux-x64-musl"
  v=$(get_const $g miseVersion)
  case $(uname -s) in Darwin) os=macos ;; Linux) os=linux ;; *) echo "bump: devvm has no host mise for $(uname -s)" >&2; exit 1 ;; esac
  case $(uname -m) in arm64|aarch64) a=arm64 ;; *) a=x64 ;; esac
  sum=$(sed -n "s/^[[:space:]]*\"$os-$a\": *\"\([0-9a-f]\{64\}\)\",\$/\1/p" "$root/$g")
  valid mise_sha256 "$sum"
  fetch -o "$tmp/mise" "https://github.com/jdx/mise/releases/download/v$v/mise-v$v-$os-$a"
  [[ $(shasum -a 256 "$tmp/mise" | cut -c1-64) == "$sum" ]] ||
    { echo "bump: devvm's host mise $v doesn't match its pinned SHA-256" >&2; exit 1; }
  chmod 700 "$tmp/mise"
  mkdir -p "$tmp/e2e-tools/config"; d=$(cd "$tmp/e2e-tools" && pwd -P)
  cp "$root/assets/shared-tools/mise.toml" "$d/mise.toml"
  if ! (cd "$d" && MISE_DATA_DIR=$d/data MISE_CACHE_DIR=$d/cache MISE_STATE_DIR=$d/state MISE_CONFIG_DIR=$d/config \
      MISE_GLOBAL_CONFIG_FILE=$d/config/none.toml MISE_TRUSTED_CONFIG_PATHS=$d MISE_CEILING_PATHS=$(dirname "$d") \
      "$tmp/mise" lock --platform "$(tr ' ' ',' <<<"$platforms")") >"$tmp/mise-lock.log" 2>&1; then
    cat "$tmp/mise-lock.log" >&2; echo "bump: mise lock failed" >&2; exit 1
  fi
  # shellcheck disable=SC2086  # the platforms, a word each
  dotnet_downloads "$d/mise.lock" $platforms
  while read -r tool; do
    new=$(lock_version "$d/mise.lock" "$tool")
    valid "${tool}_version" "$new"
    if [[ $tool != rust ]]; then
      for p in $platforms; do
        lock_checksum "$d/mise.lock" "$tool" "$p" || { echo "bump: $tool $new has no checksummed download for $p yet: try again later" >&2; exit 1; }
      done
    fi
    cur=$(lock_version "$root/$f" "$tool")
    if report "e2e $tool" "${cur:-none}" "$new"; then want=true; fi
  done < <(awk -F' *= *' '/^\[tools\]/ { s = 1; next } /^\[/ { s = 0 } s && NF == 2 { print $1 }' "$d/mise.toml")
  sum=$(shasum -a 256 "$d/mise.toml" | cut -c1-64)
  if report "e2e lock" "$(sed -n '1s/^# devvm e2e: shared-tools\/mise.toml sha256:\(.\{12\}\).*/\1/p' "$root/$f" 2>/dev/null)" "${sum:0:12}"; then
    want=true
  fi
  $want || return 0
  { echo "# devvm e2e: shared-tools/mise.toml sha256:$sum"; cat "$d/mise.lock"; } > "$tmp/e2e-mise.lock"
  e2e_lock=$tmp/e2e-mise.lock
}

# hub_index REPO TAG FILE: Docker Hub's index (manifest list) of library/REPO:TAG into FILE, as the
# registry sent it, so its SHA-256 is its digest. The anonymous pull token goes to curl as a header
# it reads from a file, never in a process's arguments.
hub_index() {
  printf 'Authorization: Bearer %s\n' \
    "$(fetch "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/$1:pull" | jq -r .token)" > "$tmp/hub-auth"
  fetch -H @"$tmp/hub-auth" -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://registry-1.docker.io/v2/library/$1/manifests/$2" > "$3"
  jq -e '.manifests | length > 0' "$3" >/dev/null || { echo "bump: Docker Hub sent no index for $1:$2" >&2; exit 1; }
}

# The container images e2e's --from checks start from (tests/e2e/from_test.go's from*Digest), by the
# digest of their index on Docker Hub, so the bases made from them stay cached until these pins move.
bump_e2e-from() {
  local f=tests/e2e/from_test.go p ref var new
  for p in node:24=fromNodeDigest alpine:3=fromAlpineDigest ubuntu:26.04=fromUbuntuDigest; do
    ref=${p%%=*} var=${p#*=}
    hub_index "${ref%%:*}" "${ref#*:}" "$tmp/e2e-from-index"
    new=sha256:$(shasum -a 256 "$tmp/e2e-from-index" | cut -c1-64)
    if report "$ref" "$(get_const $f "$var" | cut -c8-19)" "${new:7:12}"; then set_var $f "$var" "$new"; fi
  done
}

for t in $tools; do "bump_$t"; done
write_staged
[[ -z $e2e_lock ]] || cat "$e2e_lock" > "$root/tests/e2e-mise.lock"

$check && exit 0
if (( ${#summary[@]} == 0 )); then echo "==> everything is up to date"; exit 0; fi
echo
echo "Bump pinned versions"
echo
printf -- '- %s\n' "${summary[@]}"
echo
echo "==> the edited images and tools rebuild on next use. Test: devvm build <image>; then devvm gc"
