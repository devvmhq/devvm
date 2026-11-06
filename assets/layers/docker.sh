#!/bin/bash
# docker: a built-in layer (devvm --layers docker). Docker's engine, CLI, BuildKit and Compose, the VM's
# user in the docker group, the daemon started at each boot.
set -euxo pipefail
: "${devvm_libc:?}"   # devvm's prelude (prelude.sh) comes before this script
# Pinned downloads (scripts/bump.sh updates them). Docker's repository has no snapshots: its packages
# are pinned by version.
docker_version=29.9.3
containerd_version=2.4.2
buildx_version=0.38.1
compose_version=5.7.0
model_version=1.2.9
docker_gpg_sha256=1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570

# Alpine (--from alpine): Docker is Alpine's own package, from the release's repositories, which have
# no snapshots: not pinned.
if [[ $devvm_libc == musl ]]; then
  sudo apk add --no-cache docker docker-cli-buildx docker-cli-compose
  echo "devvm: Alpine's Docker packages aren't pinned" >&2
  sudo rc-update add docker default
  sudo addgroup "$USER" docker
else
  export DEBIAN_FRONTEND=noninteractive
  # Docker from its signed apt repository (the key's checksum pinned), each package at the repository's
  # build of the pinned version for this distribution.
  # shellcheck source=/dev/null
  . /etc/os-release
  case " $ID ${ID_LIKE:-} " in
    *" ubuntu "*) dist=ubuntu codename=${UBUNTU_CODENAME:-$VERSION_CODENAME} ;;
    *)            dist=debian codename=$VERSION_CODENAME ;;
  esac
  devvm-fetch "https://download.docker.com/linux/$dist/gpg" "$docker_gpg_sha256" /tmp/docker.asc
  sudo install -D -m 644 /tmp/docker.asc /etc/apt/keyrings/docker.asc
  rm /tmp/docker.asc
  printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
    "$dist" "$codename" | sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null
  # The package servers answer 5xx now and then: try again twice, after 15 and 30 s.
  for i in 1 2 3; do
    sudo apt-get update --error-on=any && break
    (( i < 3 )) || { echo "devvm: couldn't update the package lists (above)" >&2; exit 1; }
    sleep $((i * 15))
  done
  pkgver() {   # pkgver PACKAGE VERSION: the repository's full version for it (epoch and build suffix)
    local v
    v=$(apt-cache madison "$1" | awk -v want="$2" '{ v = $3; sub(/^[0-9]+:/, "", v) } index(v, want "-") == 1 { print $3; exit }')
    [[ -n $v ]] || { echo "devvm: Docker's repository has no $1 $2 for $dist $codename" >&2; exit 1; }
    echo "$1=$v"
  }
  sudo apt-get install -y \
    "$(pkgver docker-ce "$docker_version")" "$(pkgver docker-ce-cli "$docker_version")" \
    "$(pkgver docker-ce-rootless-extras "$docker_version")" "$(pkgver containerd.io "$containerd_version")" \
    "$(pkgver docker-buildx-plugin "$buildx_version")" "$(pkgver docker-compose-plugin "$compose_version")" \
    "$(pkgver docker-model-plugin "$model_version")"
  sudo usermod -aG docker "$USER"
fi

# Running now too, for the rest of the build: systemd started it with the package, and OpenRC
# starts it here.
if [[ -d /run/openrc ]]; then
  sudo rc-service docker start
fi
