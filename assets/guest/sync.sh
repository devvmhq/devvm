#!/bin/bash
# Runs inside a project VM on every devvm launch, from a staging dir the host sends over:
#   home/        agents' settings files and logins from devvm's store, copied into ~
#   links        agents' settings folders from devvm's store, two lines each: the path relative to
#                ~, then the folder's path, where it's mounted read-only; linked into ~
#   claude.json  the store's .claude.json's settings, account state and MCP servers (never its API key
#                or per-project history), merged in (MCP servers' env values and headers are
#                placeholders)
#   env          exports (LANG, CLAUDE_CODE_OAUTH_TOKEN, GH_TOKEN: placeholders), installed as
#                ~/.config/devvm/env
#   ca/          the CAs the VM trusts besides its system's, one certificate per file:
#                devvm-proxy.crt and devvm-proxy-next.crt, the proxy's; devvm-extra-N.crt, the user's
#                own; ca.sh installs them
#   vm           DEVVM_* host settings applied to every VM: hostname, timezone, locale, git identity,
#                DEVVM_PROXY (the proxy URL, empty when the guest has none),
#                DEVVM_NO_PROXY (what reaches the VM itself without the proxy; its links go through it),
#                DEVVM_LOCAL_PROXY (ADDRESS:PORT on the VM's loopback, where devvm-local-proxy
#                listens; empty without a proxy), DEVVM_TRANSPARENT_ADDR and
#                DEVVM_TRANSPARENT_HOST (transparent mode's stand-in address, empty for off, and the
#                proxy's reserved target; transparent.sh), and DEVVM_PASTE_DIR (the folder devvm
#                paste writes to, config.toml's paste-dir: made here, the user's own, mode 700)
#   transparent.sh  devvm-transparent: transparent mode, for tools that ignore the proxy settings
#   tools        the VM's shared tool images, "FILE TOOL VERSION" lines; tools.sh mounts them
#   tool-alias.toml  the [tool_alias] table of devvm's shared-tools/mise.toml: the backend each
#                tool's image was built from, which mise's registry might not give its name
#   agent-env    export lines of every coding agent's guest environment (its self-update, version
#                check and telemetry off), installed as /etc/devvm/agents.sh, which devvm-tools.sh reads
#   toolchain-env  export lines of every toolchain's guest environment (its telemetry off, its global
#                installs into ~), then devvm_path, their folders of ~ for the PATH, each followed by a
#                colon; installed as /etc/devvm/toolchains.sh, which devvm-tools.sh reads
#   version-files.toml  the VM's mise's idiomatic_version_file_enable_tools setting: the toolchains'
#                tools whose version files (.nvmrc, global.json) a project chooses versions with
#   opencode-import.json  the store's opencode logins as opencode auth import reads them, with
#                placeholders (none: no opencode login in the store); opencode.sh imports them
#   hosts        the VM's links (devvm checked them): a line per target, "ADDRESS NAME
#                NAME.devvm.internal...", for /etc/hosts
#   guide.md     devvm's guide for the agents in the VM, installed as /usr/share/doc/devvm/README.md
#   moved        only after the project folder moved: moved_from and moved_to, its old path and
#                its new, and claude_from and claude_to, their Claude Code slugs; claude.sh moves
#                Claude Code's sessions and settings from the old to the new
set -euo pipefail
# devvm reads devvm-tools' report when this setup fails, so an older one (from boot) goes first.
sudo rm -f /run/devvm/tools-unmounted
s=$(cd "$(dirname "$0")" && pwd)

# shellcheck source=/dev/null
. "$s/vm"
# Running systemd, else OpenRC (Alpine).
systemd=false; [[ -d /run/systemd/system ]] && systemd=true
if [[ -n "$DEVVM_HOSTNAME" && "$(hostname)" != "$DEVVM_HOSTNAME" ]]; then
  if $systemd; then sudo hostnamectl set-hostname "$DEVVM_HOSTNAME"
  else echo "$DEVVM_HOSTNAME" | sudo tee /etc/hostname >/dev/null; sudo hostname "$DEVVM_HOSTNAME"; fi
  grep -q "[[:space:]]$DEVVM_HOSTNAME\$" /etc/hosts || echo "127.0.1.1 $DEVVM_HOSTNAME" | sudo tee -a /etc/hosts >/dev/null
fi
# link_hosts FILE HOSTS: replace (or remove) the block of the VM's links in FILE with HOSTS, a line
# per target: its address, its names and NAME.devvm.internal. Written in place, never renamed over,
# so the file keeps its mode, and only when it changed.
link_hosts() {
  local tmp
  tmp=$(mktemp)
  sed '/^# devvm links begin$/,/^# devvm links end$/d' "$1" > "$tmp"
  [[ ! -s $2 ]] || { echo '# devvm links begin'; cat "$2"; echo '# devvm links end'; } >> "$tmp"
  cmp -s "$tmp" "$1" || sudo cp "$tmp" "$1"   # into the file itself, which keeps its mode
  rm -f "$tmp"
}
link_hosts /etc/hosts "$s/hosts"
# A link listens on its target's own port, so one below 1024 needs them open to the VM's user, whose
# sshd binds it. 1024 is the kernel's default (Lima's Alpine VMs); Lima's Ubuntu and Debian
# VMs have 0.
ports=/proc/sys/net/ipv4/ip_unprivileged_port_start
if [[ "$(cat "$ports" 2>/dev/null)" == 1024 ]]; then
  echo 0 | sudo tee "$ports" >/dev/null || echo "devvm: links to ports below 1024 can't listen in this VM" >&2
