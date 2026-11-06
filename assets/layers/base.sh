#!/bin/bash
# Essentials, and mise for the shared tools (shared-tools/).
# shellcheck disable=SC2034  # the checksums are read through devvm_sha
set -euxo pipefail
: "${devvm_arch:?}" "${devvm_libc:?}"   # devvm's prelude (prelude.sh) comes before this script
# Pinned downloads (scripts/bump.sh updates them).
mise_version=2026.11.3
mise_sha256_arm64=a9a2a66bbbe87ea9cc849e1f006606ae2b34f3d3ba56a54d5cd5d64b6557d164
mise_sha256_amd64=c2f7e814cb39f60b7f67a0ff36cb805e94128bf2dff09d175309d0f095b8cc3b
mise_sha256_arm64_musl=78c87fb8c8af031755f9241cbcd1886cd458c75e8f324020015445a776789e38
mise_sha256_amd64_musl=72f1cb4591ee2ba90150022c3ee8aea4dd88fadac10c56d04ede4faffb5a1b03

export DEBIAN_FRONTEND=noninteractive
# Alpine (--from alpine): apk, OpenRC and musl. Its packages come from the release's repositories,
# which have no snapshots: they aren't pinned.
if [[ $devvm_libc == musl ]]; then
  # GNU tar: devvm's launch sends files with it (busybox's lacks options it uses). sqlite: an export
  # of agents' history copies opencode's database without its logins with it.
  sudo apk add --no-cache git curl wget ca-certificates build-base unzip jq ripgrep bash coreutils tar \
    util-linux util-linux-misc losetup findmnt erofs-utils sqlite
  echo "devvm: Alpine's packages aren't pinned" >&2
else
# The package lists are apt.sh begin's: base's packages are recorded at its first build and replayed
# from devvm's cache after (layers/apt.sh), and a --from image's come from its own sources, unpinned.
# erofs-utils: the shared tools' builds make their images with it, in a VM cloned from base. sqlite3:
# an export of agents' history copies opencode's database without its logins with it.
sudo apt-get install -y git curl wget ca-certificates build-essential unzip jq ripgrep erofs-utils sqlite3
fi

# devvm-fetch URL SHA256 OUT: every layer downloads through this, so an image only ever contains
# files whose checksums are pinned in devvm's scripts.
sudo tee /usr/local/bin/devvm-fetch >/dev/null <<'SH'
#!/bin/sh
set -eu
url=$1 want=$2 out=$3
curl -fsSL --retry 3 -o "$out" "$url"
got=$(sha256sum "$out" | cut -d' ' -f1)
if [ "$got" != "$want" ]; then
  rm -f "$out"
  printf 'devvm-fetch: checksum mismatch for %s\n  expected %s\n  got      %s\n' "$url" "$want" "$got" >&2
  exit 1
fi
SH
sudo chmod 755 /usr/local/bin/devvm-fetch

if [[ $devvm_libc == gnu ]]; then
# Ubuntu's cloud image is a server's: drop what a dev VM doesn't use (snapd, multipath, modem and disk
# managers, firmware updates, crash reporting, tracing tools, kernel headers), and apt's background
# updates, which hold apt's lock and, on a confined tier, show up as blocks. Measured: 0.9 GB less
# disk, 100 MB less memory, 7 fewer services and 9 fewer timers. The everyday tools the server
# metapackages brought stay. Only what's installed is touched (--from images have none of it).
keep="bc file htop lsof nano rsync screen strace tcpdump time tmux mtr-tiny pciutils usbutils
  inetutils-telnet dmidecode lshw linux-image-virtual"
drop="ubuntu-server ubuntu-standard snapd multipath-tools modemmanager udisks2 unattended-upgrades
  fwupd rsyslog networkd-dispatcher bpftrace bpfcc-tools linux-tools-common sysstat python3-boto3
  linux-headers-generic linux-headers-virtual apport landscape-common lxd-installer lxd-agent-loader
  open-iscsi plymouth needrestart update-notifier-common open-vm-tools"
installed() {   # installed PACKAGE...: the ones that are
  { dpkg-query -Wf '${db:Status-Status} ${Package}\n' "$@" 2>/dev/null || true; } | awk '$1 == "installed" { print $2 }'
}
# shellcheck disable=SC2086  # package lists
{
  k=$(installed $keep); [[ -z $k ]] || sudo apt-mark manual $k >/dev/null
  d=$(installed $drop); [[ -z $d ]] || sudo apt-get purge -y --autoremove $d
}
sudo systemctl mask apt-daily.timer apt-daily-upgrade.timer
fi

# Interactive shell snippets from features go in ~/.bashrc.d.
mkdir -p ~/.bashrc.d
grep -qs bashrc.d ~/.bashrc || echo 'for f in ~/.bashrc.d/*.sh; do [ -r "$f" ] && . "$f"; done' >> ~/.bashrc

