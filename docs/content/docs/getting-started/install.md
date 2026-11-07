---
title: Installing
explainer: Install devvm and check what it needs
weight: 1
---

This page gets devvm installed and checked on your machine. [Upgrading and uninstalling](../using/upgrading.md) covers what comes later.

## Installing devvm

Run the command for your system:

{{< install >}}

Install devvm one way, with Homebrew or with the install script, but not both ([one devvm](../using/upgrading.md#one-devvm)). The install script needs `curl`, `tar`, and `sha256sum` or `shasum`, it never needs sudo for devvm itself, and it checks the download's checksum and signature before it installs anything ([Installing and updating](../internals/install.md)). To check the script itself against devvm's release key before you run it, follow [checking the install script](../internals/install.md#checking-the-install-script). It takes options after `bash -s --`, as in `curl -fsSL … | bash -s -- --yes`:

| Option | What it does |
| --- | --- |
| `--yes` | answer yes to every question |
| `--version 0.1.0`, or `DEVVM_VERSION=0.1.0` | install that release |
| `--no-deps` | install devvm only |
| `--force` | install even when Homebrew installed devvm, which leaves you with two copies |
| `DEVVM_PREFIX=/some/dir` | install under that folder instead of `~/.local` |

## Requirements

devvm runs its VMs on [Lima](../backends/lima.md#requirements), its default [backend](../backends/_index.md). It's tested on Apple silicon Macs and on Linux on both arm64 and x86_64, but not yet on Intel Macs ([Known limits](../using/limits.md#intel-macs)). The last column says whether the install script offers to install a requirement for you.

{{< os-tabs >}}
{{< os-tab name="macOS" >}}

| You need | To check | To get it | The install script |
| --- | --- | --- | --- |
| macOS at [Lima's minimum](../backends/lima.md#requirements), on arm64 or x86_64 | `sw_vers` and `uname -m` | | {{< no >}} doesn't |
| 8 GiB of memory and 4 CPUs, recommended | `sysctl hw.memsize hw.ncpu` | | {{< no >}} doesn't |
| Lima at [its minimum](../backends/lima.md#requirements) | `limactl --version` | `brew install lima`, or [Lima's install guide](https://lima-vm.io/docs/installation/) | {{< yes >}} offers it: Homebrew's when you have Homebrew, otherwise Lima's latest release into `~/.local` |
| OpenSSH 8.7 or newer, for ssh and editors by name ([details](../using/ssh.md#ssh-access-by-name)) | `ssh -V` | macOS 13's has it | {{< no >}} doesn't |
| Docker Desktop, only for `--from` with an [image only Docker has](../using/from.md#images-only-docker-has) | `docker version` | Docker Desktop | {{< no >}} doesn't |

{{< /os-tab >}}
{{< os-tab name="Linux" >}}

| You need | To check | To get it | The install script |
| --- | --- | --- | --- |
| Linux on arm64 or x86_64 | `uname -m` | | {{< no >}} doesn't |
| 8 GiB of memory and 4 CPUs, recommended | `free -g` and `nproc` | | {{< no >}} doesn't |
| Lima at [its minimum](../backends/lima.md#requirements) | `limactl --version` | `brew install lima`, or [Lima's install guide](https://lima-vm.io/docs/installation/) | {{< yes >}} offers it: Homebrew's when you have Homebrew, otherwise Lima's latest release into `~/.local` |
| QEMU | `qemu-img --version` | your package manager: apt, dnf, pacman or zypper | {{< yes >}} offers it, with sudo |
| Access to `/dev/kvm` | `ls -l /dev/kvm` | `sudo usermod -aG kvm $USER`, then log out and back in | {{< yes >}} offers it, with sudo |
| OpenSSH 8.7 or newer, for ssh and editors by name ([details](../using/ssh.md#ssh-access-by-name)) | `ssh -V` | Ubuntu 22.04's has it | {{< no >}} doesn't |
| Docker's engine, only for `--from` with an [image only Docker has](../using/from.md#images-only-docker-has) | `docker version` | Docker's engine | {{< no >}} doesn't |

{{< /os-tab >}}
{{< /os-tabs >}}

- **A smaller machine works too.** devvm gives base fewer CPUs and less memory there ([size and ports](../using/ports-size.md#size-and-ports)).
- **Your first VM needs 8 GiB of free disk space**, for the base image, the VM's own image and the shared tools ([free space](../using/ports-size.md#free-space)).
- **Everything else goes inside the VMs,** including the toolchains, the coding agents and Docker for your projects ([docker layer](../using/images.md#built-in-layers)), so you don't install Claude Code, Codex or opencode on this machine ([devvm's agent store](../using/coding-agents.md#devvms-agent-store)).
- **Linux hosts** have a few limits, such as no `.git` protection ([Known limits](../using/limits.md#linux-hosts)).

## Checking what devvm needs

Run `devvm doctor`:

```sh
devvm doctor
```

It lists everything that devvm's backend, Lima, needs and doesn't have, each with what to do, then its warnings, such as a machine with less than the recommended memory, and it fails only when something is missing. devvm never installs anything on this machine itself, and every command that needs the backend checks it first and stops at the first thing that's missing. [What devvm doctor checks](../internals/install.md#what-devvm-doctor-checks) lists each check and warning.

When `devvm doctor` finds nothing missing, you're ready for [your first VM](first-vm.md). Before you make it, read [staying safe](../safety/_index.md#the-habits), which lists the habits that close most of what devvm leaves open.
