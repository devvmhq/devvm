#!/bin/bash
# Update every version and checksum pinned in the repo: the downloads in assets/ and devvm's host mise,
# the end-to-end tests' own pins, Go and its modules, the dev tools in mise.toml, the workflows' actions
# and the docs site's theme and Mermaid.
#   scripts/bump.sh --check     show current and latest for every pin, by group, with whether a bump is
#                               a patch, minor or major; change nothing
#   scripts/bump.sh             apply every safe bump: patches and minors, never a major or a held pin,
#                               nor the tools applied only when named; print a summary for the commit
#   scripts/bump.sh tool...     update the given tools; naming an images or e2e tool applies its major too
# Tools, by group:
#   images: docker nix direnv nix-direnv mise git-lfs rustup crane omb eza tealdeer
#     mise: base's, and the one devvm runs on the host to resolve the shared tools' versions
#   shared-tools: cursor (only when named), the one shared tool pinned for users
#     (assets/shared-tools/mise.toml): devvm's login and proxy code was read against its pinned release
#   e2e: e2e-tools e2e-from e2e-curl e2e-ubuntu e2e-lima
#     e2e-tools: the shared tools' versions every e2e section but tools uses; it applies majors, as
#       users get them too (assets/shared-tools/mise.toml holds what's held)
#     e2e-from: the container images e2e's --from checks start from, by digest
#     e2e-curl: the curl image e2e's network checks run in containers, its newest release by digest
#     e2e-ubuntu: the dated release of Ubuntu's cloud image e2e pins base to
#     e2e-lima: the Lima release CI's e2e workflow installs, and its tarball's checksum
#   go: go go-minor go-mods deadcode
#     go: Go's newest patch release (go.mod's and docs/go.mod's go lines, mise.toml and mise.lock)
#     go-minor (only when named): Go's newest minor release, the same files
#     go-mods: go.mod's direct modules (the depguard allowlist), patches and minors, with go get and
#       go mod tidy; a module's next major, a new path, is only reported; the indirect ones move as
#       go mod tidy needs them
#     deadcode: mise.toml's deadcode task, golang.org/x/tools' newest release
#   dev-tools: mise.toml's tools but go, through mise lock, so mise.lock keeps their checksums
#   ci: ci-mise actions
#     ci-mise: the mise the workflows install (jdx/mise-action's version), base's mise
#     actions: every workflow's uses:, by its release's tag's commit, then actionlint and zizmor
#   docs: hextra mermaid
#     hextra: the docs site's theme, docs/go.mod
#     mermaid (only when named): docs/hugo.yaml's Mermaid; its diagrams need a look after a bump
# Holds: a pin that never moves past a version. Go modules and actions take Dependabot's ignore
# list (.github/dependabot.yml: pflag stays below 1.1.0, whose help prints every boolean flag with
# [=true|false]); the rest are holds below.
holds="node 25" # NAME BOUND...: node stays on its major, 24, which is an LTS release
# The shared tools themselves (node, claude, ...) and Ubuntu's cloud image aren't pinned for users:
# devvm resolves them per user. Only e2e pins them, so its cached images stay valid between runs.
# Left as they are, on purpose:
#   install.sh's Lima: the newest release, checked against its SHA256SUMS when it's installed
#   packaging/homebrew/devvm.rb: release.sh fills in each release's URLs and checksums
#   testdata/mise.lock and testdata/v<release>/: fixed for the tests, and frozen
#   skills-lock.json: the vendored security-audit skill's hash, from the skills tool
#   docs/static's fonts and docs/assets/icons: files copied in, with their versions in comments
#   docs/layouts' copies of Hextra's files (the hero button, Mermaid's partial): checked by hand
#     against Hextra's after a hextra bump
#   assets/layers/ubuntu-*.gpg and internal/ubuntu's Series: keys and a release, moved by hand
#     (contributing/maintaining.md)
#   tests/e2e/tools_test.go's cowsay@1.6.0: any version works, it only installs one
# Pins: each pin internal/structure's TestBumpCoversPins finds (assets/'s <tool>_version, _commit and
# _sha256 lines and its mise.toml's versions, mise.toml, the go.mod files, the workflows' uses: and
# version values, tests/e2e-mise.lock, tests/e2e's pinned consts and docs/hugo.yaml), as KEY and the
# tool that moves it, or "-" and why it's left. A KEY ending in * is every key it starts. A pin
# found and not here fails the test, and so does a line here that finds nothing.
# pin assets/features/cli-tools.sh:eza eza
# pin assets/features/cli-tools.sh:tealdeer tealdeer
# pin assets/features/omb.sh:omb omb
# pin assets/layers/base.sh:mise mise
# pin assets/layers/base.sh:git_lfs git-lfs
# pin assets/layers/docker.sh:docker docker
# pin assets/layers/docker.sh:containerd docker
# pin assets/layers/docker.sh:buildx docker
# pin assets/layers/docker.sh:compose docker
# pin assets/layers/docker.sh:model docker
# pin assets/layers/docker.sh:docker_gpg docker
# pin assets/layers/nix.sh:nix nix
# pin assets/layers/nix.sh:direnv direnv
# pin assets/layers/nix.sh:nix_direnv nix-direnv
# pin assets/oci/build.sh:crane crane
# pin assets/shared-tools/build.sh:rustup rustup
# pin assets/shared-tools/mise.toml:cursor cursor
# pin go.mod:go go
# pin go.mod:require:* go-mods
# pin go.mod:indirect:* - moved by go mod tidy, as the direct modules need them
# pin docs/go.mod:go go
# pin docs/go.mod:indirect:github.com/imfing/hextra hextra
# pin mise.toml:tool:go go
# pin mise.toml:tool:* dev-tools
# pin mise.toml:run:golang.org/x/tools/cmd/deadcode deadcode
# pin .github/workflows:uses:* actions
# pin .github/workflows:version ci-mise
# pin .github/workflows:LIMA_VERSION e2e-lima
# pin .github/workflows:LIMA_SHA256 e2e-lima
# pin tests/e2e-mise.lock:* e2e-tools
# pin tests/e2e:e2eUbuntuRelease e2e-ubuntu
# pin tests/e2e:fromNodeDigest e2e-from
# pin tests/e2e:fromAlpineDigest e2e-from
# pin tests/e2e:fromUbuntuDigest e2e-from
# pin tests/e2e:curlImage e2e-curl
# pin tests/e2e:dotnetOther - an SDK other than the shared one for a global.json to ask for: any older one works
# pin docs/hugo.yaml:mermaid mermaid
# pin docs/hugo.yaml:min - Hextra's oldest Hugo, which Hugo checks; mise.toml pins the Hugo that builds the site
# Needs curl and jq, and per group mise (dev-tools, go, e2e lock checks), go (go-mods, hextra when
# applied) and gh (actions). Checksums come from upstream's published sums where there are any;
# otherwise they're computed from the download (said so in the output). Edited scripts change image
# keys, so the affected images rebuild on next use: test with `devvm build <image>`, then `devvm gc`.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
every="docker nix direnv nix-direnv mise git-lfs rustup crane omb eza tealdeer cursor e2e-tools e2e-from e2e-curl e2e-ubuntu e2e-lima go go-minor go-mods deadcode dev-tools ci-mise actions hextra mermaid"
# Applied only when named: --check reports them.
named_only="cursor go-minor mermaid"
# Naming one of these applies its major too.
legacy="docker nix direnv nix-direnv mise git-lfs rustup crane omb eza tealdeer e2e-tools e2e-from e2e-curl e2e-ubuntu e2e-lima"

