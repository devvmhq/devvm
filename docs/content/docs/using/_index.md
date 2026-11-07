---
title: Using devvm
explainer: What you change in your VMs as you work
weight: 20
---

These pages cover what you change as you work, such as the images VMs are made from, their ports and size, your coding agents, and the editors you connect with.

- [Coding agents](coding-agents.md): the supported agents, which are Claude Code, Codex and opencode, the config they get in a VM, their logins, and their history.
- [Toolchains](toolchains.md): the list of toolchains, with what each brings, the files that choose it, its version files, its global installs and its allowlist.
- [SSH and editors](ssh.md): reaching VMs by name from ssh and VS Code, and forwarding your SSH agent.
- [Ports and size](ports-size.md): the ports a VM forwards to your machine, and its CPUs, memory and disk.
- [Managing VMs](vms.md): restarting, stopping, deleting and listing VMs, and what devvm does in a subfolder or when a project folder is made again, moved, deleted or on a disk that isn't connected.
- [Images and tools](images.md): base, the desktop, docker and nix layers and your own layers, how the shared tools work, which image a new VM gets, and rebuilding, listing and deleting images.
- [Customizing images](customizing.md): your own layers, and the optional features in every VM, such as oh-my-bash and fzf.
- [Settings and files](settings.md): your settings and the commands that write them, devvm's state, and the environment variables devvm reads.
- [Working on a clone](clone.md): `--clone`, which gives a VM its own clone of the repo instead of mounting the folder.
- [devcontainer.json](devcontainer.md): what a new VM takes from a project's `devcontainer.json`, and what devvm refuses.
- [Links between VMs](links.md): `devvm link`, which lets a program in one VM reach a service in another by name, as in `postgres://db:5432/app`, once that VM publishes the port.
- [A VM's desktop](desktop.md): the `desktop` layer, and opening a VM's desktop in Screen Sharing with `devvm desktop`.
- [Starting from a container image](from.md): `--from`, which starts base from `node:24`, `alpine:3` or your own image.
- [What devvm keeps on this machine](space.md): everything devvm, its tools and its backends keep outside your project folders, and how each is cleaned up.
- [The command line](command-line.md): the help, flags and their values, the questions devvm asks and what answers them, `--json` for scripts, and shell completion.
- [Upgrading and uninstalling](upgrading.md): `devvm upgrade`, switching between Homebrew and the install script, and removing devvm with everything it keeps.
- [Troubleshooting](troubleshooting.md): what to do when a command or a build fails, a request is blocked, or a VM won't start.
- [Known limits](limits.md): what devvm can't do, in one list.
