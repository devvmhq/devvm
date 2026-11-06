#!/bin/bash
# Builds one shared tool image in a throwaway VM (a clone of base) that has an output folder of its own
# mounted writable at /mnt/devvm-tools (never the share itself), with mise.toml and the host's lock
# (~/.local/share/devvm/mise.lock) in its .build folder. Each tool gets a VM and a folder of its own,
# so nothing one tool's installer does, as root or not, reaches another tool's image.
#   build.sh TOOL VERSION FILE
# The tool is installed at exactly the locked version (checked against the lock's checksums), then
# packed into one compressed read-only EROFS image, FILE in that folder; devvm moves it into the share
# and records its hash. Project VMs mount it at /opt/devvm/tools/<tool>/<version>, where mise finds it
# as a shared install. For a glibc build, FILE.glibc beside it holds the newest glibc version its
# programs and libraries need (glibc_needs), which devvm records and checks a --from base against.
# shellcheck disable=SC2034  # the checksums are read through devvm_sha
set -euxo pipefail
: "${devvm_arch:?}" "${devvm_libc:?}"   # devvm's prelude (prelude.sh) comes before this script
# Pinned downloads (scripts/bump.sh updates them). rustup checks the toolchain against Rust's signed
# channel manifest.
rustup_version=1.29.3
rustup_sha256_arm64=9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282
rustup_sha256_amd64=b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55
rustup_sha256_arm64_musl=956deacdc27ab82ae7ac821d5d3e9e95e4795d476d17efe99743bb8531ca1031
rustup_sha256_amd64_musl=1552169aba2836777136ccff7a381e95c17233b5d8250cd5e10ae3cb971bfcfc

share=/mnt/devvm-tools
# Where VMs mount the tools: installs record absolute paths (mise's bin links, scripts' shebangs).
work=/opt/devvm/tools
# On Alpine (musl), the musl builds: mise picks them, and rustup gets the musl toolchain.
case $devvm_arch in arm64) triple=aarch64 ;; amd64) triple=x86_64 ;; esac
triple+=-unknown-linux-$devvm_libc
# The output folder, once it's there: Lima mounts it at boot (cloud-init, nofail), so a mount that
# failed or came late leaves an empty folder. Waits up to 60 s for the lock devvm put in it,
# mounting it again from fstab while it isn't mounted, then fails naming it and mount's last error.
wait_share() {
  local i err=""
  for (( i = 0; i < 60; i++ )); do
    [[ -f $share/.build/mise.lock ]] && return 0
    mountpoint -q "$share" || err=$(sudo mount "$share" 2>&1) || true
    sleep 1
  done
  echo "build.sh: devvm's output folder isn't mounted at $share (no .build/mise.lock after 60 s)${err:+: $err}" >&2
  return 1
}
# glibc_needs DIR: the newest glibc version that DIR's ELF programs and libraries need, from their
# version needs (GLIBC_2.28), or nothing when none needs one. Test data is left out: Go's has ELF
# files of other systems that nothing runs. Only files with GLIBC_ in them are read with readelf
# (base's build-essential has it), which takes no others. The version needs come after readelf's
# "Version needs section" line, until its next section.
glibc_needs() {
  local files elf
  files=$(mktemp); elf=$(mktemp)
  find "$1" -name testdata -prune -o -type f -print0 > "$files"
  # xargs exits 123 when a grep matched nothing.
  LC_ALL=C xargs -0 -r grep -laZ 'GLIBC_2\.' < "$files" > "$elf" || (( $? == 123 ))
  { LC_ALL=C xargs -0 -r readelf -W -V < "$elf" 2>/dev/null || (( $? == 123 )); } |
    awk '/^Version needs section/ { n = 1; next } /^Version [a-z]+ section/ { n = 0 }
      n && match($0, /Name: GLIBC_[0-9]+\.[0-9]+/) { print substr($0, RSTART + 12, RLENGTH - 12) }' |
    sort -t. -k1,1n -k2,2n | tail -1
  rm -f "$files" "$elf"
}
[[ $devvm_libc != gnu ]] || command -v readelf >/dev/null || { echo "build.sh: base has no readelf (binutils)" >&2; exit 1; }
(( $# == 3 )) || { echo "build.sh: wants TOOL VERSION FILE, got $# arguments" >&2; exit 2; }
tool=$1 version=$2 file=$3

wait_share
sudo rm -rf "$work"; sudo mkdir -p "$work"; sudo chown "$USER" "$work"
cd "$share/.build"
export MISE_LOCKED=1 MISE_TRUSTED_CONFIG_PATHS="$share/.build" MISE_YES=1

if [[ $tool == rust ]]; then
  # The toolchain as rustup lays it out, plus rustup itself for the VM to install (it links the
  # toolchain in). rust-src and rust-analyzer now: components can't be added to a read-only toolchain.
  dir=$work/rust/$version; mkdir -p "$dir"
  devvm-fetch "https://static.rust-lang.org/rustup/archive/$rustup_version/$triple/rustup-init" \
    "$(devvm_sha rustup)" "$dir/rustup-init"
  chmod 755 "$dir/rustup-init"
  RUSTUP_HOME=$work/rustup CARGO_HOME=$work/cargo "$dir/rustup-init" -y -q --no-modify-path \
    --profile default --default-toolchain "$version" -c rust-src -c rust-analyzer
  mv "$work/rustup/toolchains/$version-$triple" "$dir/toolchain"
else
  # By name: mise resolves mise.toml's version through the lock (an explicit tool@version isn't
  # matched to the lock's entry).
  mise install --shared "$work" "$tool"
  dir=$work/$tool/$version
  [[ -d $dir ]] || { echo "build.sh: mise installed another $tool version than $version: $(ls "$work/$tool")" >&2; exit 1; }
  # Corepack writes its shims next to node: now, since VMs can't.
  [[ $tool != node ]] || PATH=$dir/bin:$PATH corepack enable yarn
fi
[[ $devvm_libc != gnu ]] || glibc_needs "$dir" > "$share/$file.glibc"
# --all-root: the VM's user only reads them. lz4hc: about half the size, fast to read.
mkfs.erofs -zlz4hc --all-root "$share/$file.tmp" "$dir" >/dev/null
mv "$share/$file.tmp" "$share/$file"
sudo rm -rf "$work"
