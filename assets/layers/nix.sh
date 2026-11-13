#!/bin/bash
# nix: a built-in layer (devvm --layers nix). Nix, multi-user, with the VM's own store and
# daemon and flakes on; direnv and nix-direnv, so a project's .envrc (use flake, use nix) loads its dev
# shell in the VM's shells.
# shellcheck disable=SC2034  # the checksums are read through devvm_sha
set -euxo pipefail
: "${devvm_arch:?}" "${devvm_libc:?}"   # devvm's prelude (prelude.sh) comes before this script
# Pinned downloads (scripts/bump.sh updates them). Nix's release tarball, with its own installer;
# nix-direnv is one script, the same on every architecture.
nix_version=2.35.6
nix_sha256_arm64=1f177b572baadf8c09750028f047fe943a3615a34f3e4a43927367a1e0120801
nix_sha256_amd64=b12ab64a291e41846b9da37b7f56eafa7683deaa267bafb2ed43440ab2f45f13
direnv_version=2.38.2
direnv_sha256_arm64=ec2a32dbe2f19dd085bd9315a1b4ea88b55ec288aa7eabe8723490baf466cb23
direnv_sha256_amd64=148820a187930d36ca6a204504e5cdb30d8396cd1a97e64c3b3f10d793c11247
nix_direnv_version=3.2.0
nix_direnv_sha256=856e8d0b5247b9edc88d9377b8333a97a2363cc69abaa7760fb857609d597ebe

# Nix names the architectures as the kernel does.
case $devvm_arch in arm64) n=aarch64 ;; amd64) n=x86_64 ;; esac

# Alpine (--from alpine): GNU tar needs xz for the tarball (base has shadow's groupadd and useradd,
# which the installer uses). Its packages come from the release's repositories: not pinned. Nix's
# own binaries are static or linked against its store's C library, so they run on musl.
if [[ $devvm_libc == musl ]]; then
  sudo apk add --no-cache xz
  echo "devvm: Alpine's packages aren't pinned" >&2
fi

# Nix from its release tarball, whose installer copies the tarball's store in (downloading nothing),
# makes the build users (nixbld) and, with systemd, starts nix-daemon. Its settings: flakes and the nix command on;
# only root trusted, so the VM's user can't choose other caches (substituters) or keys; and builds
# fail where the kernel can't sandbox them, rather than run unsandboxed without a word.
devvm-fetch "https://releases.nixos.org/nix/nix-$nix_version/nix-$nix_version-$n-linux.tar.xz" "$(devvm_sha nix)" /tmp/nix.tar.xz
rm -rf /tmp/nix-install; mkdir /tmp/nix-install
tar -xJf /tmp/nix.tar.xz -C /tmp/nix-install --strip-components=1
rm /tmp/nix.tar.xz
printf '%s\n' 'experimental-features = nix-command flakes' 'trusted-users = root' 'sandbox = true' \
  'sandbox-fallback = false' > /tmp/nix-extra.conf
# Without proxy variables (a build behind a company proxy has its build proxy's): the installer writes
# any it finds into nix-daemon's settings, and so into the image. guest/sync.sh gives the daemon the
# VM's own proxy. The system's CAs, devvm's and the user's among them, rather
# than a bundle of Nix's own.
env -u http_proxy -u https_proxy -u ftp_proxy -u all_proxy -u no_proxy \
  -u HTTP_PROXY -u HTTPS_PROXY -u FTP_PROXY -u ALL_PROXY -u NO_PROXY \
  NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
  /tmp/nix-install/install --daemon --yes --no-channel-add --nix-extra-conf-file /tmp/nix-extra.conf
rm -rf /tmp/nix-install /tmp/nix-extra.conf
# The store is root's (Nix's installer before 2.35.3 left it the installing user's).
if [[ -n $(sudo find /nix/store -mindepth 1 -maxdepth 1 ! -user root -print -quit) ]]; then
  echo "devvm: Nix's store isn't root's after its install" >&2
  exit 1