check=false named=false
if [[ ${1:-} == --check ]]; then check=true; shift; fi
if (( $# > 0 )); then
  named=true tools=$*
elif $check; then
  tools=$every
else
  tools=""
  for t in $every; do [[ " $named_only " == *" $t "* ]] || tools+="$t "; done
fi
for t in $tools; do [[ " $every " == *" $t "* ]] || { echo "bump: unknown tool '$t' (have: $every)" >&2; exit 1; }; done
for c in curl jq; do command -v "$c" >/dev/null || { echo "bump: needs $c" >&2; exit 1; }; done
# need CMD: the current tool needs CMD.
need() { command -v "$1" >/dev/null || { echo "bump: $cur_tool needs $1" >&2; exit 1; }; }

summary=() notes=()
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
    *Image) re='^[a-z0-9][a-z0-9./-]*:[0-9A-Za-z][0-9A-Za-z._-]*@sha256:[0-9a-f]{64}$' ;;
    *_module) re='^[A-Za-z0-9][A-Za-z0-9._~/-]*$' ;;
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
  # set_text's edits: a whole line, or every occurrence of a text.
  for (( i = 0; i < ${#texts[@]}; i += 4 )); do
    M=${texts[i + 1]} O=${texts[i + 2]} V=${texts[i + 3]} awk '
      ENVIRON["M"] == "line" { if ($0 == ENVIRON["O"]) print ENVIRON["V"]; else print; next }
      { o = ENVIRON["O"]; out = ""; s = $0
        while ((j = index(s, o)) > 0) { out = out substr(s, 1, j - 1) ENVIRON["V"]; s = substr(s, j + length(o)) }
        print out s }' "$root/${texts[i]}" > "$tmp/edit"
    cat "$tmp/edit" > "$root/${texts[i]}"
  done
}
# set_text FILE line|text OLD NEW: stages NEW in place of the line OLD, or of each OLD in the file's
# lines. The caller checks NEW's values with valid first.
texts=()
set_text() { texts+=("$1" "$2" "$3" "$4"); }
# staged_value FILE VAR: VAR's value as this run stages it, else as FILE has it.
staged_value() {
  local i v
  v=$(get "$1" "$2")
  for (( i = 0; i < ${#staged[@]}; i += 3 )); do
    [[ ${staged[i]} != "$1" || ${staged[i + 1]} != "$2" ]] || v=${staged[i + 2]}
  done
  echo "$v"
}

# bump_kind CURRENT NEW: patch, minor or major for two versions (a leading v aside); calendar for a
# new year of a version that starts with one (mise's 2026.11.8), which is safe; else change.
bump_kind() {
  local a=${1#v} b=${2#v} re='^[0-9]+(\.[0-9]+)*$' a1 b1
  [[ $a =~ $re && $b =~ $re ]] || { echo change; return; }
  a1=${a%%.*} b1=${b%%.*}
  if [[ $a1 != "$b1" ]]; then
    if (( a1 >= 1000 )); then echo calendar; else echo major; fi
    return
  fi
  a=${a#"$a1"}; a=${a#.}; b=${b#"$b1"}; b=${b#.}
  if [[ ${a%%.*} == "${b%%.*}" ]]; then echo patch; else echo minor; fi
}

# report TOOL CURRENT LATEST [KIND]: print it, with the bump's kind (else bump_kind's); true when an
# update is wanted: not --check, different, and neither a major the run doesn't apply (majors_ok)
# nor a tool applied only when named that wasn't.
report() {
  if [[ $2 == "$3" ]]; then printf '  %-9s %-14s up to date\n' "$1" "$2"; return 1; fi
  # The latest comes from upstream: no control characters on the terminal.
  local shown kind note=""
  shown=$(LC_ALL=C tr -d '\000-\037\177' <<<"$3")
  # Upstream's latest can lag behind a pin (a release marker not yet moved): an older version is
  # never offered.
  local ver='^v?[0-9]+(\.[0-9]+)*$'
  if [[ $2 =~ $ver && $3 =~ $ver && $(printf '%s\n%s\n' "${2#v}" "${3#v}" | sort -V | head -1) == "${3#v}" ]]; then
    printf "  %-9s %-14s newer than upstream's latest, %s\n" "$1" "$2" "$shown"; return 1
  fi
  kind=${4:-$(bump_kind "$2" "$3")}
  if [[ $kind == major ]] && ! $majors_ok; then
    if [[ " $legacy " == *" $cur_tool "* ]]; then note=", applied when named: scripts/bump.sh $cur_tool"; else note=", not applied: by hand"; fi
  elif $only_named; then
    note=", applied only when named: scripts/bump.sh $cur_tool"
  fi
  printf '  %-9s %-14s -> %s (%s%s)\n' "$1" "$2" "$shown" "$kind" "$note"
  $check && return 1
  [[ -z $note ]] || return 1
  summary+=("$1: $2 -> $3")
}

# held NAME VERSION: VERSION is at or past a hold on NAME (holds, then .github/dependabot.yml's
# ignore list); prints the bound.
held() {
  local b
  # shellcheck disable=SC2086  # holds: NAME BOUND pairs, a word each
  b=$( { printf '%s %s\n' $holds
    awk '/- dependency-name:/ { n = $3 } n && /versions: \[">= / { v = $0; sub(/.*">= /, "", v); sub(/".*/, "", v); print n, v; n = "" }' \
      "$root/.github/dependabot.yml"; } | awk -v n="$1" '$1 == n { print $2; exit }')
  [[ -n $b && $(printf '%s\n%s\n' "${b#v}" "${2#v}" | sort -V | head -1) == "${b#v}" ]] || return 1
  echo "$b"
}
# say_held NAME CURRENT BOUND NEWEST: the line for a newest version that a hold keeps out.
say_held() {
  printf '  %-9s %-14s held below %s (newest %s)\n' "$1" "$2" "$3" "$(LC_ALL=C tr -d '\000-\037\177' <<<"$4")"
}

# newest_of [MAJOR]: the newest of the versions on stdin (a leading v kept), only release versions,
# within MAJOR when given; nothing for none.
newest_of() {
  { grep -E '^v?[0-9]+(\.[0-9]+)*$' || true; } | awk -v m="${1:-}" '{ v = $0; sub(/^v/, "", v); split(v, p, ".") } m == "" || p[1] == m' |
    sed 's/^v\(.*\)/\1 v/' | sort -V | tail -1 | awk '{ print $2 $1 }'
}

# choose NAME CURRENT NEWEST WITHIN: report the bump to NEWEST, or when it's a major the run doesn't
# apply or a held one, to WITHIN (the newest that is neither) and NEWEST's line beside it. Sets
# target to the version to move to; true when there's one.
choose() {
  local b
  target=""
  if b=$(held "$1" "$3"); then
    say_held "$1" "$2" "$b" "$3"
  elif [[ $(bump_kind "$2" "$3") == major ]] && ! $majors_ok; then
    report "$1" "$2" "$3" || true
  else
    if report "$1" "$2" "$3"; then target=$3; fi
    [[ -n $target ]]; return
  fi
  if [[ -n $4 && $4 != "$2" ]] && report "$1" "$2" "$4"; then target=$4; fi
  [[ -n $target ]]
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
  if report e2e-ubuntu "$(get_const $f e2eUbuntuRelease)" "$new" release; then set_var $f e2eUbuntuRelease "$new" e2e_ubuntu_release; fi
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
  if report docker-gpg "$(get $f docker_gpg_sha256 | cut -c1-12)" "$(cut -c1-12 <<<"$sha")" checksum; then
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
  valid nix_version "$new"
  u="https://releases.nixos.org/nix/nix-$new/nix-$new"
  pin3 $f nix "$new" "$(fetch "$u-aarch64-linux.tar.xz.sha256" | cut -c1-64)" "$(fetch "$u-x86_64-linux.tar.xz.sha256" | cut -c1-64)"
}

bump_direnv() {
  local f=assets/layers/nix.sh cur new u
  cur=$(get $f direnv_version); new=$(gh_latest direnv/direnv); new=${new#v}
  report direnv "$cur" "$new" || return 0
  valid direnv_version "$new"
  echo "            (checksums computed from the downloads)"
  u="https://github.com/direnv/direnv/releases/download/v$new"
  pin3 $f direnv "$new" "$(sha_of "$u/direnv.linux-arm64")" "$(sha_of "$u/direnv.linux-amd64")"
}

# nix-direnv: one script (direnvrc) at the release's tag.
bump_nix-direnv() {
  local f=assets/layers/nix.sh cur new
  cur=$(get $f nix_direnv_version); new=$(gh_latest nix-community/nix-direnv); new=${new#v}
  report nix-direnv "$cur" "$new" || return 0
  valid nix_direnv_version "$new"
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
  valid mise_version "$new"
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
  valid rustup_version "$new"
  u="https://static.rust-lang.org/rustup/archive/$new"
  pin3 $f rustup "$new" "$(fetch "$u/aarch64-unknown-linux-gnu/rustup-init.sha256" | cut -c1-64)" \
    "$(fetch "$u/x86_64-unknown-linux-gnu/rustup-init.sha256" | cut -c1-64)"
  set_var $f rustup_sha256_arm64_musl "$(fetch "$u/aarch64-unknown-linux-musl/rustup-init.sha256" | cut -c1-64)"
  set_var $f rustup_sha256_amd64_musl "$(fetch "$u/x86_64-unknown-linux-musl/rustup-init.sha256" | cut -c1-64)"
}

# git-lfs in base, from the release's published sha256sums.asc (its checksums; its signature isn't
# checked here: verify it with the Git LFS team's keys when bumping by hand).
bump_git-lfs() {
  local f=assets/layers/base.sh cur new sums
  cur=$(get $f git_lfs_version); new=$(gh_latest git-lfs/git-lfs); new=${new#v}
  report git-lfs "$cur" "$new" || return 0
  valid git_lfs_version "$new"
  sums=$(fetch "https://github.com/git-lfs/git-lfs/releases/download/v$new/sha256sums.asc")
  pin3 $f git_lfs "$new" "$(awk -v n="*git-lfs-linux-arm64-v$new.tar.gz" '$2 == n { print $1 }' <<<"$sums")" \
    "$(awk -v n="*git-lfs-linux-amd64-v$new.tar.gz" '$2 == n { print $1 }' <<<"$sums")"
}

bump_eza() {
  local f=assets/features/cli-tools.sh cur new u
  cur=$(get $f eza_version); new=$(gh_latest eza-community/eza); new=${new#v}
  report eza "$cur" "$new" || return 0
  valid eza_version "$new"
  echo "            (checksums computed from the downloads)"
  u="https://github.com/eza-community/eza/releases/download/v$new"
  pin3 $f eza "$new" "$(sha_of "$u/eza_aarch64-unknown-linux-gnu.tar.gz")" "$(sha_of "$u/eza_x86_64-unknown-linux-musl.tar.gz")"
}

bump_tealdeer() {   # publishes a .sha256 next to each binary
  local f=assets/features/cli-tools.sh cur new u
  cur=$(get $f tealdeer_version); new=$(gh_latest tealdeer-rs/tealdeer); new=${new#v}
  report tealdeer "$cur" "$new" || return 0
  valid tealdeer_version "$new"
  u="https://github.com/tealdeer-rs/tealdeer/releases/download/v$new"
  pin3 $f tealdeer "$new" "$(fetch "$u/tealdeer-linux-aarch64-musl.sha256" | cut -c1-64)" \
    "$(fetch "$u/tealdeer-linux-x86_64-musl.sha256" | cut -c1-64)"
}

# crane (go-containerregistry), which --from pulls images with, from the release's checksums.txt.
bump_crane() {
  local f=assets/oci/build.sh cur new sums
  cur=$(get $f crane_version); new=$(gh_latest google/go-containerregistry); new=${new#v}
  report crane "$cur" "$new" || return 0
  valid crane_version "$new"
  sums=$(fetch "https://github.com/google/go-containerregistry/releases/download/v$new/checksums.txt")
  pin3 $f crane "$new" "$(awk '$2 == "go-containerregistry_Linux_arm64.tar.gz" { print $1 }' <<<"$sums")" \
    "$(awk '$2 == "go-containerregistry_Linux_x86_64.tar.gz" { print $1 }' <<<"$sums")"
}

bump_omb() {
  local f=assets/features/omb.sh cur new
  cur=$(get $f omb_commit); new=$(gh_head ohmybash/oh-my-bash)
  report omb "${cur:0:12}" "${new:0:12}" commit || return 0
  valid omb_commit "$new"
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
  if report "e2e lock" "$(sed -n '1s/^# devvm e2e: shared-tools\/mise.toml sha256:\(.\{12\}\).*/\1/p' "$root/$f" 2>/dev/null)" "${sum:0:12}" checksum; then
    want=true
  fi
  $want || return 0
  { echo "# devvm e2e: shared-tools/mise.toml sha256:$sum"; cat "$d/mise.lock"; } > "$tmp/e2e-mise.lock"
  e2e_lock=$tmp/e2e-mise.lock
}

# hub_index REPO TAG FILE: Docker Hub's index (manifest list) of REPO:TAG (library/node for node) into
# FILE, as the registry sent it, so its SHA-256 is its digest. The anonymous pull token goes to curl
# as a header it reads from a file, never in a process's arguments.
hub_index() {
  printf 'Authorization: Bearer %s\n' \
    "$(fetch "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$1:pull" | jq -r .token)" > "$tmp/hub-auth"
  fetch -H @"$tmp/hub-auth" -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://registry-1.docker.io/v2/$1/manifests/$2" > "$3"
  jq -e '.manifests | length > 0' "$3" >/dev/null || { echo "bump: Docker Hub sent no index for $1:$2" >&2; exit 1; }
}

# The container images e2e's --from checks start from (tests/e2e/from_test.go's from*Digest), by the
# digest of their index on Docker Hub, so the bases made from them stay cached until these pins move.
bump_e2e-from() {
  local f=tests/e2e/from_test.go p ref var new
  for p in node:24=fromNodeDigest alpine:3=fromAlpineDigest ubuntu:26.04=fromUbuntuDigest; do
    ref=${p%%=*} var=${p#*=}
    hub_index "library/${ref%%:*}" "${ref#*:}" "$tmp/e2e-from-index"
    new=sha256:$(shasum -a 256 "$tmp/e2e-from-index" | cut -c1-64)
    if report "$ref" "$(get_const $f "$var" | cut -c8-19)" "${new:7:12}" digest; then set_var $f "$var" "$new"; fi
  done
}

# The curl image e2e's network checks run in containers (tests/e2e/transparent_test.go's curlImage):
# curlimages/curl's newest release tag, by its index's digest.
bump_e2e-curl() {
  local f=tests/e2e/transparent_test.go repo=curlimages/curl cur new
  cur=$(get_const $f curlImage); cur=${cur#"$repo":}; cur=${cur%%@*}
  new=$(fetch "https://hub.docker.com/v2/repositories/$repo/tags?page_size=100&ordering=last_updated" | jq -r '.results[].name' | newest_of)
  report e2e-curl "$cur" "$new" || return 0
  valid curl_version "$new"
  hub_index "$repo" "$new" "$tmp/e2e-curl-index"
  set_var $f curlImage "$repo:$new@sha256:$(shasum -a 256 "$tmp/e2e-curl-index" | cut -c1-64)"
}

# Cursor's CLI, the one shared tool pinned for users (assets/shared-tools/mise.toml): the release
# Cursor's install script fetches, with each platform's SHA-256 computed from its download, as Cursor
# publishes none. Only when named: devvm's login and proxy code was read against the pinned release.
bump_cursor() {
  local f=assets/shared-tools/mise.toml cur new p old sum u
  cur=$(sed -n 's/^cursor = { version = "\([^"]*\)".*/\1/p' "$root/$f")
  new=$(fetch https://cursor.com/install |
    sed -n 's|^DOWNLOAD_URL="https://downloads\.cursor\.com/lab/\([0-9A-Za-z.-]*\)/\${OS}/\${ARCH}/agent-cli-package\.tar\.gz"$|\1|p' | head -1)
  [[ -n $new ]] || { echo "bump: cursor.com/install names no package: see assets/shared-tools/mise.toml's cursor" >&2; exit 1; }
  report cursor "$cur" "$new" release || return 0
  valid cursor_version "$new"
  echo "            (checksums computed from the downloads)"
  for p in x64 arm64; do
    old=$(grep '^cursor = ' "$root/$f" | grep -o "linux-$p = { url = \"[^\"]*\", checksum = \"sha256:[0-9a-f]*\"" | sed 's/.*sha256://; s/"$//')
    u="https://downloads.cursor.com/lab/$new/linux/$p/agent-cli-package.tar.gz"
    sum=$(sha_of "$u"); valid cursor_sha256 "$sum"
    set_text $f text "sha256:$old" "sha256:$sum"
  done
  set_text $f text "$cur" "$new"
  notes+=("cursor: read the new CLI where devvm's code names its release (internal/agentlogin/cursor.go, internal/proxy/cursor.go, internal/agents/agents.go), then run scripts/bump.sh e2e-tools")
}

# go_releases: Go's stable releases, oldest first.
go_releases() {
  [[ -s $tmp/go.json ]] || fetch 'https://go.dev/dl/?mode=json&include=all' > "$tmp/go.json"
  jq -r '.[] | select(.stable) | .version | ltrimstr("go")' "$tmp/go.json" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V
}
# go_to NAME NEW: move Go to NEW: go.mod's and docs/go.mod's go lines, mise.toml's go, then mise.lock.
go_to() {
  local cur
  cur=$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$root/go.mod")
  report "$1" "$cur" "$2" || return 0
  valid go_version "$2"
  set_text go.mod line "go $cur" "go $2"
  set_text docs/go.mod line "go $(sed -n 's/^go \([0-9.]*\)$/\1/p' "$root/docs/go.mod")" "go $2"
  set_text mise.toml line "go = \"$(mise_spec go)\"" "go = \"$2\""
  mise_locks+=(go)
}
# Go's newest patch release of go.mod's minor.
bump_go() {
  # go-minor in the same run moves the same lines.
  if [[ " $tools " == *" go-minor "* ]] && ! $check; then return 0; fi
  local cur
  cur=$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$root/go.mod")
  go_to go "$(go_releases | awk -v m="${cur%.*}." 'index($0, m) == 1' | tail -1)"
}
# Go's newest minor release, only when named.
bump_go-minor() {
  local cur new
  cur=$(sed -n 's/^go \([0-9.]*\)$/\1/p' "$root/go.mod")
  new=$(go_releases | awk -F. -v m="${cur%%.*}" '$1 == m' | tail -1)
  # No newer minor: go reports the patches.
  [[ ${new%.*} != "${cur%.*}" ]] || new=$cur
  go_to go-minor "$new"
}

# mod_versions MODULE: the Go module proxy's release versions of MODULE, none for a module it hasn't.
mod_versions() {
  valid go_module "$1"
  # The proxy's escaped path: !a for each capital A.
  fetch "https://proxy.golang.org/$(awk '{ for (i = 1; i <= length($0); i++) { c = substr($0, i, 1)
    printf "%s", (c ~ /[A-Z]/ ? "!" tolower(c) : c) } }' <<<"$1")/@v/list" 2>/dev/null |
    grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true
}
# mod_bump NAME MODULE CURRENT: choose MODULE's newest release within its path's major, under any hold,
# and report the next major's path when the proxy has one. Sets target.
mod_bump() {
  local list newest within major base next b
  list=$(mod_versions "$2")
  newest=$(newest_of <<<"$list")
  within=$newest
  if b=$(held "$2" "$newest"); then
    within=$(awk -v b="$b" '{ print } END { print b }' <<<"$list" | sed 's/^v\(.*\)/\1 v/' | sort -V |
      awk -v b="${b#v}" '$1 == b { exit } { print $2 $1 }' | tail -1)
  fi
  choose "$1" "$3" "${newest:-$3}" "$within" || true
  major=${3#v}; major=${major%%.*}; (( major >= 1 )) || major=1
  base=$2
  [[ ! $2 =~ /v[0-9]+$ ]] || base=${2%/v*}
  next="$base/v$((major + 1))"
  newest=$(mod_versions "$next" | newest_of)
  if [[ -n $newest ]]; then printf '  %-9s %-14s -> %s %s (major, a new path: by hand)\n' "$1" "$3" "$next" "$newest"; fi
  [[ -n $target ]]
}

# go.mod's direct modules: the depguard allowlist's and the tests'.
bump_go-mods() {
  local mod cur
  while read -r mod cur; do
    if mod_bump "$mod" "$mod" "$cur"; then
      valid go_version "$target"
      go_gets+=("$mod@$target")
    fi
  done < <(awk '/^require \(/ { r = 1; next } r && /^\)/ { r = 0 } r && !/\/\/ indirect/ && NF >= 2 { print $1, $2 }
    /^require [^(]/ && !/\/\/ indirect/ { print $2, $3 }' "$root/go.mod")
}

# mise.toml's deadcode task: golang.org/x/tools' newest release.
bump_deadcode() {
  local cur
  cur=$(sed -n 's|.*golang\.org/x/tools/cmd/deadcode@\(v[0-9.]*\) .*|\1|p' "$root/mise.toml")
  mod_bump deadcode golang.org/x/tools "$cur" || return 0
  valid deadcode_version "$target"
  set_text mise.toml text "golang.org/x/tools/cmd/deadcode@$cur " "golang.org/x/tools/cmd/deadcode@$target "
}

# The docs site's theme, docs/go.mod's Hextra, through go get and hugo mod tidy in docs/ (go mod tidy
# there would drop it: the module has no Go code).
bump_hextra() {
  local mod=github.com/imfing/hextra cur
  cur=$(awk -v m="$mod" '$1 == m { print $2 } $1 == "require" && $2 == m { print $3 }' "$root/docs/go.mod")
  mod_bump hextra "$mod" "$cur" || return 0
  valid hextra_version "$target"
  docs_gets+=("$mod@$target")
  notes+=("hextra: compare docs/layouts' copies of Hextra's files (the hero button, Mermaid's partial) with $target's, and build the site: mise run docs")
}

# docs/hugo.yaml's Mermaid, npm's newest, only when named: its diagrams need a look.
bump_mermaid() {
  local cur new
  cur=$(sed -n 's|.*/npm/mermaid@\([0-9.]*\)/.*|\1|p' "$root/docs/hugo.yaml")
  new=$(fetch https://registry.npmjs.org/mermaid/latest | jq -r .version)
  report mermaid "$cur" "$new" || return 0
  valid mermaid_version "$new"
  set_text docs/hugo.yaml text "/npm/mermaid@$cur/" "/npm/mermaid@$new/"
  notes+=("mermaid: look at the diagrams on the docs site (mise run docs) before committing")
}

# mise_spec TOOL: mise.toml's version for TOOL in [tools], an inline table's version included.
mise_spec() {
  mise_tools | awk -F'\t' -v t="$1" '$1 == t { print $2 }'
}
# mise_tools: mise.toml's [tools], a TOOL<tab>VERSION line each.
mise_tools() {
  awk '/^\[tools\]/ { s = 1; next } /^\[/ { s = 0 }
    s && /^[^#]/ && / = / { k = $1; gsub(/"/, "", k)
      if (match($0, /version = "[^"]*"/)) v = substr($0, RSTART + 11, RLENGTH - 12); else { v = $3; gsub(/"/, "", v) }
      print k "\t" v }' "$root/mise.toml"
}

# mise.toml's dev tools, go aside (bump_go): mise's newest of each, a major or a held one only
# reported, then mise lock, so mise.lock keeps their checksums.
bump_dev-tools() {
  need mise
  local key spec newest within line
  while IFS=$'\t' read -r key spec; do
    [[ $key != go ]] || continue
    newest=$(mise latest "$key" 2>/dev/null) || { echo "bump: mise latest $key failed" >&2; exit 1; }
    within=$(mise latest "$key@${spec%%.*}" 2>/dev/null) || within=""
    choose "$key" "$spec" "$newest" "$within" || continue
    valid "${key##*/}_version" "$target"
    line=$(K=$key awk '/^\[tools\]/ { s = 1; next } /^\[/ { s = 0 } { k = $1; gsub(/"/, "", k) }
      s && k == ENVIRON["K"] && / = / { print; exit }' "$root/mise.toml")
    set_text mise.toml line "$line" "$(O="\"$spec\"" V="\"$target\"" awk '{ i = index($0, ENVIRON["O"])
      print substr($0, 1, i - 1) ENVIRON["V"] substr($0, i + length(ENVIRON["O"])) }' <<<"$line")"
    mise_locks+=("$key")
  done < <(mise_tools)
}

# The mise the workflows install (jdx/mise-action's version input): base's mise, as this run leaves it.
bump_ci-mise() {
  local f cur want
  want=$(staged_value assets/layers/base.sh mise_version)
  while read -r cur; do
    report ci-mise "$cur" "$want" || continue
    for f in "$root"/.github/workflows/*.yml; do set_text "${f#"$root"/}" text "version: $cur" "version: $want"; done
    workflows_changed=true
  done < <(grep -h -A4 'uses: jdx/mise-action@' "$root"/.github/workflows/*.yml | sed -n 's/^ *version: \([^ ]*\)$/\1/p' | sort -u)
}

# Every workflow's uses:, pinned by a release's tag's commit with the tag as a comment: moved to the
# newest release's (within its major, the next major only reported), the commit from gh api.
bump_actions() {
  need gh
  local repo sha tag newest within f new
  while read -r repo sha tag; do
    gh api "repos/$repo/releases?per_page=100" --jq '.[] | select((.draft or .prerelease) | not) | .tag_name' > "$tmp/tags"
    newest=$(newest_of < "$tmp/tags")
    within=$(newest_of "$(sed 's/^v//; s/\..*//' <<<"$tag")" < "$tmp/tags")
    choose "$repo" "$tag" "${newest:-$tag}" "$within" || continue
    valid action_version "$target"
    new=$(gh api "repos/$repo/commits/$target" --jq .sha)
    valid action_commit "$new"
    for f in "$root"/.github/workflows/*.yml; do
      set_text "${f#"$root"/}" text "uses: $repo@$sha # $tag" "uses: $repo@$new # $target"
    done
    workflows_changed=true
  done < <(sed -n 's/^ *\(- \)\{0,1\}uses: \([^@ ]*\)@\([0-9a-f]\{40\}\) # \([^ ]*\)$/\2 \3 \4/p' "$root"/.github/workflows/*.yml | sort -u)
}

mise_locks=() go_gets=() docs_gets=() workflows_changed=false
# run_logged STEP CMD...: run CMD in the repo, its output shown only when it fails.
run_logged() {
  local step=$1
  shift
  echo "==> $step"
  (cd "$root" && "$@") >"$tmp/step.log" 2>&1 ||
    { cat "$tmp/step.log" >&2; echo "bump: $step failed; the pins above are already edited" >&2; exit 1; }
}

group=""
for cur_tool in $tools; do
  case $cur_tool in
    cursor) g=shared-tools ;;
    e2e-*) g=e2e ;;
    go | go-minor | go-mods | deadcode) g=go ;;
    dev-tools) g=dev-tools ;;
    ci-mise | actions) g=ci ;;
    hextra | mermaid) g=docs ;;
    *) g=images ;;
  esac
  [[ $g == "$group" ]] || { group=$g; echo "[$g]"; }
  # Majors: e2e-tools takes them (assets/shared-tools/mise.toml holds what's held), a named images
  # or e2e tool too, and a tool applied only when named, when it is.
  majors_ok=false only_named=false
  if [[ $cur_tool == e2e-tools ]] || { $named && [[ " $legacy $named_only " == *" $cur_tool "* && $cur_tool != go-minor ]]; }; then
    majors_ok=true
  fi
  if ! $named && [[ " $named_only " == *" $cur_tool "* ]]; then only_named=true; fi
  "bump_$cur_tool"
done
write_staged
[[ -z $e2e_lock ]] || cat "$e2e_lock" > "$root/tests/e2e-mise.lock"
$check && exit 0

if (( ${#mise_locks[@]} > 0 )); then need mise; run_logged "mise lock ${mise_locks[*]}" mise lock "${mise_locks[@]}"; fi
if (( ${#go_gets[@]} > 0 )); then
  need go
  run_logged "go get ${go_gets[*]}" go get "${go_gets[@]}"
  run_logged "go mod tidy" go mod tidy
fi
if (( ${#docs_gets[@]} > 0 )); then
  need go; need mise
  run_logged "go get ${docs_gets[*]} (docs)" sh -c 'cd docs && exec go get "$@"' sh "${docs_gets[@]}"
  # go.sum keeps the old version's lines; Hugo's tidy, not Go's, which would drop the module.
  run_logged "hugo mod tidy (docs)" sh -c 'cd docs && exec mise exec -- hugo mod tidy'
fi
if $workflows_changed; then
  need mise
  run_logged actionlint mise exec -- actionlint
  run_logged "zizmor --offline ." mise exec -- zizmor --offline .
fi

if (( ${#summary[@]} == 0 )); then echo "==> everything is up to date"; exit 0; fi
echo
echo "Bump pinned versions"
echo
printf -- '- %s\n' "${summary[@]}"
echo
(( ${#notes[@]} == 0 )) || { printf -- '==> %s\n' "${notes[@]}"; echo; }
echo "==> the edited images and tools rebuild on next use. Test: devvm build <image>; then devvm gc"