fi
if [[ -n "$DEVVM_TZ" ]]; then
  if $systemd; then
    [[ "$(timedatectl show -p Timezone --value)" == "$DEVVM_TZ" ]] ||
      sudo timedatectl set-timezone "$DEVVM_TZ" || echo "devvm: unknown timezone $DEVVM_TZ" >&2
  elif [[ -f /usr/share/zoneinfo/$DEVVM_TZ ]]; then
    [[ "$(readlink /etc/localtime)" == "/usr/share/zoneinfo/$DEVVM_TZ" ]] || sudo ln -sfn "/usr/share/zoneinfo/$DEVVM_TZ" /etc/localtime
  fi
fi
# The paste folder (devvm paste): made once, the user's own, mode 700, by the user, or by root where
# the user can't make it, such as a folder under /srv. One that's there keeps its mode. A link, a
# file or another user's folder there is left alone, with a warning, as is one that can't be made,
# under a file say: paste.sh refuses them too, and the rest of the setup goes on.
if [[ -n "${DEVVM_PASTE_DIR:-}" ]]; then
  p=$DEVVM_PASTE_DIR
  if [[ -L $p ]]; then echo "devvm: $p is a symbolic link, so devvm paste can't use it" >&2
  elif [[ -e $p && ! -d $p ]]; then echo "devvm: $p isn't a folder, so devvm paste can't use it" >&2
  elif [[ -d $p && ! -O $p ]]; then echo "devvm: $p isn't yours, so devvm paste can't use it" >&2
  elif [[ ! -e $p ]]; then
    (umask 077 && mkdir -p "$p") 2>/dev/null || sudo install -d -m 700 -o "$(id -u)" -g "$(id -g)" "$p" \
      || echo "devvm: $p can't be made, so devvm paste can't use it" >&2
  fi
fi
# musl (Alpine) has no locales to generate.
if [[ -n "$DEVVM_LANG" ]] && command -v locale-gen >/dev/null && ! locale -a | grep -qix "${DEVVM_LANG/UTF-8/utf8}"; then
  sudo locale-gen "$DEVVM_LANG" >/dev/null
fi

# Shared tools: mount this VM's images (at boot too, by devvm-tools.service), then tell mise which
# versions to use, and each one's backend. /etc/mise is mise's system config: the user's own
# (~/.config/mise) comes first.
cmp -s "$s/tools.sh" /usr/local/sbin/devvm-tools 2>/dev/null || sudo install -D -m 755 "$s/tools.sh" /usr/local/sbin/devvm-tools
cmp -s "$s/tools" /etc/devvm/tools 2>/dev/null || sudo install -D -m 644 "$s/tools" /etc/devvm/tools
# The toolchains' and the agents' environment (their telemetry and self-updates off, the toolchains'
# folders for the PATH), which base's devvm-tools.sh reads in every shell, before anything here runs
# one.
cmp -s "$s/toolchain-env" /etc/devvm/toolchains.sh 2>/dev/null ||
  sudo install -D -m 644 "$s/toolchain-env" /etc/devvm/toolchains.sh
