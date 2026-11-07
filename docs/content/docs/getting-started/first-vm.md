---
title: Your first VM
explainer: Make a project's VM and start an agent in it
weight: 2
---

This page assumes you've [installed devvm](install.md) and that `devvm doctor` finds nothing missing.

## Making the VM

1. **Run devvm in a project folder.**

   ```sh
   cd ~/some-project
   devvm
   ```

2. **Answer the first-run questions.** The first time, devvm asks for the CPUs and memory new VMs get, the coding agents they get, and the optional extras the base image gets, such as oh-my-bash and fzf. These set devvm's defaults for every new VM, and `devvm setup` changes them later ([devvm setup](../using/settings.md#devvm-setup)).

3. **Check the summary.** devvm shows what this folder's new VM gets: the toolchains your folder's files call for, the agents and layers you chose for new VMs, its size, its network tier and its ports.

   ```text
   ==> This folder's new VM, devvm-some-project-71a99b1076d92c, gets:
     toolchain node, for package.json
     agent claude, from devvm setup
     layers: none
     size: 4 CPUs, 8 GiB of memory and a 100 GiB disk
     network tier: allowlist
     ports: all
   Customize it? [y/N]
   ```

4. **Confirm, or customize it.** Confirm to make the VM as shown, or customize it to choose its toolchains, coding agents, layers, network tier, ports and size first ([what a new VM gets](../using/images.md#what-a-new-vm-gets)). Flags such as `--agents claude` choose a part beforehand, and devvm asks nothing when the flags choose everything.

   > [!TIP]
   > Keep allowlist, the default, unless the VM needs more. The allowlist [network tier](../safety/network.md) limits the VM to the hosts you allow, and every tier keeps your secrets on your machine.

5. **Wait for the base image.** The first time, devvm builds it with the extras you chose, which takes a few minutes once, and later VMs start in seconds ([images](../using/images.md)).

   ```console
   ==> building image base (e9f517dddef1)
       a few minutes, and Ubuntu's cloud image to download the first time
       (details: ~/.local/share/devvm/logs/runs/20261024-090616-83541.log)
   ==> image base ready
   ==> creating devvm-some-project-71a99b1076d92c from base
   ==> starting devvm-some-project-71a99b1076d92c
   ```

Then devvm opens a shell in the VM at the same path, `~/some-project`, with `(devvm)` at the start of the prompt.

## Claude Code's login

If the VM has Claude Code, log it in to your Claude account once. Run this in a terminal on this machine, not the VM's shell, in the project folder:

```sh
devvm agent login claude
```

Open the address it shows in a browser, approve the login, and paste the code the browser shows. devvm keeps the login on this machine for every VM with Claude Code, and the VMs get only a placeholder ([Claude Code login](../using/coding-agents.md#claude-code-login)). If you already use an agent on this machine, `devvm agent import` copies its settings and logins into devvm's agent store instead ([devvm's agent store](../using/coding-agents.md#devvms-agent-store)).

## Starting the agent

Start the agent in the VM's shell, so it runs in the VM:

```console
~/some-project $ devvm
(devvm) ~/some-project $ claude
```

Run `claude`, `codex` or `opencode`, whichever the VM has ([running an agent](../using/coding-agents.md#running-an-agent)).

## What's in the VM

- **Your project folder**, read-write, so your editor and the VM share the same files. It's the only folder of yours the VM sees, and its `.git` hooks and config are locked ([.git protection](../safety/git-protection.md)).
- **The toolchains**, ready at once ([toolchains](../using/toolchains.md)).
- **Your git name and email**, so commits made in the VM are yours.
- **Ports**: a dev server on `:5173` in the VM opens at `localhost:5173` on your machine ([ports](../using/ports-size.md#ports)).
- **sudo** without a password, so `apt install` works as on any Ubuntu machine.

## Working on a clone instead

To keep the VM out of your working tree and `.git` entirely, make it with its own clone of the repo:

```sh
devvm --clone                 # a VM with its own clone of your branches
git fetch devvm               # later, on this machine: the VM's work as devvm/<branch>
git log -p main..devvm/main   # review it before you merge it
```

Use it for an agent working unattended, a repo you don't trust, or on Linux hosts, which have no `.git` protection ([working on a clone](../using/clone.md)).

## Coming back to it

The VM belongs to its folder, and its subfolders use it too ([a subfolder](../using/vms.md#a-subfolder)):

```sh
devvm                   # a shell in this folder's VM, started if it's stopped
devvm tools             # choose its toolchains and agents
devvm tools add claude  # add Claude Code to this folder's VM
devvm code              # open it in VS Code
devvm stop              # stop it
devvm ls                # list every VM
devvm rm                # delete it after asking, saving its agents' history first
```

[Everyday commands](commands.md) lists the rest, and [managing VMs](../using/vms.md) covers restarting, deleting and moving VMs. Before you let an agent loose in the VM, read [staying safe](../safety/_index.md#the-habits).
