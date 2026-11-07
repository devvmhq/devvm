#!/bin/bash
# Install or upgrade devvm, then offer to install what it needs (Lima; on Linux, QEMU and KVM access).
#   curl -fsSL https://github.com/devvmhq/devvm/releases/latest/download/install.sh | bash
#   ... | bash -s -- --yes            answer yes to every question
#   ... | bash -s -- --version 0.1.0  a given release, or a prerelease such as 0.2.0-rc.1 (also DEVVM_VERSION)
#   ... | bash -s -- --no-deps        devvm only
#   ... | bash -s -- --force          install even when Homebrew installed devvm
# devvm (its binary, with its README, changelog and license) goes in ~/.local/lib/devvm/<version>,
# linked from ~/.local/bin/devvm (DEVVM_PREFIX instead of ~/.local). It never needs sudo; installing
# QEMU or joining the kvm group does, and asks first.
# Everything is in functions, so a download cut short runs nothing.
set -euo pipefail

# Set by scripts/release.sh in the copy attached to a release: that release, and the public key its
# SHA256SUMS is signed with (packaging/minisign.pub).
release_version=""
minisign_key=""
repo_url=${DEVVM_REPO_URL:-https://github.com/devvmhq/devvm}

# Lima, when neither it nor Homebrew is there: its latest release, checked against its SHA256SUMS.
lima_url=${DEVVM_LIMA_URL:-https://github.com/lima-vm/lima}

yes=false deps=true force=false version=${DEVVM_VERSION:-}
# Downloads and redirects over HTTPS only (tests add file).
protos="=https"
fetch() { curl --proto "$protos" --proto-redir "$protos" -fsSL "$@"; }
prefix=${DEVVM_PREFIX:-$HOME/.local}
tmp="" # downloads, deleted on exit

usage() {
  cat <<EOF
Install or upgrade devvm ($repo_url), then offer to install what it needs.
usage: install.sh [--yes] [--version VERSION] [--no-deps] [--force]
  --yes        answer yes to every question
  --version    install that release, or a prerelease, instead of the latest (also DEVVM_VERSION)
  --no-deps    install devvm only
  --force      install even when Homebrew installed devvm, leaving two copies
devvm goes in $prefix/lib/devvm/<version>, linked from $prefix/bin/devvm (DEVVM_PREFIX changes $prefix).
EOF
}

say() { echo "==> $*"; }
warn() { echo "install: $*" >&2; }
die() {
  warn "$*"
  exit 1
}

# ask QUESTION: yes with --yes; otherwise asks on the terminal (stdin is this script when piped).
# No terminal and no --yes: no.
ask() {
  local a
  ! $yes || return 0
  { : </dev/tty; } 2>/dev/null || return 1
  printf '%s [y/N] ' "$1" >/dev/tty
  read -r a </dev/tty || return 1
  [[ $a == [yY]* ]]
}

# platform: the host as release archives name it, <os>-<arch> (darwin-arm64, linux-amd64…). On a
# Mac, Apple silicon's even when this shell runs under Rosetta, which reports x86_64.
platform() {
  local os arch
  case $(uname -s) in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "devvm runs on macOS and Linux" ;;
  esac
  case $(uname -m) in
    arm64 | aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) die "devvm runs on arm64 and x86_64 hosts, not $(uname -m)" ;;
  esac
  if [[ $os == darwin && $arch == amd64 && "$(sysctl -in hw.optional.arm64 2>/dev/null)" == 1 ]]; then
    arch=arm64
  fi
  echo "$os-$arch"
}

check_host() {
  platform >/dev/null
  command -v curl >/dev/null || die "needs curl"
  command -v tar >/dev/null || die "needs tar"
  command -v sha256sum >/dev/null || command -v shasum >/dev/null || die "needs sha256sum or shasum"
}

# Where Homebrew installs, when neither the brew on PATH nor HOMEBREW_CELLAR/HOMEBREW_PREFIX has a
# Cellar with devvm (tests set their own).
brew_prefixes=(/opt/homebrew /usr/local /home/linuxbrew/.linuxbrew)

# check_brew: stops when Homebrew installed devvm, as a second copy would leave the one first on PATH
# running and brew upgrade updating the other. --force installs anyway. It never runs brew, which can
# download its own Ruby: it looks for a keg of the formula devvm or devvm-rc (Cellar/<formula>/<version>)
# in the Cellar next to the brew on PATH, in HOMEBREW_CELLAR and HOMEBREW_PREFIX's, and in brew_prefixes'.
check_brew() {
  local b cellars=() c f k
  ! $force || return 0
  b=$(command -v brew) || return 0
  [[ -z ${HOMEBREW_CELLAR:-} ]] || cellars+=("$HOMEBREW_CELLAR")
  [[ -z ${HOMEBREW_PREFIX:-} ]] || cellars+=("$HOMEBREW_PREFIX/Cellar")
  cellars+=("$(dirname -- "$b")/../Cellar")
  for c in ${brew_prefixes[@]+"${brew_prefixes[@]}"}; do cellars+=("$c/Cellar"); done
  for c in "${cellars[@]}"; do
    for f in devvm devvm-rc; do
      for k in "$c/$f"/*; do
        [[ -d $k ]] || continue
        die "Homebrew installed devvm: run brew upgrade $f to upgrade it, or brew uninstall $f first to use this script (--force installs a second copy)"
      done
    done
  done
}

# sha256 FILE: its SHA-256, with sha256sum (Linux's coreutils) or shasum (macOS's; Perl's, which
# Fedora lacks), whichever there is.
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -c1-64; }

# latest_release REPO_URL: the version of a GitHub repo's latest release, from where releases/latest
# redirects (no API, so no rate limit). GitHub's latest release is never a prerelease, and one there
# anyway is refused: only --version installs a prerelease.
latest_release() {
  local url
  url=$(fetch -I -o /dev/null -w '%{url_effective}' "$1/releases/latest") ||
    die "couldn't find the latest release at $1/releases"
  [[ $url == */tag/v* ]] || die "no release found at $1/releases"
  valid_version "${url##*/tag/v}" || die "$1's latest release has an odd version: '${url##*/tag/v}'"
  [[ ${url##*/tag/v} != *-* ]] || die "$1's latest release is a prerelease: '${url##*/tag/v}'"
  echo "${url##*/tag/v}"
}

# It goes into URLs and paths: digits and dots, and maybe a -suffix, as a prerelease's 0.2.0-rc.1 has.
valid_version() { [[ $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; }

# The release to install: --version, DEVVM_VERSION, this script's own release, else the latest.
resolve_version() {
  [[ -z $version ]] || {
    echo "${version#v}"
    return
  }
  [[ -z $release_version ]] || {
    echo "$release_version"
    return
  }
  latest_release "$repo_url"
}

# check_signature DIR: SHA256SUMS in DIR is signed by the release key (SHA256SUMS.minisig next to
# it). A checksum from the same place as the archive only catches a damaged download; the signature,
# made with a key the maintainer holds, also catches a release that someone else replaced. Needs
# minisign: without it, says how to check by hand and carries on.
check_signature() {
  local d=$1
  if [[ -z $minisign_key ]]; then
    warn "this install.sh has no release key (not from a release), so the signature isn't checked"
    return 0
  fi
  if ! command -v minisign >/dev/null; then
    warn "minisign isn't installed, so devvm's signature isn't checked, only its checksum"
    echo "  to check it yourself: minisign -Vm SHA256SUMS -P $minisign_key" >&2
    return 0
  fi
  minisign -Vqm "$d/SHA256SUMS" -x "$d/SHA256SUMS.minisig" -P "$minisign_key" >/dev/null 2>&1 ||
    die "devvm's SHA256SUMS isn't signed with the release key: not installed"
  say "signature checked"
}

# in_use DIR: a running process names something in DIR, as a VM's proxy or QEMU does devvm's binary.
# ps's output is read whole first, so no grep in a pipeline with it shows up as one.
in_use() {
  local procs
  procs=$(ps -A -ww -o command= 2>/dev/null) || return 1
  [[ $procs == *"$1/"* ]]
}

# install_devvm VERSION: download this host's archive, check it against the release's signed
# SHA256SUMS, unpack, link.
install_devvm() {
  local v=$1 name want got lib="$prefix/lib/devvm" d new base
  name="devvm-$v-$(platform)"
  base="$repo_url/releases/download/v$v"
  say "downloading devvm $v ($name)"
  fetch -o "$tmp/devvm.tar.gz" "$base/$name.tar.gz" || die "couldn't download $name.tar.gz"
  fetch -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "couldn't download devvm $v's checksums"
  if [[ -n $minisign_key ]]; then
    fetch -o "$tmp/SHA256SUMS.minisig" "$base/SHA256SUMS.minisig" ||
      die "couldn't download devvm $v's signature (SHA256SUMS.minisig)"
  fi
  check_signature "$tmp"
  want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
  got=$(sha256 "$tmp/devvm.tar.gz")
  [[ -n $want && $got == "$want" ]] || die "$name.tar.gz doesn't match its checksum: not installed"
  rm -rf "$tmp/x"
  mkdir "$tmp/x"
  tar -xzf "$tmp/devvm.tar.gz" -C "$tmp/x"
  [[ -x "$tmp/x/$name/devvm" ]] || die "$name.tar.gz has no devvm in it"
  mkdir -p "$lib" "$prefix/bin"
  # Reinstalling the same version: move the new copy in beside it, point bin/devvm at it, and only
  # then remove the old one, so an interruption never leaves bin/devvm pointing at nothing.
  new="$lib/.$v.new.$$"
  mv "$tmp/x/$name" "$new"
  ln -sfn "$new/devvm" "$prefix/bin/devvm"
  rm -rf "${lib:?}/$v"
  mv "$new" "$lib/$v"
  ln -sfn "$lib/$v/devvm" "$prefix/bin/devvm"
  # Older versions: nothing links to them now, but a running VM's proxy (or QEMU, on Linux) may
  # still run one; it's kept until a later install. (devvm's images and VMs are in
  # ~/.local/share/devvm.)
  for d in "$lib"/* "$lib"/.*.new.*; do
    [[ $d != "$lib/$v" && -d $d ]] || continue
    if [[ $d != "$lib"/.* ]] && in_use "$d"; then
      say "keeping devvm ${d##*/}: a running VM uses it"
    else
      rm -rf "$d"
    fi
  done
  say "installed devvm $v: $prefix/bin/devvm"
}

# path_hint: how to add the prefix's bin to PATH when it's missing; when it's there but another devvm
# comes first, a warning naming it. Paths are only resolved, never run.
path_hint() {
  local first
  case ":$PATH:" in
    *":$prefix/bin:"*) ;;
    *)
      echo
      echo "$prefix/bin isn't on your PATH. Add it, in ~/.zshrc or ~/.bashrc:"
      echo "  export PATH=\"$prefix/bin:\$PATH\""
      return 0
      ;;
  esac
  hash -r
  first=$(command -v devvm) || return 0
  [[ "$(readlink -f -- "$first")" != "$(readlink -f -- "$prefix/bin/devvm")" ]] || return 0
  warn "another devvm comes first on your PATH: $first"
  echo "  remove it, or put $prefix/bin before its folder in PATH" >&2
}

# Lima's latest release archive into the prefix (bin/limactl, share/lima), checked against the
# release's SHA256SUMS.
install_lima_tarball() {
  local v f want plat
  v=$(latest_release "$lima_url")
  plat=$(platform)
  case $plat in # Lima's names: Darwin-arm64, Darwin-x86_64, Linux-aarch64, Linux-x86_64
    darwin-arm64) plat=Darwin-arm64 ;;
    darwin-amd64) plat=Darwin-x86_64 ;;
    linux-arm64) plat=Linux-aarch64 ;;
    linux-amd64) plat=Linux-x86_64 ;;
  esac
  f="lima-$v-$plat.tar.gz"
  say "downloading Lima $v"
  fetch -o "$tmp/$f" "$lima_url/releases/download/v$v/$f" || die "couldn't download Lima $v"
  fetch -o "$tmp/lima-SHA256SUMS" "$lima_url/releases/download/v$v/SHA256SUMS" ||
    die "couldn't download Lima $v's checksums"
  want=$(awk -v f="$f" '$2 == f || $2 == "*" f { print $1 }' "$tmp/lima-SHA256SUMS")
  [[ -n $want && "$(sha256 "$tmp/$f")" == "$want" ]] || die "$f doesn't match its checksum: not installed"
  mkdir -p "$prefix"
  tar -xzf "$tmp/$f" -C "$prefix"
  say "installed Lima $v: $prefix/bin/limactl"
}