cmp -s "$s/agent-env" /etc/devvm/agents.sh 2>/dev/null || sudo install -D -m 644 "$s/agent-env" /etc/devvm/agents.sh
# A tool it can't mount fails the setup, and devvm names it from /run/devvm/tools-unmounted.
sudo /usr/local/sbin/devvm-tools
{
  cat "$s/tool-alias.toml"
  echo
  echo '[tools]'
  while read -r _ tool version; do [[ -z $tool || $tool == rust ]] || echo "$tool = \"$version\""; done < "$s/tools"
  cat <<'T'

[settings]
experimental = true
shared_install_dirs = ["/opt/devvm/tools"]
# A version that a project's version files choose and that isn't shared installs into the VM on first
# use.
auto_install = true
not_found_auto_install = true
python.compile = false
# mise points GOBIN into Go's install, which is read-only: go install goes to ~/go/bin.
go_set_gobin = false
# Each .NET SDK in a folder of its own, the shared one's its image: mise would otherwise run every SDK
# from one folder of the VM's, which the shared one isn't in.
dotnet.isolated = true
# The project folder is the user's own: trust its mise.toml without asking.
trusted_config_paths = ["/"]
T
  # A project's version files choose versions, as their own tools would: the toolchains' tools that
  # devvm's registry names, in [settings] still.
  cat "$s/version-files.toml"
} > "$s/mise.toml"
cmp -s "$s/mise.toml" /etc/mise/config.toml 2>/dev/null || sudo install -D -m 644 "$s/mise.toml" /etc/mise/config.toml
# Links into tools this VM doesn't have any more (devvm tools rm, devvm tools update).
for l in ~/.rustup/toolchains/* ~/.local/bin/claude; do
  [[ -L $l && ! -e $l && "$(readlink "$l")" == /opt/devvm/tools/* ]] && rm "$l"
done
# Rust: rustup in ~/.cargo (installed once, from the tool image) with the shared toolchain linked in.
# Other toolchains (rust-toolchain.toml) install next to it.
rust=$(awk '$2 == "rust" { print $3 }' "$s/tools")
if [[ -n $rust ]]; then
  [[ -x ~/.cargo/bin/rustup ]] ||
    /opt/devvm/tools/rust/"$rust"/rustup-init -y -q --no-modify-path --default-toolchain none >/dev/null
  mkdir -p ~/.rustup/toolchains
  t=~/.rustup/toolchains/$rust-$(uname -m)-unknown-linux-$(command -v apk >/dev/null && echo musl || echo gnu)
  [[ -L $t || -e $t ]] || ln -s /opt/devvm/tools/rust/"$rust"/toolchain "$t"
  ~/.cargo/bin/rustup default >/dev/null 2>&1 || ~/.cargo/bin/rustup default "$rust" >/dev/null
fi
# Go's telemetry off (it keeps the setting in ~/.config/go).
go=$(awk '$2 == "go" { print $3 }' "$s/tools")
if [[ -n $go && "$(cat ~/.config/go/telemetry/mode 2>/dev/null)" != off* ]]; then
  /opt/devvm/tools/go/"$go"/bin/go telemetry off
fi
# .NET's global tools (~/.dotnet/tools) find the shared runtime in every shell through
# /etc/dotnet/install_location, where .NET's programs look for an installed one: mise sets
# DOTNET_ROOT only in the shells it activates.
dotnet=$(awk '$2 == "dotnet" { print $3 }' "$s/tools")
loc=$(cat /etc/dotnet/install_location 2>/dev/null || true)
if [[ -n $dotnet && $loc != "/opt/devvm/tools/dotnet/$dotnet" ]]; then
  sudo mkdir -p /etc/dotnet
  echo "/opt/devvm/tools/dotnet/$dotnet" | sudo tee /etc/dotnet/install_location >/dev/null
elif [[ -z $dotnet && $loc == /opt/devvm/tools/* ]]; then
  sudo rm -f /etc/dotnet/install_location
fi
# Claude Code checks for itself at ~/.local/bin/claude.
claude=$(awk '$2 == "claude" { print $3 }' "$s/tools")
if [[ -n $claude ]]; then mkdir -p ~/.local/bin; ln -sfn /opt/devvm/tools/claude/"$claude"/claude ~/.local/bin/claude; fi
mise reshim 2>/dev/null || true
# A command over ssh runs bash non-interactively, and bash then reads ~/.bashrc instead of BASH_ENV:
# its first line, before the "not interactive: return". Features (oh-my-bash) and users replace ~/.bashrc.
if [[ "$(head -1 ~/.bashrc 2>/dev/null)" != '. /etc/profile.d/devvm-tools.sh' ]]; then
  { echo '. /etc/profile.d/devvm-tools.sh'; cat ~/.bashrc 2>/dev/null || true; } > ~/.bashrc.tmp && mv ~/.bashrc.tmp ~/.bashrc
fi
# "(devvm) " before the prompt. Themes (oh-my-bash's) rebuild PS1 before each prompt from
# PROMPT_COMMAND, and ~/.bashrc.d loads after them, so this hook runs last: an array entry on bash 5.1+
# (where oh-my-bash appends its own), else appended to the string (where oh-my-bash prepends).
mkdir -p ~/.bashrc.d
cat > ~/.bashrc.d/devvm-prompt.sh <<'EOF'
_devvm_prompt() { [[ $PS1 == '(devvm) '* ]] || PS1="(devvm) $PS1"; }
if (( BASH_VERSINFO[0] * 100 + BASH_VERSINFO[1] >= 501 )); then
  PROMPT_COMMAND+=(_devvm_prompt)
else
  PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND;}_devvm_prompt"
fi
EOF
# After root touches a directory on a virtiofs mount, the VM sees it as root's for a few seconds, and
# git refuses the repo ("dubious ownership"). The VM has one user, so git may trust every directory.
git config --global --get-all safe.directory | grep -qx '\*' || git config --global --add safe.directory '*'
[[ -z "$DEVVM_GIT_NAME" ]] || git config --global user.name "$DEVVM_GIT_NAME"
[[ -z "$DEVVM_GIT_EMAIL" ]] || git config --global user.email "$DEVVM_GIT_EMAIL"

[[ -d "$s/home" ]] && cp -a "$s/home/." ~/

while IFS= read -r rel && IFS= read -r mount; do
  [[ -n "$rel" && -e "$mount" ]] || continue
  mkdir -p "$(dirname ~/"$rel")"
  [[ -L ~/"$rel" ]] || rm -rf ~/"$rel"
  ln -sfn "$mount" ~/"$rel"
done < "$s/links"

# merge_claude_json VM HOST COPIED: host settings win, but keep the VM's own per-folder state
# (trust, allowed tools). MCP servers: each of the host's replaces the VM's by that name whole (no
# field of an older copy survives, such as a header the VM's copy now takes out), the ones
# devvm copied last time (COPIED, a JSON list) that the host no longer has go, and servers added
# in the VM stay.
merge_claude_json() {
  local vm=$1 host=$2 copied=$3 prev='[]'
  [[ ! -s $copied ]] || prev=$(cat "$copied")
  [[ -s $vm ]] || echo '{}' > "$vm"
  jq -s --argjson prev "$prev" '
    ((.[0] | del(.mcpServers)) * (.[1] | del(.projects, .mcpServers)))
    + {mcpServers: (((.[0].mcpServers // {}) | with_entries(select(.key as $k | $prev | index($k) | not)))
                    + (.[1].mcpServers // {}))}' "$vm" "$host" > "$vm.tmp"
  mkdir -p "$(dirname "$copied")"
  jq '.mcpServers // {} | keys' "$host" > "$copied.tmp" && mv "$copied.tmp" "$copied"
  mv "$vm.tmp" "$vm"
}
if [[ -f "$s/claude.json" ]]; then
  merge_claude_json ~/.claude.json "$s/claude.json" ~/.local/state/devvm/claude-mcp-copied.json
fi
# The project folder moved since the last setup: Claude Code's sessions and settings, kept by its old
# path, follow it.
# shellcheck source-path=SCRIPTDIR source=claude.sh
. "$s/claude.sh"
if [[ -f "$s/moved" ]]; then
  # shellcheck source=/dev/null
  . "$s/moved"
  # shellcheck disable=SC2154 # the moved file sets them
  claude_project_moved "$moved_from" "$moved_to" "$claude_from" "$claude_to"
fi

# VS Code over Remote-SSH (devvm code): its settings for this machine turn off what reaches the host
# beyond the editor itself: forwarding ports the VM's programs open (to the host's localhost, which
# devvm ports doesn't cover) and answering git's credential prompts with the host's GitHub sign-in.
# The VM can change this file back; it's here so ordinary use is safe.
vsc=~/.vscode-server/data/Machine/settings.json
vsc_off='{"remote.autoForwardPorts": false, "git.terminalAuthentication": false, "github.gitAuthentication": false}'
mkdir -p "$(dirname "$vsc")"
if [[ ! -s $vsc ]]; then
  echo "$vsc_off" | jq . > "$vsc"
elif jq -e 'type == "object"' "$vsc" >/dev/null 2>&1; then
  jq --argjson off "$vsc_off" '. + $off' "$vsc" > "$vsc.tmp" && mv "$vsc.tmp" "$vsc"
else
  echo "devvm: $vsc isn't plain JSON, so VS Code's port forwarding and git sign-in stay as they are there" >&2
fi

# Codex has no env-var opt-out: append the off switches unless the (host-copied) config sets them.
if command -v codex >/dev/null; then
  mkdir -p ~/.codex && touch ~/.codex/config.toml
  grep -q '^\[analytics\]' ~/.codex/config.toml || printf '\n[analytics]\nenabled = false\n' >> ~/.codex/config.toml
  grep -q '^\[otel\]' ~/.codex/config.toml || printf '\n[otel]\nmetrics_exporter = "none"\n' >> ~/.codex/config.toml
  # Its login stays in ~/.codex/auth.json, which holds placeholders, never in a keyring: a top-level
  # key, so it goes first, in place of any other choice.
  { echo 'cli_auth_credentials_store = "file"'; grep -v '^cli_auth_credentials_store[[:space:]]*=' ~/.codex/config.toml || true; } > ~/.codex/config.toml.devvm &&
    mv ~/.codex/config.toml.devvm ~/.codex/config.toml
fi

# opencode 2's database gets the store's opencode logins that changed (opencode.sh); none in the store
# is an empty list, so devvm's entries go. A failed import is tried again at the next launch.
oc_import=$s/opencode-import.json
[[ -f $oc_import ]] || echo '[]' > "$oc_import"
bash -l "$s/opencode.sh" < "$oc_import" >/dev/null ||
  echo "devvm: opencode in this VM didn't get the logins devvm keeps for it; the next launch tries again" >&2

# The VM's proxy (DEVVM_PROXY set): Lima's env: already puts the proxy variables in /etc/environment;
# point the tools that ignore them at the proxy too. When the guest has none, undo all of it.
proxy_block() {   # proxy_block FILE [CONTENT]: replace (or remove) devvm's block in FILE
  local f=$1
  [[ -f "$f" ]] || touch "$f"
  sed -i '/^# devvm-proxy begin/,/^# devvm-proxy end/d' "$f"
  [[ -z "${2:-}" ]] || printf '# devvm-proxy begin\n%s\n# devvm-proxy end\n' "$2" >> "$f"
}
docker_proxies() {   # docker_proxies CONFIG [sudo]: set (or remove) the proxy for containers
  local c=$1 tmp as=()
  [[ -z "${2:-}" ]] || as=(sudo)
  if ! ${as[@]+"${as[@]}"} test -f "$c"; then
    [[ -n "${DEVVM_PROXY:-}" ]] || return 0
    ${as[@]+"${as[@]}"} mkdir -p "$(dirname "$c")"
    echo '{}' | ${as[@]+"${as[@]}"} tee "$c" >/dev/null
  fi
  tmp=$(mktemp)
  if [[ -n "${DEVVM_PROXY:-}" ]]; then
    ${as[@]+"${as[@]}"} jq --arg p "$DEVVM_PROXY" \
      --arg n "$DEVVM_NO_PROXY" '.proxies.default = {httpProxy: $p, httpsProxy: $p, noProxy: $n}' "$c" > "$tmp"
  else
    ${as[@]+"${as[@]}"} jq 'del(.proxies.default)' "$c" > "$tmp"
  fi
  ${as[@]+"${as[@]}"} cp "$tmp" "$c"
  rm -f "$tmp"
}
# nix-daemon (the nix layer) does Nix's downloads, and gets nothing of /etc/environment: under systemd
# a drop-in with the proxy, else its settings file, devvm's alone (OpenRC reads it), both removed
# when the guest has no proxy.
nixd=/nix/var/nix/profiles/default/bin/nix-daemon
nix_conf=/etc/systemd/system/nix-daemon.service.d/devvm-proxy.conf
$systemd || nix_conf=/etc/conf.d/nix-daemon
# The shared Nix cache, also only with a proxy: the proxy's /nix/ first, then cache.nixos.org
# itself, which Nix goes to when the cache doesn't answer. The keys stay Nix's own (cache.nixos.org-1),
# so each path is checked here as well. nix.conf includes devvm's file when it's there.
nix_cache=/etc/nix/devvm-cache.conf
if [[ -x $nixd ]] && ! grep -qx '!include devvm-cache.conf' /etc/nix/nix.conf 2>/dev/null; then
  echo '!include devvm-cache.conf' | sudo tee -a /etc/nix/nix.conf >/dev/null
fi
nix_before=$(cat "$nix_conf" "$nix_cache" 2>/dev/null || true)
if [[ -n "${DEVVM_PROXY:-}" && -x $nixd ]]; then
  echo 'substituters = http://devvm.proxy/nix https://cache.nixos.org' | sudo tee "$nix_cache" >/dev/null
else
  sudo rm -f "$nix_cache"
fi
if [[ -n "${DEVVM_PROXY:-}" && -x $nixd ]]; then
  sudo mkdir -p "$(dirname "$nix_conf")"
  if $systemd; then
    printf '[Service]\nEnvironment="http_proxy=%s" "https_proxy=%s" "no_proxy=%s"\n' \
      "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_NO_PROXY"
  else
    printf 'export http_proxy=%s https_proxy=%s no_proxy=%s\n' "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_NO_PROXY"
  fi | sudo tee "$nix_conf" >/dev/null
else
  sudo rm -f "$nix_conf"
fi
if [[ "$nix_before" != "$(cat "$nix_conf" "$nix_cache" 2>/dev/null || true)" ]]; then
  if $systemd; then sudo systemctl daemon-reload && sudo systemctl try-restart nix-daemon.service
  elif [[ -x $nixd && -e /run/openrc/softlevel ]]; then sudo rc-service nix-daemon restart >/dev/null
  fi
fi
dropin=/etc/systemd/system/docker.service.d/devvm-proxy.conf
$systemd || dropin=/etc/conf.d/docker   # OpenRC: a block in Docker's own settings
# conf_block FILE [CONTENT]: replace (or remove) devvm's block in a root-owned FILE.
conf_block() {
  [[ -f $1 ]] || return 0
  sudo sed -i '/^# devvm-proxy begin/,/^# devvm-proxy end/d' "$1"
  [[ -z "${2:-}" ]] || printf '# devvm-proxy begin\n%s\n# devvm-proxy end\n' "$2" | sudo tee -a "$1" >/dev/null
}
before=$(sudo cat "$dropin" 2>/dev/null || true)
if [[ -n "${DEVVM_PROXY:-}" ]]; then
  echo 'Defaults env_keep += "http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY NODE_USE_ENV_PROXY"' |
    sudo tee /etc/sudoers.d/devvm-proxy >/dev/null
  sudo chmod 440 /etc/sudoers.d/devvm-proxy
  [[ ! -d /etc/apt/apt.conf.d ]] ||
    printf 'Acquire::http::Proxy "%s";\nAcquire::https::Proxy "%s";\n' "$DEVVM_PROXY" "$DEVVM_PROXY" |
      sudo tee /etc/apt/apt.conf.d/95devvm-proxy >/dev/null
  if ! $systemd; then
    # Without PAM (Alpine), shells don't read Lima's /etc/environment.
    printf 'export http_proxy=%s https_proxy=%s HTTP_PROXY=%s HTTPS_PROXY=%s no_proxy=%s NO_PROXY=%s\n' \
      "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_NO_PROXY" "$DEVVM_NO_PROXY" |
      sudo tee /etc/profile.d/devvm-proxy.sh >/dev/null
  fi
  if command -v dockerd >/dev/null; then
    sudo mkdir -p "$(dirname "$dropin")"
    if $systemd; then
      printf '[Service]\nEnvironment="HTTP_PROXY=%s" "HTTPS_PROXY=%s" "NO_PROXY=%s"\n' \
        "$DEVVM_PROXY" "$DEVVM_PROXY" "$DEVVM_NO_PROXY" | sudo tee "$dropin" >/dev/null
    else
      conf_block "$dropin" "export HTTP_PROXY=$DEVVM_PROXY HTTPS_PROXY=$DEVVM_PROXY NO_PROXY=$DEVVM_NO_PROXY"
    fi
  fi
  # Every SSH connection goes through the proxy too, checked against the allowlist and logged.
  # The proxy URL carries the VM's password (http://devvm:KEY@host:port), sent as Proxy-Authorization.
  hp=${DEVVM_PROXY#http://} auth=""
  if [[ $hp == *@* ]]; then auth=$(printf %s "${hp%%@*}" | base64 | tr -d '\n'); hp=${hp#*@}; fi
  { printf '#!/bin/bash\nexec 3<>/dev/tcp/%s/%s || exit 1\nauth=%q\n' "${hp%:*}" "${hp##*:}" "$auth"
    cat <<'SH'
# ssh's ProxyCommand: CONNECT HOST PORT through devvm's proxy. When the proxy
# refuses, its reason (with the host command that allows it) goes to stderr, where ssh shows it.
printf 'CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\nProxy-Authorization: Basic %s\r\n\r\n' "$1" "$2" "$1" "$2" "$auth" >&3
read -r _ code _ <&3
while IFS= read -r line <&3 && [[ -n ${line%$'\r'} ]]; do :; done
if [[ $code != 200 ]]; then
  read -r -t 5 msg <&3 && echo "$msg" >&2
  exit 1
fi
# Both directions. The reader replaces this script, so it ends when ssh signals its ProxyCommand on exit;
# the writer ends when ssh closes its input.
cat <&0 >&3 &
exec cat <&3
SH
  } | sudo tee /usr/local/bin/devvm-ssh-proxy >/dev/null
  sudo chmod 755 /usr/local/bin/devvm-ssh-proxy
  # devvm-local-proxy: the VM's local proxy, for programs that ask for a proxy's password instead
  # of taking it from HTTP_PROXY, such as browsers (the desktop starts it). The VM's user's alone,
  # installed whole, so a connection already running keeps the copy it started with.
  lp=${DEVVM_LOCAL_PROXY:-}
  if [[ -n $lp ]]; then
    { printf '#!/bin/bash\nup_host=%q up_port=%q auth=%q\nlisten=%q port=%q\n' \
        "${hp%:*}" "${hp##*:}" "$auth" "${lp%:*}" "${lp##*:}"
      cat <<'SH'
# The VM's local proxy, for programs that ask for a proxy's password instead of taking it from
# HTTP_PROXY, such as the desktop's browsers. socat listens on $listen alone, the VM's loopback,
# takes connections from the loopback alone, and runs this script on each connection. The script
# reads the request's head and sends it to devvm's proxy, and nowhere else, with the VM's password
# in Proxy-Authorization in place of any the client sent; then socat carries the rest both ways. It
# takes only what a proxy gets: CONNECT HOST:PORT, a tunnel, and a request for an absolute http://
# or https:// URL, whose connection closes after its reply, as the password goes with the head
# alone. A web page that reaches it as a server gets 400.
#   devvm-local-proxy start   starts the listener unless it runs, its log ~/.cache/devvm-local-proxy.log
#   devvm-local-proxy url     prints its URL, for http_proxy and https_proxy
set -u
self=/usr/local/bin/devvm-local-proxy
# refuse STATUS TEXT: answer the client with TEXT, then read what it sent until it closes (or for
# 2 s), so the answer isn't lost to a reset.
refuse() {
  printf 'HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n' \
    "$1" $((${#2} + 1)) "$2"
  exec socat -T 2 -u STDIN /dev/null
}
case ${1:-} in
start)
  d=/run/devvm-local-proxy
  sudo install -d -m 700 -o "$(id -u)" -g "$(id -g)" "$d"
  pid=$(cat "$d/socat.pid" 2>/dev/null || true)
  [[ -n $pid && "$(cat "/proc/$pid/comm" 2>/dev/null)" == socat ]] && exit 0
  mkdir -p ~/.cache
  setsid socat "TCP4-LISTEN:$port,bind=$listen,range=127.0.0.0/8,reuseaddr,fork,max-children=256" \
    "EXEC:$self,nofork" </dev/null >>~/.cache/devvm-local-proxy.log 2>&1 &
  echo "$!" > "$d/socat.pid"
  i=0
  until (: <"/dev/tcp/$listen/$port") 2>/dev/null; do
    i=$((i + 1))
    [[ $i -le 50 ]] || { echo "devvm-local-proxy: socat didn't start: ~/.cache/devvm-local-proxy.log" >&2; exit 1; }
    sleep 0.1
  done
  exit 0 ;;
url) echo "http://$listen:$port"; exit 0 ;;
'') ;;
*) echo "usage: devvm-local-proxy [start|url]" >&2; exit 2 ;;
esac
# One connection, on stdin and stdout.
IFS= read -r -t 30 line || exit 0
line=${line%$'\r'}
re='^([A-Z]+) ([^[:space:]]+) HTTP/1\.[01]$'
[[ $line =~ $re ]] || refuse '400 Bad Request' "devvm-local-proxy: not an HTTP request"
method=${BASH_REMATCH[1]} target=${BASH_REMATCH[2]}
re='^https?://[^[:space:]]+$'
[[ $method != CONNECT ]] || re='^([A-Za-z0-9._-]+|\[[0-9A-Fa-f:.]+\]):[0-9]+$'
[[ $target =~ $re ]] || refuse '400 Bad Request' "devvm-local-proxy: a proxy, for http_proxy and https_proxy, not a web server"
head=$line$'\r\n' n=0
shopt -s nocasematch
while :; do
  IFS= read -r -t 30 h || exit 0
  h=${h%$'\r'}
  [[ -n $h ]] || break
  n=$((n + 1))
  [[ $n -le 100 ]] || refuse '431 Request Header Fields Too Large' "devvm-local-proxy: too many headers"
  case ${h%%:*} in
    proxy-authorization|proxy-connection) continue ;;
    connection|keep-alive) [[ $method == CONNECT ]] || continue ;;
  esac
  head+=$h$'\r\n'
done
shopt -u nocasematch
head+="Proxy-Authorization: Basic $auth"$'\r\n'
[[ $method == CONNECT ]] || head+=$'Connection: close\r\n'
exec 3<>"/dev/tcp/$up_host/$up_port" || refuse '502 Bad Gateway' "devvm-local-proxy: devvm's proxy doesn't answer"
printf '%s\r\n' "$head" >&3
exec socat - FD:3
SH
    } > "$s/local-proxy"
    cmp -s "$s/local-proxy" /usr/local/bin/devvm-local-proxy 2>/dev/null ||
      sudo install -m 700 -o "$(id -u)" -g "$(id -g)" "$s/local-proxy" /usr/local/bin/devvm-local-proxy
  fi
  mkdir -p ~/.ssh
  proxy_block ~/.ssh/config "Host *
  ProxyCommand /usr/local/bin/devvm-ssh-proxy %h %p"
  # devvm-why: what the proxy blocked lately, and how to allow it; then, with transparent mode, what
  # it refused in the VM and the names tools looked up.
  sudo tee /usr/local/bin/devvm-why >/dev/null <<'SH'
#!/bin/sh
# What this VM's proxy blocked recently, and the host command that allows it.
curl -sS --proxy "$HTTP_PROXY" http://devvm.proxy/blocked
if [ -x /usr/local/sbin/devvm-transparent ]; then sudo -n /usr/local/sbin/devvm-transparent status; fi
SH
  sudo chmod 755 /usr/local/bin/devvm-why
  # Before each prompt: what was blocked since the last one (HTTPS clients don't show the proxy's
  # reason), and the host command that allows it. A hook like devvm-prompt.sh's.
  mkdir -p ~/.bashrc.d
  { printf '_devvm_proxy=%q\n' "$DEVVM_PROXY"
    cat <<'EOF'
_devvm_blocked() {
  local out
  out=$(curl -s --max-time 1 --proxy "$_devvm_proxy" "http://devvm.proxy/blocked/new?after=${_devvm_seen:-}") || return 0
  [[ ${out%%$'\n'*} =~ ^[0-9]+$ ]] || return 0
  _devvm_seen=${out%%$'\n'*}
  [[ $out != *$'\n'* ]] || printf '%s\n' "${out#*$'\n'}" >&2
}
if (( BASH_VERSINFO[0] * 100 + BASH_VERSINFO[1] >= 501 )); then
  PROMPT_COMMAND+=(_devvm_blocked)
else
  PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND;}_devvm_blocked"
fi
EOF
  } > ~/.bashrc.d/devvm-blocked.sh
else
  rm -f ~/.bashrc.d/devvm-blocked.sh
  sudo rm -f /etc/sudoers.d/devvm-proxy /etc/apt/apt.conf.d/95devvm-proxy /usr/local/bin/devvm-why \
    /usr/local/bin/devvm-ssh-proxy /usr/local/bin/devvm-local-proxy /etc/profile.d/devvm-proxy.sh
  if $systemd; then sudo rm -f "$dropin"; else conf_block "$dropin"; fi
  [[ ! -f ~/.ssh/config ]] || proxy_block ~/.ssh/config
fi
# Transparent mode (transparent.sh, devvm-transparent) on a confined VM: every name resolves to the
# stand-in address, whose ports 80, 443 and those the rules name go to the proxy, which reads the host
# from the connection. The proxy URL, which holds the password, goes on stdin, never in arguments.
# Off on an open VM. An image from before transparent mode lacks its programs, so names don't resolve.
if [[ -n ${DEVVM_TRANSPARENT_ADDR:-} && -n ${DEVVM_PROXY:-} ]]; then
  rc=0
  printf '%s\n' "$DEVVM_PROXY" | sudo bash "$s/transparent.sh" setup "$DEVVM_TRANSPARENT_ADDR" "$DEVVM_TRANSPARENT_HOST" || rc=$?
  if [[ $rc == 3 ]]; then
    echo "devvm: this VM's image has no dnsmasq, socat or nft, so tools that skip the proxy can't resolve names in it; devvm update makes it again from a newer image" >&2
  elif [[ $rc != 0 ]]; then
    echo "devvm: transparent mode didn't start in this VM, so tools that skip the proxy can't resolve names in it; the next launch tries again" >&2
  fi
else
  sudo bash "$s/transparent.sh" teardown || true
fi
if command -v dockerd >/dev/null; then
  docker_proxies ~/.docker/config.json
  docker_proxies /root/.docker/config.json sudo
  if [[ "$before" != "$(sudo cat "$dropin" 2>/dev/null || true)" ]]; then
    if $systemd; then sudo systemctl daemon-reload && sudo systemctl restart docker
    elif [[ -e /run/openrc/softlevel ]]; then sudo rc-service docker restart >/dev/null
    fi
  fi
fi

# The CAs this VM trusts besides its system's, those in ca/ (ca.sh): the proxy's, which shows its
# own certificates for hosts where it puts tokens in, and the user's own from the host's
# ~/.config/devvm/ca. Trusted only in this VM.
mkdir -p "$s/ca"
sudo sh "$s/ca.sh" "$s/ca"

# GitHub over HTTPS with the token from ~/.config/devvm/env (a placeholder).
if [[ -n "${DEVVM_GITHUB:-}" ]]; then
  git config --global credential.https://github.com.helper \
    '!f() { test "$1" = get || return 0; . ~/.config/devvm/env; printf "username=x-access-token\npassword=%s\n" "$GH_TOKEN"; }; f'
else
  git config --global --unset-all credential.https://github.com.helper 2>/dev/null || true
fi

# devvm's guide for the agents in the VM, and devvm-help, which prints it. Root's and read-only, so
# nothing the VM's user runs changes what other agents read, and installed again when it differs.
guide=/usr/share/doc/devvm/README.md
cmp -s "$s/guide.md" "$guide" 2>/dev/null || sudo install -D -m 644 -o root -g root "$s/guide.md" "$guide"
if [[ "$(cat /usr/local/bin/devvm-help 2>/dev/null)" != "$(printf '#!/bin/sh\nexec cat %s\n' "$guide")" ]]; then
  printf '#!/bin/sh\nexec cat %s\n' "$guide" | sudo tee /usr/local/bin/devvm-help >/dev/null
  sudo chmod 755 /usr/local/bin/devvm-help
fi

# Tell agents why the network may fail, and what to do about it.
note="Network access in this VM goes through devvm's proxy, which allows what the VM's network tier allows. If a download or request fails with a connection error or a 403 from the proxy, run \`devvm-why\` to see what was blocked; the user can allow it on the host with \`devvm network allow <host>\`. A tool that ignores the proxy settings still reaches the proxy over HTTPS and HTTP, and on the ports the rules name, by the names it looks up (transparent mode). A connection refused on another port, or a DNS error where transparent mode is off, while \`devvm-why\` shows nothing blocked means the tool skipped the proxy: turn on its own proxy setting, or give it \`\$HTTPS_PROXY\`."
for f in ~/.claude/CLAUDE.md ~/.codex/AGENTS.md; do
  [[ -d "$(dirname "$f")" ]] || continue
  [[ -f "$f" ]] && sed -i '/^<!-- devvm-proxy -->/,/^<!-- \/devvm-proxy -->/d' "$f"
  [[ -n "${DEVVM_PROXY:-}" ]] || continue
  printf '\n<!-- devvm-proxy -->\n%s\n<!-- /devvm-proxy -->\n' "$note" >> "$f"
done

mkdir -p ~/.config/devvm
install -m 600 "$s/env" ~/.config/devvm/env
grep -qs 'devvm/env' ~/.profile || echo '[ -f ~/.config/devvm/env ] && . ~/.config/devvm/env' >> ~/.profile
