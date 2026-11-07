---
title: Starting from a container image
weight: 13
---

By default `base` starts from Ubuntu 26.04's cloud image. `--from IMAGE` starts it from a Debian-, Ubuntu- or Alpine-based container image instead, such as `node:24`, `python:3.13`, `debian:13`, `alpine:3` or an image you built yourself. Use it when your project needs a distribution or a system package set that Ubuntu doesn't give you.

```sh
devvm --from node:24                 # a new VM whose base is node:24
devvm build --from alpine:3 base     # just the image
DEVVM_FROM=node:24 devvm             # a default for every new VM and build
```

Everything above `base` builds and runs as usual, including base's own setup and the layers (`docker` and yours). The VM gets its [shared tools](images.md#shared-tools) as any VM does, and [shared tools and glibc](#shared-tools-and-glibc) describes where devvm builds them. devvm keeps the images from each container image apart, so each `base` and the images on it sit side by side with the others, and each image's record keeps the container image its base came from and that image's digest.

`--from` applies to new VMs and to `devvm build` and `devvm rebuild`, and `devvm images rm` takes it to delete an image on a base from that container image. An existing VM keeps what it was made from, and `devvm tools add` and `devvm tools update` build its shared tools for that `base`. devvm ignores another `--from` for an existing VM, with a warning.

[Images and caching](../internals/images.md#how-its-converted) explains how devvm turns a container image into a VM's disk and how it treats the image's code as untrusted.

## Tags and digests

devvm builds `base` from the exact image that a tag names when it builds, which is the image's digest, and it records that digest:

- When the tag moves to a new image, such as a new `node:24`, the next build makes a new `base`. That build happens for a new VM with `--from`, `devvm build`, `devvm rebuild`, `devvm layer add` or `devvm update`. The old `base` is then stale, and the VMs made from it are outdated, which `devvm update` changes ([outdated VMs](images.md#outdated-vms)).
- Offline, or when the registry doesn't have the image any more, devvm uses the digest it recorded last, with a warning. With none recorded, it fails.
- Opening an existing VM never looks anything up, because only builds do.
- `--from node:24@sha256:<digest>` pins an exact image, and devvm never looks it up.

devvm reads a reference the way Docker does, so `node:24` is Docker Hub's `library/node` with the tag `24`. A first part with a dot or a port, or `localhost`, names the registry, as in `ghcr.io/org/image:tag` or `localhost:5000/app`. A reference without a tag means `latest`. [Digests](../internals/images.md#digests) explains how devvm asks a registry for them.

## Images only Docker has

Public images work from any registry, such as Docker Hub, ghcr.io or quay.io. When a registry doesn't give devvm the image, devvm takes it from Docker, as long as Docker runs and has it. That covers an image you built and never pushed, and a private one, which you get with `docker login` and then `docker pull`. devvm itself never logs in to a registry, and it never pulls, builds or deletes Docker's images.

If the image's tag moves to another image while devvm builds, for example because you rebuilt it, devvm stops and asks you to run it again.

## Supported images

- **Debian, Ubuntu and Alpine** and the distributions based on them. devvm refuses any other, and an image without a shell or `/etc/os-release`.
- **This machine's architecture.** devvm refuses an image without an arm64 build (or amd64), and names the ones it has.
- **Alpine** uses OpenRC and musl, so its VMs get musl builds of the shared tools, built on that image and kept apart for it. Its base also gets GNU `tar` from Alpine's packages in place of busybox's, because devvm sends the VM's setup and [imports agents' history](coding-agents.md#importing-history) with it.
- **Unpinned packages.** While `base` builds, its packages come from the container image's own sources as they are then, apt's or Alpine's, and so do the [desktop layer](images.md#desktop)'s, the [docker layer](images.md#docker)'s on Alpine and the [nix layer](images.md#nix)'s. devvm keeps no copy of them, unlike base from Ubuntu's image, so building the same base again installs the sources' packages of that day, and it needs the network for them.
- **A glibc new enough for your shared tools.** A Debian or Ubuntu image's glibc must be at least as new as each shared tool needs, which [shared tools and glibc](#shared-tools-and-glibc) covers.
- **Tested.** The end-to-end tests make VMs from `node:24` (Debian 12, with node, python, claude and the docker and nix layers) and `alpine:3` (with node, dotnet and the docker, nix and desktop layers), and build `base` from `ubuntu:26.04`.

## Shared tools and glibc

A shared tool's build is mounted by every VM that gets the same version of the tool, so devvm never builds one where a container image's code could change it for other VMs:

- **Debian and Ubuntu images:** their VMs get the same builds of the shared tools as VMs on Ubuntu's own image, which devvm builds in its own `base` from Ubuntu's cloud image, never in the container image. When that `base` isn't built yet, devvm says so and builds it first, as it does for a VM on Ubuntu's image.
- **Alpine images:** musl builds need Alpine, so devvm builds them in the VM's own `base` and keeps them for the VMs on that exact image, by its digest. When the image's tag moves to a new image, devvm builds the tools it gives those VMs next again, on the new image.

The [shared tools](images.md#shared-tools) are their projects' own prebuilt releases, and each one is built against a glibc version that it needs at least. devvm reads that version from each tool's files when it builds the tool's image, and it reads the container image's glibc version when it converts the image. When a Debian- or Ubuntu-based image has an older glibc than a tool needs, devvm refuses to give that tool to the VM. The error names the tool, the glibc version it needs and the one the image has, and devvm doesn't make the VM. The same happens when `devvm tools add`, `devvm tools update` or `devvm update` would give an existing VM such a tool.

To fix it, start from a newer container image with `--from`, or leave that tool out. Today no shared tool needs a glibc newer than 2.28, so Debian 10 and Ubuntu 20.04 and newer images run all of them, and [toolchains](toolchains.md#which-bases-they-run-on) lists what each one needs. devvm measures it again for every new version it builds. Alpine-based images get musl builds of the tools, which need no glibc. devvm doesn't check base from Ubuntu's own image, whose glibc is newer than any shared tool needs.

## Backends

Lima starts `base` from a container image by converting the image into a disk ([backends](../internals/backends.md#features)).