# run_fix "COMMAND LINE": a fix devvm suggested, split into words (no globs, no shell syntax), with
# the terminal as its input (sudo asks there).
run_fix() {
  local -a a
  read -r -a a <<<"$1"
  ((${#a[@]})) || return 0
  echo "  \$ ${a[*]}"
  if { : </dev/tty; } 2>/dev/null; then "${a[@]}" </dev/tty; else "${a[@]}" </dev/null; fi
}

# doctor_fixes: the problems in devvm doctor --json's output on stdin, a "KEY<TAB>FIX<TAB>MESSAGE"
# line each, FIX empty for a null one. It reads the layout devvm writes, a field a line, each
# value on its line; escapes in a value come out as the character, and tabs, newlines and \u ones
# as a space.
doctor_fixes() {
  awk '
    function unq(s, out, i, c) {
      if (s == "null") return ""
      s = substr(s, 2, length(s) - 2)
      out = ""
      for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (c == "\\") {
          i++
          c = substr(s, i, 1)
          if (c == "u") { c = " "; i += 4 } else if (c == "n" || c == "t" || c == "r" || c == "b" || c == "f") c = " "
        }
        out = out c
      }
      return out
    }
    /^  "problems": \[$/ { in_p = 1; next }
    in_p && /^  \]/ { in_p = 0 }
    in_p && /^      "(key|fix|message)": / {
      name = $0
      sub(/^      "/, "", name)
      sub(/".*/, "", name)
      v = $0
      sub(/^      "[a-z]+": /, "", v)
      sub(/,$/, "", v)
      val[name] = unq(v)
      if (name == "message") print val["key"] "\t" val["fix"] "\t" val["message"]
    }
  '
}

# What devvm's own check says is missing (devvm doctor --json's problems), and an offer to fix
# each.
install_deps() {
  local devvm=$1 line rest key fix m fixes=() relogin=false
  while IFS= read -r line; do [[ -z $line ]] || fixes+=("$line"); done \
    < <({ PATH="$prefix/bin:$PATH" "$devvm" doctor --json || true; } | doctor_fixes)
  ((${#fixes[@]})) || {
    say "everything devvm needs is installed"
    # Its warnings (a small machine, low free space) aren't fixes.
    PATH="$prefix/bin:$PATH" "$devvm" doctor || true
    return 0
  }
  $yes || { : </dev/tty; } 2>/dev/null ||
    echo "No terminal to ask on, so nothing is installed: run this again with --yes, or in a terminal."
  for line in "${fixes[@]}"; do
    key=${line%%$'\t'*}
    rest=${line#*$'\t'}
    fix=${rest%%$'\t'*}
    m=${rest#*$'\t'}
    [[ -n $fix || $key == lima ]] || key=none
    case $key in
      lima)
        if [[ -n $fix ]]; then # Homebrew's
          if ask "devvm needs Lima 2.2 or newer, working. Run $fix?"; then run_fix "$fix"; else echo "  skipped: $fix"; fi
        elif ask "devvm needs Lima 2.2 or newer. Install Lima's latest release into $prefix?"; then
          install_lima_tarball
        else
          echo "  skipped: see https://lima-vm.io/docs/installation/"
        fi
        ;;
      qemu | kvm-access)
        if ask "devvm needs $([[ $key == qemu ]] && echo QEMU || echo 'access to /dev/kvm'). Run $fix?"; then
          run_fix "$fix"
          [[ $key != kvm-access ]] || relogin=true
        else
          echo "  skipped: $fix"
        fi
        ;;
      *) echo "  $m" ;; # nothing to run, e.g. virtualization is off in the firmware
    esac
  done
  echo
  if $relogin; then
    # The new group only applies to new logins, so a check now would still fail.
    echo "Log out and back in (or restart) so the kvm group applies, then run devvm doctor to check."
  else
    PATH="$prefix/bin:$PATH" "$devvm" doctor || true
  fi
}

main() {
  while (($#)); do
    case $1 in
      -y | --yes) yes=true ;;
      --no-deps) deps=false ;;
      --force) force=true ;;
      --version)
        [[ -n ${2:-} ]] || die "--version needs a version"
        version=$2
        shift
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
    shift
  done
  check_host
  check_brew
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  local v
  v=$(resolve_version)
  valid_version "$v" || die "not a devvm version: '$v'"
  install_devvm "$v"
  ! $deps || install_deps "$prefix/lib/devvm/$v/devvm"
  path_hint
}

# Tests source this file with DEVVM_INSTALL_TEST set; piped into bash, BASH_SOURCE is empty.
[[ -n ${DEVVM_INSTALL_TEST:-} ]] || main "$@"
