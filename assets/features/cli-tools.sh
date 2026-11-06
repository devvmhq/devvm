#!/bin/bash
# CLI tools: bat, fd, eza, zoxide (z), btop, tree, tldr
# shellcheck disable=SC2034  # the checksums are read through devvm_sha
set -euxo pipefail
: "${devvm_arch:?}" "${devvm_libc:?}"   # devvm's prelude (prelude.sh) comes before this script
# Pinned downloads (scripts/bump.sh updates them). eza and tealdeer (tldr) come from their releases:
# Debian 12 (node:24's base) has no eza, and its tealdeer 1.6 can't read today's pages archive.
eza_version=0.23.5
eza_sha256_arm64=40b87ae8628aa2ff0f0d2dc24ab52f689631366385c3da630bae745671fd71ec
eza_sha256_amd64=e06eebab74b73d6b7d51a796a353824b001bea82df077706382e100815d28904
tealdeer_version=1.9.1
tealdeer_sha256_arm64=247746dd2d18f0135da79b849bb6b49e45262e2fe4318adfc3082dc6ce3ee968
tealdeer_sha256_amd64=5a9c3f0691b9cf375ee5c99de802885f3d50498cf686a539e2ddda028eb6b4f6

if [[ $devvm_libc == musl ]]; then
  # Alpine packages eza too (its arm64 release build below is glibc).
  sudo apk add --no-cache bat fd zoxide btop tree eza
else
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y bat fd-find zoxide btop tree
  # Debian renames bat and fd to avoid clashes; expose the usual names.
  sudo ln -sf /usr/bin/batcat /usr/local/bin/bat
  sudo ln -sf /usr/bin/fdfind /usr/local/bin/fd
fi

# eza's x86_64 build is the static musl one; its arm64 build is glibc (fine on Debian 12's 2.36).
case $devvm_arch in
  arm64) e=aarch64-unknown-linux-gnu t=aarch64 ;;
  amd64) e=x86_64-unknown-linux-musl t=x86_64 ;;
esac
tmp=$(mktemp -d)
if [[ $devvm_libc == gnu ]]; then
  devvm-fetch "https://github.com/eza-community/eza/releases/download/v$eza_version/eza_$e.tar.gz" "$(devvm_sha eza)" "$tmp/eza.tar.gz"
  tar -C "$tmp" -xzf "$tmp/eza.tar.gz"
  sudo install -m 755 "$tmp/eza" /usr/local/bin/eza
fi
devvm-fetch "https://github.com/tealdeer-rs/tealdeer/releases/download/v$tealdeer_version/tealdeer-linux-$t-musl" "$(devvm_sha tealdeer)" "$tmp/tldr"
sudo install -m 755 "$tmp/tldr" /usr/local/bin/tldr
rm -rf "$tmp"
eza --version

# The pages download at build time; a failure leaves tldr without pages until `tldr --update`.
tldr --update || echo "cli-tools: couldn't download tldr pages; run 'tldr --update' in the VM" >&2
echo 'eval "$(zoxide init bash)"' > ~/.bashrc.d/zoxide.sh
