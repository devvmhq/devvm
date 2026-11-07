---
title: Lima
explainer: What Lima needs, and how devvm uses it
weight: 1
---

[Lima](https://lima-vm.io) is devvm's backend, so every new VM runs on it ([a VM's backend](_index.md#a-vms-backend)). Each project gets a full VM with a kernel of its own, which runs on Apple's Virtualization framework (`vz`) on macOS and on QEMU on Linux. [How it works › Lima](../internals/lima.md) explains how devvm sets up, confines and clones Lima's VMs.

## Requirements

{{< os-tabs >}}
{{< os-tab name="macOS" >}}

- macOS 13 or newer, on arm64 or x86_64.
- Lima 2.2 or newer, with `limactl` on your `PATH`. Install it with Homebrew, `brew install lima`, or see [Lima's install guide](https://lima-vm.io/docs/installation/).

{{< /os-tab >}}
{{< os-tab name="Linux" >}}

- Linux on arm64 or x86_64.
- Lima 2.2 or newer, with `limactl` on your `PATH`. Install it with Homebrew, `brew install lima`, or see [Lima's install guide](https://lima-vm.io/docs/installation/).
- QEMU from your package manager: `qemu-system-aarch64` (or `qemu-system-x86_64`) and `qemu-img`.
- Read and write access to `/dev/kvm`. That means virtualization is turned on in the firmware (or nested virtualization, in a VM), and your user is in the `kvm` group.

{{< /os-tab >}}
{{< /os-tabs >}}

Every command that needs Lima checks these first and stops at the first one that's missing. `devvm doctor` lists them all, each with the command that installs it where there's one to run. That's Homebrew's command for Lima when you have Homebrew, your package manager's for QEMU (apt, dnf, pacman or zypper), and `sudo usermod -aG kvm $USER` for `/dev/kvm`, after which you log in again. devvm never runs them itself ([details](../getting-started/install.md#checking-what-devvm-needs)).

## devvm's Lima home

devvm keeps its VMs and images in a Lima home of its own, `~/.local/share/devvm/lima` (under `$XDG_DATA_HOME` when that's set), whatever `LIMA_HOME` says. Your own Lima VMs stay where they are, and Lima settings you made for them don't reach devvm's VMs. Because of this, a plain `limactl list` doesn't show devvm's VMs, so run `devvm limactl list` instead ([below](#running-limactl)). To keep devvm's VMs somewhere else, such as a sandbox or a shorter path, set `DEVVM_LIMA_HOME` to that folder.

Lima adds `_config/default.yaml` and `_config/override.yaml` from its home to every VM it starts, so devvm refuses to start a VM while devvm's Lima home holds either file, and you move them out of it to go on. devvm also refuses a VM that has a network devvm didn't give it, because a second network would get around the VM's proxy, so remove the `networks:` entries from the file that devvm names ([why](../internals/lima.md#a-lima-home-of-its-own)).

## Running limactl

`devvm limactl ARGS...` runs `limactl` with ARGS on devvm's Lima home, in devvm's place. Its input, output, terminal and exit code are `limactl`'s, and devvm passes every option on to it, `--help` included. `devvm help limactl` shows devvm's own help for the command.

```sh
devvm limactl list
devvm limactl shell --start devvm-img-<key>
```

- It runs from devvm's data folder, as devvm's own `limactl` commands do, so give files by their absolute path.
- It runs without your SSH agent, so a VM never gets your agent through it, whatever its [SSH agent](../using/ssh.md#agent-forwarding) choice says ([why](../internals/security.md)).
- A VM you start this way, with `devvm limactl start` or `shell --start`, runs unconfined, whatever its tier, and `devvm ls` shows it as `unenforced` ([confinement](../internals/lima.md#confinement)). Start project VMs with devvm.

## File watchers

A dev server or a watcher that runs in the VM, such as Vite, webpack, `tsc --watch` or nodemon, learns about changed files from the VM's kernel. The kernel sees every change made in the VM, so a file that a coding agent or a command in the VM's shell writes reaches the watcher at once. Lima's mounts don't pass on the changes you make on this machine, though, so a watcher in the VM misses a file you save in your editor here, and the dev server doesn't reload. On Linux the project folder is a 9p mount, and there a watcher also misses the VM's own changes in the folder's subfolders, and sees only those to files at the top of the folder.

To have a watcher see your edits, set it to poll the project folder, which costs some CPU in a large project:

| Tool | Setting |
| --- | --- |
| chokidar, and tools built on it | `CHOKIDAR_USEPOLLING=1` |
| Vite | `server.watch.usePolling: true` in `vite.config` |
| webpack | `watchOptions.poll` in `webpack.config.js`, or `WATCHPACK_POLLING=true` |
| nodemon | `nodemon --legacy-watch` |
| `tsc --watch` | `"watchOptions": { "watchFile": "fixedPollingInterval" }` in `tsconfig.json` |
| cargo watch | `cargo watch --poll` |

Set the variables in the VM's shell or in your project's scripts, before the tool starts. Leave Lima's own file event setting, `mountInotify`, off, because with it on, commands in the VM that write to the project folder, such as `git init`, fail at random ([details](../internals/lima.md#what-devvm-sets-on-a-vm)).

## Cleaning up

`devvm gc` deletes devvm's stale and unfinished Lima images and the shared tools that no VM has and that aren't current, and it leaves your VMs as they are. `devvm images rm` deletes an image that's current, except on Linux hosts one whose disk VMs use ([cleaning up](../using/images.md#cleaning-up)). Lima also keeps a download cache of its own, which holds Ubuntu's cloud images that devvm's builds downloaded, and which your own Lima VMs share. `devvm gc --caches` deletes the cloud images that devvm downloads and that no image of devvm's names, after listing them and asking, and devvm never runs `limactl prune`, which empties the whole cache ([what devvm keeps](../using/space.md#lima)).

## Limits

- **Folder names in VM names are cut short.** Lima keeps each VM's ssh socket in the VM's folder in its home, and a socket path has fewer than 104 characters. A long Lima home leaves less room for the name, so set `DEVVM_LIMA_HOME` to a shorter folder if devvm says the home is too long, or that a VM's name leaves no room for the folder's name ([VM names](../internals/backends.md#vm-names)).
- **Paths can't contain commas.** Lima and QEMU split mount options on commas, so devvm refuses to mount a project folder whose path has one, and it leaves out an agent's folder whose path has one, with a warning.
- **Disks can grow but not shrink.**
- **Mounts on Linux are 9p.** A 9p share mounts once per guest, and file watchers in the VM miss changes in its subfolders ([file watchers](#file-watchers)).
- **A VM that Lima reports as failed to start** still starts, with a warning, when it's running but degraded because one of Lima's boot scripts failed. A VM that doesn't answer over ssh within a minute is stopped instead, and its console log is in its Lima folder (`serial*.log`).
