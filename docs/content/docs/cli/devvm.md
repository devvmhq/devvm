---
title: devvm
explainer: "Open a shell in this folder's VM, creating it first"
---

Open a shell in this folder's VM, creating it first

## Synopsis

Open a shell in this folder's VM, at the same path, and create the VM first when the folder has none. devvm shows what a new VM gets, and in a terminal you can customize its toolchains, coding agents, layers, tier, ports and size, which --tools, --agents, --layers, --network, --ports, --cpus, --memory and --disk choose without asking. The VM mounts only this folder read-write, and with --clone it gets its own clone of the repo instead.

```text
devvm [flags]
```

## Examples

```text
  devvm                                             # open this folder's VM, choosing what a new one gets
  devvm --agents claude                             # create the VM with Claude Code and the toolchains the folder needs
  devvm --tools node --agents none --layers docker  # create the VM with node and the docker layer alone
  devvm --clone                                     # create the VM with its own clone of this repo
  devvm ls                                          # list every VM
```

## Options

```text
      --agents LIST       the LIST of coding agents a new VM gets, such as claude,codex, or none (default: devvm setup's)
      --clone             create the VM with its own clone of this repo, or of each repo in this folder, instead of mounting the folder
      --cpus N            the N CPUs the VM gets (default: devvm size -g, else the image's)
      --disk GiB          the VM's disk in GiB, at least the image's (default: devvm size -g, else the image's)
      --from IMAGE        the container IMAGE base starts from, such as node:24, instead of Ubuntu's cloud image (default: $DEVVM_FROM)
  -h, --help              help for devvm
      --here              use this folder's own VM, never that of a folder it's inside
      --layers LIST       the LIST of layers a new VM's image gets, bottom first, such as docker,mine, or none (default: devvm setup's)
      --memory GiB        the VM's memory in GiB, such as 16 or 16G (default: devvm size -g, else the image's)
      --network TIER      the network TIER a new VM gets: open, internet, allowlist or none, or learn for internet while it learns the hosts it needs (default: devvm network -g's, else allowlist)
      --no-devcontainer   create the VM without reading the folder's devcontainer.json
      --no-git-protect    create the VM without .git protection
      --no-input          never ask, and fail where devvm would ask, naming the flag that answers it
      --parent            use the VM of the nearest folder this one is inside, even across a git repo of its own
      --ports PORTS       the PORTS the VM forwards: all, none or a list like 5173,8000-8010 (default: devvm ports -g, else all)
      --repos LIST        the repos a --clone VM of a folder of repos gets: all, none or a LIST of their paths, such as frontend,backend (default: a picker in a terminal)
      --ssh-agent         create the VM with your SSH agent forwarded, so anything in it can use your keys while you're connected
      --ssh-config        add devvm's Include line to ~/.ssh/config without asking
      --tools LIST        the LIST of toolchains a new VM gets, such as node,python, or none (default: those the folder's files call for)
      --unsafe-folder     mount a folder devvm refuses, such as your home folder
  -v, --verbose           show the output of the commands devvm runs instead of a status line
      --version           print devvm's version
```

## Docs

[The docs page for devvm](../getting-started/commands.md) explains it in full.

## See also

- [devvm adopt](devvm_adopt.md): Give this folder the VM made for an earlier folder or path
- [devvm agent](devvm_agent.md): Fill devvm's store of the agents' settings and logins that every VM gets
- [devvm build](devvm_build.md): Build an image and the images under it, unless they're built
- [devvm clone](devvm_clone.md): Show or change whether this folder's VM has its own clone of the repo
- [devvm code](devvm_code.md): Open this folder's VM in VS Code over Remote-SSH
- [devvm completion](devvm_completion.md): Print the shell completion script for bash, zsh or fish
- [devvm desktop](devvm_desktop.md): Open this folder's VM's desktop in a VNC viewer
- [devvm doctor](devvm_doctor.md): Check what devvm needs on this machine
- [devvm gc](devvm_gc.md): Delete stale images, unused tools, old files and the Nix cache over its cap
- [devvm git-protect](devvm_git-protect.md): Show or change .git protection for this folder's VM
- [devvm group](devvm_group.md): Show the group this folder's VM is in
- [devvm history](devvm_history.md): List the saved exports of agents' history
- [devvm host](devvm_host.md): Get this folder's VM ready for ssh and editors, and show its host name
- [devvm images](devvm_images.md): List the images and the shared tools
- [devvm layer](devvm_layer.md): Make image layers of your own, or change this folder's VM's layers
- [devvm limactl](devvm_limactl.md): Run Lima's limactl for debugging, which starts VMs unconfined, whatever their tier
- [devvm link](devvm_link.md): List this folder's VM's links to your other VMs
- [devvm ls](devvm_ls.md): List the project VMs
- [devvm mount-cache](devvm_mount-cache.md): Show or change how this folder's VM caches its project folder
- [devvm network](devvm_network.md): Show or change the network tier of this folder's VM
- [devvm paste](devvm_paste.md): Send the clipboard's image, PDF or copied file to this folder's VM
- [devvm ports](devvm_ports.md): Show or change which ports this folder's VM forwards
- [devvm rebuild](devvm_rebuild.md): Build an image again, and delete the old one
- [devvm restart](devvm_restart.md): Restart this folder's VM and open a shell in it
- [devvm retention](devvm_retention.md): Show or change what devvm deletes on its own
- [devvm rm](devvm_rm.md): Delete this folder's VM, or a VM whose folder is gone
- [devvm secret](devvm_secret.md): List the secrets kept on this machine, for VMs to use
- [devvm setup](devvm_setup.md): Choose base's features, and new VMs' size, network tier, coding agents and layers
- [devvm size](devvm_size.md): Show or change this folder's VM's size
- [devvm space](devvm_space.md): Show what devvm keeps on this machine and the space it takes
- [devvm ssh-agent](devvm_ssh-agent.md): Show or change SSH agent forwarding for this folder's VM
- [devvm stop](devvm_stop.md): Stop this folder's VM
- [devvm tools](devvm_tools.md): Show or choose this folder's VM's shared tools
- [devvm update](devvm_update.md): Bring this folder's VM up to date, then open a shell in it
- [devvm upgrade](devvm_upgrade.md): Install devvm's newest release