# mise runs the shared tools (node, python, go, rust, agents), read-only images mounted at
# /opt/devvm/tools by devvm-tools (guest/tools.sh). Each VM's tools and versions are in
# /etc/mise/config.toml (written by guest/sync.sh); the user's own mise config and a project's version
# files (.nvmrc, .python-version, ...) come first, and versions that aren't shared install into the VM.
# mise names amd64 x64, and its musl builds end -musl.
m=${devvm_arch/amd64/x64}; [[ $devvm_libc == gnu ]] || m+=-musl
devvm-fetch "https://github.com/jdx/mise/releases/download/v$mise_version/mise-v$mise_version-linux-$m" "$(devvm_sha mise)" /tmp/mise
sudo install -m 755 /tmp/mise /usr/local/bin/mise
rm /tmp/mise
# Every shell finds the tools: login shells through profile.d, interactive ones through mise activate
# (it follows `cd` into projects with other versions), and the rest (`bash -c` from agents, editors
# over SSH) through BASH_ENV and mise's shims. Read-only installs can't take global packages or update
# themselves: npm installs globals into ~/.npm-global, and Claude Code's and opencode's auto-updates
# are off.
sudo tee /etc/profile.d/devvm-tools.sh >/dev/null <<'SH'
case ":$PATH:" in *":$HOME/.local/share/mise/shims:"*) ;; *)
  export PATH="$HOME/.npm-global/bin:$HOME/go/bin:$HOME/.cargo/bin:$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH" ;;
esac
export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export DISABLE_AUTOUPDATER=1
export OPENCODE_DISABLE_AUTOUPDATE=1
SH
grep -qs devvm-tools /etc/environment || echo 'BASH_ENV=/etc/profile.d/devvm-tools.sh' | sudo tee -a /etc/environment >/dev/null
echo 'eval "$(mise activate bash)"' > ~/.bashrc.d/mise.sh
# Mounted at boot, once the share is: devvm-tools reads the VM's list (/etc/devvm/tools).
if [[ $devvm_libc == musl ]]; then
  sudo tee /etc/init.d/devvm-tools >/dev/null <<'RC'
#!/sbin/openrc-run
description="Mount devvm's shared tools"
depend() { need localmount; after netmount; }
start() { [ -x /usr/local/sbin/devvm-tools ] || return 0; /usr/local/sbin/devvm-tools; }
RC
  sudo chmod 755 /etc/init.d/devvm-tools
  sudo rc-update add devvm-tools default
else
sudo tee /etc/systemd/system/devvm-tools.service >/dev/null <<'UNIT'
[Unit]
Description=Mount devvm's shared tools
RequiresMountsFor=/mnt/devvm-tools
ConditionPathExists=/usr/local/sbin/devvm-tools
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/devvm-tools
[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl enable devvm-tools.service
fi

# No browser in the VM: tools that open one (xdg-open, $BROWSER) print the URL instead of crashing.
# Ports are forwarded, so the URL works on the host as-is.
sudo tee /usr/local/bin/xdg-open >/dev/null <<'SH'
#!/bin/sh
echo "devvm: no browser in the VM; open on your host: $*" >&2
SH
sudo chmod 755 /usr/local/bin/xdg-open
echo 'export BROWSER=xdg-open' | sudo tee /etc/profile.d/devvm-browser.sh >/dev/null

# Telemetry off for every tool that has a switch (installed by devvm or commonly by projects).
sudo tee /etc/profile.d/devvm-telemetry.sh >/dev/null <<'SH'
export DO_NOT_TRACK=1                 # generic opt-out (bun and others)
export DISABLE_TELEMETRY=1            # Claude Code
export DISABLE_ERROR_REPORTING=1      # Claude Code
export NEXT_TELEMETRY_DISABLED=1
export TURBO_TELEMETRY_DISABLED=1
export ASTRO_TELEMETRY_DISABLED=1
export NUXT_TELEMETRY_DISABLED=1
export GATSBY_TELEMETRY_DISABLED=1
export STORYBOOK_DISABLE_TELEMETRY=1
export YARN_ENABLE_TELEMETRY=0
export DOTNET_CLI_TELEMETRY_OPTOUT=1
SH
# Ubuntu: no news fetched for the login banner or apt output.
sudo sed -i 's/^ENABLED=1/ENABLED=0/' /etc/default/motd-news 2>/dev/null || true
sudo pro config set apt_news=false >/dev/null 2>&1 || true

# GitHub's SSH host keys (from https://api.github.com/meta), so the first git-over-SSH doesn't prompt.
sudo tee -a /etc/ssh/ssh_known_hosts >/dev/null <<'KEYS'
github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
KEYS
# The VM's sshd: local forwarding (Lima's own port and socket forwards, editors' -L and -D), and
# remote forwards of TCP ports only on 127.77.0.0/16, the addresses of the VM's links, which devvm
# on the host asks for by number (clientspecified binds the address asked for, which PermitListen
# matches as text, so 0.0.0.0, 127.0.0.1, ::1 and localhost are refused); no remote socket forwards,
# tunnels or X11. First in sshd_config.d, so it wins. It's a safeguard for an honest VM, as root in
# it can change it: the boundary is the host's ssh, which asks for no other remote forward and
# accepts a channel only for a forward it asked for.
if [[ -d /etc/ssh ]]; then
  sudo mkdir -p /etc/ssh/sshd_config.d
  printf '%s\n' 'AllowTcpForwarding all' 'AllowStreamLocalForwarding local' 'GatewayPorts clientspecified' \
    'PermitListen 127.77.*:*' 'PermitTunnel no' 'X11Forwarding no' | sudo tee /etc/ssh/sshd_config.d/00-devvm.conf >/dev/null
fi
