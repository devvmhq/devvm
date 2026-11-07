---
title: Known limits
explainer: What devvm can't do, and where
weight: 18
---

These are the things devvm can't do, everywhere, on Linux hosts and on Intel Macs.

## Everywhere

- **Shared tools are read-only,** so a tool can't update itself or add components, for example with `rustup component add` ([details](images.md#shared-tools)).
- **A layer goes in and comes out by making the VM again.** A layer's script changes the VM's disk, so `devvm layer add` gives a VM more layers by making it again, and everything on its disk but its home folder is lost ([details](images.md#adding-layers-to-a-vm)). `devvm layer rm` takes layers out in the same way ([details](images.md#taking-layers-out-of-a-vm)). Shared tools go in with `devvm tools add` and come out with `devvm tools rm`, and the VM's disk stays as it is ([details](toolchains.md#adding-and-removing-tools)).
- **`--from` takes only Debian-, Ubuntu- and Alpine-based images,** and Alpine, Debian 12 (`node:24`) and Ubuntu before 26.04 on arm64 build with unpinned packages ([details](from.md#supported-images)).
- **Agent folders** that you make or delete on this machine reach a running VM only after a restart, which devvm offers ([details](coding-agents.md#mounts)).
- **Ports** are forwarded over TCP only, and forwarding every port means every port from 1024 up. You can tunnel UDP over a forwarded TCP port with socat ([details](ports-size.md#ports)).
- **Links between VMs** carry TCP over IPv4 alone, and a container in a VM reaches them only with `--network host` ([details](links.md#ports)).
- **Changing a running VM's ports** closes the connections that are open through its forwards ([details](ports-size.md#ports)).
- **With Lima,** project paths can't contain commas, folder names in VM names are cut short, and disks grow but never shrink ([details](../backends/lima.md#limits)).
- **File watchers in a Lima VM** don't see the edits you make on this machine, only those made in the VM, so set a dev server that should reload on your edits to poll the project folder ([details](../backends/lima.md#file-watchers)).

## Linux hosts

- They have no `.git` protection, so use `--clone` there ([details](../safety/git-protection.md#linux-hosts)).
- VMs mount the project folder with 9p instead of virtiofs, so file watchers in a VM also miss the VM's own changes in the folder's subfolders, and need polling there ([details](../backends/lima.md#file-watchers)).
- Set-id bits that root in a VM sets on files in the project folder reach the host, and devvm names such files when it makes a VM and once a VM is stopped ([what to do](troubleshooting.md#devvm-names-set-id-files)).
- devvm keeps your secrets in files under `~/.local/share/devvm/secrets/`, which only you can read, instead of the Keychain ([details](settings.md#devvms-state)).
- devvm has no Time Machine to keep VMs' CAs out of, so keep `~/.local/share/devvm/vms` out of your backups ([details](../internals/security.md#secrets-stay-on-the-host)).

## Intel Macs

devvm has a release for Intel Macs, which the install script and Homebrew install, but nobody has tested it on one yet. The tested systems are Apple silicon Macs and Linux on arm64 and x86_64, so on an Intel Mac, VMs, `--from` and the network tiers' confinement may fail in ways that devvm's tests haven't caught.
