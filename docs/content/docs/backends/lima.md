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

## The project folder's cache

On Linux, QEMU shares the project folder with the VM over 9p, and the VM's kernel can cache the folder's files as it reads and writes them. devvm gives every new VM the cache mode `none`, so the VM reads and writes straight through to the folder on this machine, and what you edit on either side is what the other side sees.

Lima's own default for a writable folder is `mmap`, which caches the files' contents in the VM, and Linux's 9p client loses data with it in two ways. When a program reads a file's size while another write is making the file longer, the VM can take the older size from this machine and drop the new data, even after `fsync` succeeded, so the file ends up empty or cut to 4096 bytes, and Claude Code's edits are one way to set it off. And when a program locks a file it has written, its next read of that file returns zeros. That stops SQLite at its second write with `file is not a database`, whatever its journal mode, and Gradle's second build with `Unexpected lock protocol found in lock file`.

A VM made before devvm chose `none` keeps `mmap`, and devvm doesn't change its mount on its own, so each launch of such a VM warns until you switch it:

```sh
devvm mount-cache        # how this folder's VM caches its project folder
devvm mount-cache none   # switch it to none from the VM's next start
```

`devvm mount-cache none` switches the mode from the VM's next start, so in a terminal devvm offers to restart a running VM now. `none` is the only mode devvm sets, so any other value is refused.

A [`--clone` VM](../using/clone.md) mounts no project folder, so it has no cache mode until `devvm clone off` mounts the folder, which then gets `none`. On macOS, VMs share the project folder with virtiofs, which has no cache mode to choose, so there `devvm mount-cache` refuses. On Linux it refuses the same for a VM whose project folder isn't a 9p mount, such as one whose Lima config you gave another mount type.

### When the cache mode changes

The VM gets `none` at its next start, and that start mounts the project folder again once the VM runs, before devvm sets the VM up or opens its shell. The VM's first mount of the folder at boot still has the old mode, because the guest reads its new mount options only after it has mounted the folder. devvm then checks the folder:

- When the folder is mounted with `none`, the VM goes on as usual.
- When the folder is still mounted with the old mode, isn't mounted at all, or the VM doesn't answer the check, devvm restarts the VM once, before it sets it up or opens its shell. The folder can be left with the old mode when the VM couldn't unmount it, because something it started at boot, such as a container, was using it, or when the VM's start was degraded. It can be left unmounted when the VM unmounted it but couldn't mount it again, as happens on an Alpine VM. The restart's boot mounts the folder with `none`, and devvm checks it again.

After the restart, devvm checks the folder once more and doesn't restart the VM again:

- When the folder is mounted with `none`, the VM goes on as usual.
- When the folder is still mounted with the old mode, devvm warns and goes on, though the folder can lose writes under that mode.
- When the folder isn't mounted at all, or the VM doesn't answer the check, devvm stops the VM before it sets it up or opens its shell, because files written there would go to the VM's own disk and vanish once the folder is mounted over them.

In both cases a restart of your own repeats what failed, so look first at why the VM's boot didn't mount the folder with `none`. Its console log, which [A VM won't boot](../using/troubleshooting.md#a-vm-wont-boot) shows where to find, has what the boot said, and the run log has what the VM's `umount` and `mount` said and why devvm restarted the VM ([logs](../using/troubleshooting.md#a-command-fails)).

When devvm can't stop the VM for the restart, a folder still mounted with the old mode is warned of and the VM goes on as it runs. Otherwise the launch fails, saying the VM didn't stop, because it may still be running. Pressing Ctrl+C while devvm mounts or checks the folder, or stops the VM for the restart, stops the VM, without a restart. A launch into a running VM checks too that its project folder is mounted, and refuses to open a shell in one whose folder isn't, naming `devvm restart`.

### devvm mount-cache --json

`devvm mount-cache --json` prints the VM's mode, whether it was chosen, as the VM was made or with `devvm mount-cache none`, rather than kept from Lima, and the mode its next start gives it, when that differs:

```json
{
  "version": 1,
  "mode": "mmap",
  "chosen": true,
  "pending": "none"
}
```

### Programs that map files for writing

With `none`, a program in the VM can't map a file in the project folder for shared writing, or map one it opened for writing at all, and it fails at once with `Invalid argument`, so nothing is lost. LMDB, numpy's `memmap` in `r+` or `w+` mode, Java's read-write `MappedByteBuffer` and the mold linker do this, while git, npm, Vite, webpack, esbuild, jest, Go, cargo with Rust's own linker, GNU ld, lld, Gradle, DuckDB, ccache and Terraform work as they do on any disk. LMDB is what Parcel's `.parcel-cache` and Gatsby's cache use.

For a program that maps its files, keep them on the VM's own disk, such as in its home folder: for mold, a `CARGO_TARGET_DIR` there, or lld instead. Or use a [`--clone` VM](../using/clone.md), whose repo is on the VM's own disk. The VM's own disk is also far faster than the shared folder, so a `--clone` VM suits large repos and heavy builds: on a Fedora 43 host, `npm ci` of 369 packages took 4.8 s in the shared folder and 2.8 s on the VM's disk, a debug `cargo build` of ripgrep 8.5 s and 4.8 s, and a cold `git status` of an 18,000-file tree 23 s and 1 s.

### SQLite and the cache

SQLite's default journal works in the project folder. WAL mode doesn't, because SQLite maps its shared index and fails with `disk I/O error`, unless the connection sets `PRAGMA locking_mode=EXCLUSIVE` before `PRAGMA journal_mode=WAL`. That keeps the index in memory, and lets only that connection use the database. A project whose database is in WAL mode works in a `--clone` VM, or with the database outside the project folder, such as in the VM's home folder or `/tmp`.

## Cleaning up

`devvm gc` deletes devvm's stale and unfinished Lima images and the shared tools that no VM has and that aren't current, and it leaves your VMs as they are. `devvm images rm` deletes an image that's current, except on Linux hosts one whose disk VMs use ([cleaning up](../using/images.md#cleaning-up)). Lima also keeps a download cache of its own, which holds Ubuntu's cloud images that devvm's builds downloaded, and which your own Lima VMs share. `devvm gc --caches` deletes the cloud images that devvm downloads and that no image of devvm's names, after listing them and asking, and devvm never runs `limactl prune`, which empties the whole cache ([what devvm keeps](../using/space.md#lima)).

## Limits

- **Folder names in VM names are cut short.** Lima keeps each VM's ssh socket in the VM's folder in its home, and a socket path has fewer than 104 characters. A long Lima home leaves less room for the name, so set `DEVVM_LIMA_HOME` to a shorter folder if devvm says the home is too long, or that a VM's name leaves no room for the folder's name ([VM names](../internals/backends.md#vm-names)).
- **Paths can't contain commas.** Lima and QEMU split mount options on commas, so devvm refuses to mount a project folder whose path has one, and it leaves out an agent's folder whose path has one, with a warning.
- **Disks can grow but not shrink.**
- **Mounts on Linux are 9p.** A 9p share mounts once per guest, file watchers in the VM miss changes in its subfolders ([file watchers](#file-watchers)), and SQLite's WAL mode doesn't work in the project folder, which a `--clone` VM avoids ([SQLite and the cache](#sqlite-and-the-cache)).
- **A VM that Lima reports as failed to start** still starts, with a warning, when it's running but degraded because one of Lima's boot scripts failed. A VM that doesn't answer over ssh within a minute is stopped instead, and its console log is in its Lima folder (`serial*.log`).
