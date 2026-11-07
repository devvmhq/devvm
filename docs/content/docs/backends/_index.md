---
title: Backends
explainer: The program that runs devvm's VMs
weight: 40
---

A backend is the program that runs devvm's VMs and builds their images. devvm decides what goes into a VM, such as its image, its folders, its size and its network tier, and the backend runs it. [Lima](lima.md) is devvm's backend.

## What Lima gives you

| | Lima |
| --- | --- |
| Each project gets | a full VM with a kernel of its own |
| Hosts | macOS or Linux, on arm64 or x86_64 |
| You need | Lima, and on Linux QEMU and access to `/dev/kvm` ([requirements](lima.md#requirements), with the versions) |
| Confined network tiers | {{< yes >}} yes |
| The docker layer | {{< yes >}} yes |
| The nix layer | {{< yes >}} yes |
| Limits | paths with commas, and shrinking a disk ([limits](lima.md#limits)) |

`.git` protection uses your machine's file flags, so it doesn't depend on the backend.

An image's key includes its backend, so a backend never uses an image another backend built.

`devvm doctor` lists Lima's features, which are what Lima does itself, what devvm builds from other parts of it, and what it can't do. These are backend features, not the base image features you choose with `devvm setup`, even though `devvm doctor` calls both "features". [How it works › Backends](../internals/backends.md) explains them.

## A VM's backend

Every new VM runs on Lima, and no flag or setting chooses another backend. A VM's state records its backend, and the VM stays on that backend for as long as it exists. When a VM's state names a backend this devvm doesn't have, such as one a different build of devvm made, devvm stops and says so, and you use the devvm that made the VM.

The `--json` output of `devvm ls`, `devvm images` and `devvm space` names each VM's, image's and shared tool's backend in a `backend` field, and `devvm doctor --json` names the backend it checks.

## When a backend lacks something

When a VM needs something its backend can't do, devvm stops and says so in one line, naming the VM and what to do. It never falls back to something weaker, so a VM on a confined tier never starts with an open network.

That holds for a new VM's tier too, though allowlist is the default. When a backend can't confine a new VM to its proxy, the VM can only be open, so devvm refuses to make it unless you give `--network open`, and it says so in one line ([network tiers](../safety/network.md#network-tiers)).