fi
# Running now too, for the rest of the build: systemd started it, and OpenRC (Alpine) gets a
# service of devvm's, started at each boot and now. Under OpenRC, its settings are
# /etc/conf.d/nix-daemon (guest/sync.sh's: the VM's proxy), which OpenRC reads
# for its service.
if [[ -d /run/systemd/system ]]; then
  sudo systemctl is-active --quiet nix-daemon.socket
elif [[ -d /run/openrc ]]; then
  sudo tee /etc/init.d/nix-daemon >/dev/null <<'RC'
#!/sbin/openrc-run
description="Nix's daemon"
command=/nix/var/nix/profiles/default/bin/nix-daemon
command_args=--daemon
command_background=true
pidfile=/run/nix-daemon.pid
output_log=/var/log/nix-daemon.log
error_log=/var/log/nix-daemon.log
depend() { need localmount; after net; }
RC
  sudo chmod 755 /etc/init.d/nix-daemon
  sudo rc-update add nix-daemon default
  sudo rc-service nix-daemon start
fi

# sudo nix: sudo's secure_path, with Nix's default profile last.
sp=$(sudo sed -n 's/^Defaults[[:space:]]\{1,\}secure_path[[:space:]]*=[[:space:]]*"\{0,1\}\([^"]*\)"\{0,1\}[[:space:]]*$/\1/p' /etc/sudoers | tail -1)
if [[ -n $sp ]]; then
  printf 'Defaults secure_path="%s:/nix/var/nix/profiles/default/bin"\n' "$sp" > /tmp/devvm-nix.sudoers
  sudo visudo -cqf /tmp/devvm-nix.sudoers
  sudo install -m 440 /tmp/devvm-nix.sudoers /etc/sudoers.d/devvm-nix
  rm /tmp/devvm-nix.sudoers
fi
# Shells that read neither profile.d nor bash.bashrc (bash -c from agents, editors over SSH) find Nix
# through BASH_ENV, after everything else; login and interactive shells put it first, as Nix's own
# profile script does.
sudo tee -a /etc/profile.d/devvm-tools.sh >/dev/null <<'SH'
case ":$PATH:" in *":/nix/var/nix/profiles/default/bin:"*) ;; *)
  export PATH="$PATH:$HOME/.nix-profile/bin:/nix/var/nix/profiles/default/bin" ;;
esac
SH

# direnv, and nix-direnv's use flake and use nix for it.
devvm-fetch "https://github.com/direnv/direnv/releases/download/v$direnv_version/direnv.linux-$devvm_arch" "$(devvm_sha direnv)" /tmp/direnv
sudo install -m 755 /tmp/direnv /usr/local/bin/direnv
rm /tmp/direnv
devvm-fetch "https://raw.githubusercontent.com/nix-community/nix-direnv/$nix_direnv_version/direnvrc" "$nix_direnv_sha256" /tmp/nix-direnv
sudo install -D -m 644 /tmp/nix-direnv /usr/local/share/nix-direnv/direnvrc
rm /tmp/nix-direnv
# direnv reads ~/.config/direnv/lib/*.sh before each .envrc. A project's layout (nix-direnv's cached
# dev shell and its garbage collector roots) goes in the VM's home, not .direnv in the project folder:
# the roots point into this VM's store, and the folder is the host's.
mkdir -p ~/.config/direnv/lib
cat > ~/.config/direnv/lib/devvm-nix.sh <<'SH'
# devvm's nix layer: nix-direnv, and each project's layout in this VM's home.
. /usr/local/share/nix-direnv/direnvrc
direnv_layout_dir() {
  echo "${direnv_layout_dir:-${XDG_CACHE_HOME:-$HOME/.cache}/direnv/layouts/$(printf %s "$PWD" | sha256sum | cut -c1-16)}"
}
SH
# The hook runs before each prompt. direnv.sh loads before mise.sh (~/.bashrc.d in order), and each
# puts its hook first, so direnv's runs after mise's: a dev shell's tools come before the shared ones.
echo 'eval "$(direnv hook bash)"' > ~/.bashrc.d/direnv.sh
