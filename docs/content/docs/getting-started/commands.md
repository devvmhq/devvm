---
title: Everyday commands
explainer: The commands you use most, in their groups
weight: 3
---

This page lists devvm's commands in the groups that `devvm --help` shows them in, and each row links to the page with the details. `devvm COMMAND --help` shows a command's examples and flags, [the command line](../using/command-line.md) covers what works the same in every command, and the [CLI reference](../cli/_index.md) has every command with all its flags.

## Your VM

| | |
| --- | --- |
| `devvm` | open a shell in this folder's VM, at the same path, making the VM first with the toolchains, agents and layers you choose, or that `--tools`, `--agents` and `--layers` name ([details](../using/images.md#what-a-new-vm-gets)) |
| `devvm restart` | stop this folder's VM, start it again and open a shell in it, for changes that apply only when it starts ([details](../using/vms.md#restarting-and-stopping)) |
| `devvm stop` | stop this folder's VM ([details](../using/vms.md#restarting-and-stopping)) |
| `devvm update [--force] [--no-export] [--clean-home]` | move this folder's VM to the newest shared tools, and when its image or its `devcontainer.json` has changed, make it again from the image's new build and the file as it is now, keeping its home folder and settings, after asking, then open a shell in it ([details](../using/images.md#outdated-vms)) |
| `devvm rm [--force] [--no-export] [--no-clone-check]` | delete this folder's VM after asking, exporting its agents' history first unless `--no-export` ([details](../using/vms.md#deleting-a-vm)) |
| `devvm rm VM [--force] [--no-export] [--no-clone-check]` | delete a VM whose folder is gone, by its name or ssh alias, after asking ([details](../using/vms.md#a-vm-whose-folder-is-gone)), or one whose folder is on a disk that isn't connected, only after asking in a terminal ([details](../using/vms.md#a-folder-on-a-disk-that-isnt-connected)) |
| `devvm adopt` | use the VM made for an earlier folder at this path here ([details](../using/vms.md#a-folder-made-again)), or the VM made for this folder before you moved or renamed it ([details](../using/vms.md#a-moved-or-renamed-folder)) |
| `devvm adopt VM` | move a VM whose folder is gone to this folder, such as a copy of that folder on another disk, after asking ([details](../using/vms.md#a-moved-or-renamed-folder)) |
| `devvm ls` | list the project VMs ([details](../using/vms.md#listing-vms)) |
| `devvm code` | get this folder's VM ready as `devvm host` does, then open its folder in VS Code over Remote-SSH ([VS Code](../using/ssh.md#vs-code)) |
| `devvm desktop [--clipboard] [--no-open]` | serve this folder's VM's desktop on `127.0.0.1` with its password until Ctrl+C, and open it in Screen Sharing on macOS ([details](../using/desktop.md)) |
| `devvm host` | get this folder's VM ready for ssh and editors, and show the host name they reach it by, such as `devvm-website` ([details](../using/ssh.md#devvm-host-and-devvm-code)) |
| `devvm history` | list the exports of agents' history that devvm saved in `~/.local/share/devvm/exports` ([details](../using/coding-agents.md#history)) |
| `devvm history export [FILE]` | save the history of this folder's VM's agents to FILE, or to a new `.tar.gz` in `~/.local/share/devvm/exports` ([details](../using/coding-agents.md#history)) |
| `devvm history import [FILE]` | put the agents' history in FILE, or in the newest export devvm saved for this folder, into this folder's VM ([details](../using/coding-agents.md#importing-history)) |

In a folder without a VM of its own, each of these commands but `devvm adopt` uses the VM of the nearest folder with one that it's inside, and says which VM that is. `--here` and `--parent` choose which VM a command uses ([a subfolder](../using/vms.md#a-subfolder)).

These options are for a new VM, and only `devvm` takes them. A VM that exists ignores them with a warning ([what a new VM gets](../using/images.md#what-a-new-vm-gets)).

| | |
| --- | --- |
| `--tools LIST\|none` | the toolchains the VM gets, such as `--tools node,python`, in place of those the folder's files call for ([details](../using/images.md#what-a-new-vm-gets)) |
| `--agents LIST\|none` | the coding agents the VM gets, such as `--agents claude,codex`, in place of those `devvm setup` chose ([details](../using/images.md#what-a-new-vm-gets)) |
| `--layers LIST\|none` | the layers the VM's image gets, bottom first, such as `--layers docker`, in place of those `devvm setup` chose ([details](../using/images.md#what-a-new-vm-gets)) |
| `--network TIER` | create the VM on a [network tier](../safety/network.md#network-tiers): `open` reaches anything this machine can, `internet` any public host, `allowlist`, the default, only the hosts you allow, and `none` only its agents' and secrets' hosts and its links |
| `--clone` | create the VM with its own clone of the repo instead of mounting the folder ([details](../using/clone.md)) |
| `--no-git-protect` | create the VM without `.git` protection |
| `--ssh-agent` | create the VM forwarding your SSH agent, so while you're connected, anything in it can use your keys ([details](../using/ssh.md#agent-forwarding)) |
| `--unsafe-folder` | mount a folder that devvm refuses, such as one that is or holds your home folder ([details](../using/settings.md#project-folders-devvm-refuses)) |
| `--cpus N`, `--memory GiB`, `--disk GiB` | the VM's size ([details](../using/ports-size.md#size-and-ports)) |
| `--ports all\|none\|LIST` | which ports the VM forwards ([details](../using/ports-size.md#size-and-ports)) |
| `--from IMAGE` | start its base from a container image, such as `node:24`, instead of Ubuntu's cloud image ([details](../using/from.md)) |
| `--no-devcontainer` | create the VM without reading the folder's `devcontainer.json`, which otherwise describes the VM as far as devvm supports it, with the options you give taking priority ([details](../using/devcontainer.md)) |

### Working on a clone

| | |
| --- | --- |
| `devvm --clone` | make this folder's VM with its own clone of the repo, copy your branches into it and open a shell in the clone ([details](../using/clone.md)) |
| `git fetch devvm` | on this machine, fetch the VM's work as `devvm/<branch>`, to review before you merge it ([details](../using/clone.md#fetching-the-vms-work)) |
| `devvm rm` | delete the VM, first offering to keep the work you haven't fetched as `saved/devvm/<branch>` branches ([details](../using/clone.md#deleting-the-vm)) |
| `devvm clone on\|off` | make an existing VM again with its own clone of the repo, or with the folder mounted once devvm has offered to keep the clone's work as `devvm rm` does ([details](../using/clone.md#switching-a-vm-that-exists)) |

## Change your VM

| | |
| --- | --- |
| `devvm tools` | in a terminal, choose this folder's VM's toolchains and agents in a list, then open a shell in it, and otherwise show its shared tools, each with its version and any newer one ([details](../using/toolchains.md#adding-and-removing-tools)) |
| `devvm tools add TOOL...` | add shared tools to this folder's VM, such as `devvm tools add python claude`, then open a shell in it ([details](../using/toolchains.md#adding-and-removing-tools)) |
| `devvm tools rm TOOL...` | take shared tools out of this folder's VM, such as `devvm tools rm python claude`, then open a shell in it |
| `devvm tools update` | move this folder's VM to the newest shared tools, and only those, then open a shell in it ([details](../using/toolchains.md#adding-and-removing-tools)) |
| `devvm size [--cpus N] [--memory GiB] [--disk GiB]` | show or change this folder's VM's CPUs, memory and disk ([details](../using/ports-size.md#size)) |
| `devvm size -g ...` | show or change the size new VMs get ([details](../using/ports-size.md#size)) |
| `devvm ports [all\|none\|LIST]` | show or change which ports this folder's VM forwards to this machine's `localhost`, even while it runs ([details](../using/ports-size.md#ports)) |
| `devvm ports add\|rm LIST` | forward more ports, or stop forwarding some ([details](../using/ports-size.md#ports)) |
| `devvm ports -g ...` | show or change what new VMs forward ([details](../using/ports-size.md#ports)) |
| `devvm network [open\|internet\|allowlist\|none]` | show this folder's VM's tier, `open`, `internet`, `allowlist`, `none` or `unenforced`, and then its allowlist, file by file, or give it a tier: between the confined tiers at once, and into or out of open from its next start, offering to restart a running VM now ([details](../safety/network.md#choosing-a-tier)) |
| `devvm network -g [open\|internet\|allowlist\|none]` | show or change new VMs' tier, which is `allowlist` until you change it ([details](../safety/network.md#choosing-a-tier)) |
| `devvm network allow\|deny\|rm RULE... [-g]` | add allow or deny rules to this folder's VM's own, or remove them, at once, and with `-g`, change yours for every VM ([details](../safety/network.md#changing-rules)) |
| `devvm network log [--follow] [-n N] [--blocked]` | show what this folder's VM connected to and what its proxy blocked ([details](../safety/network.md#logs)) |
| `devvm ssh-agent [on\|devvm\|off]` | show this folder's VM's SSH agent forwarding, which is `off`, `on` for yours or `devvm` for devvm's own, then the keys devvm's own agent holds and those added to it, or change the forwarding from the VM's next start ([details](../using/ssh.md#agent-forwarding)) |
| `devvm ssh-agent add KEY_FILE [--confirm] [--lifetime DURATION]` | load a key into devvm's own agent, and load it again whenever a VM needs it ([details](../using/ssh.md#devvms-own-agent)) |
| `devvm ssh-agent rm KEY_FILE` | take a key out of devvm's own agent, so devvm doesn't load it again ([details](../using/ssh.md#devvms-own-agent)) |
| `devvm git-protect [on\|strict\|off]` | show or change this folder's VM's `.git` protection, while it's stopped ([details](../safety/git-protection.md#turning-it-on-or-off)) |
| `devvm clone [on\|off] [--force] [--no-export] [--clean-home] [--no-clone-check]` | show whether this folder's VM has its own clone of the repo, or make it again with one, or with the folder mounted, keeping its home folder and settings, after asking, then open a shell in it ([details](../using/clone.md#switching-a-vm-that-exists)) |
| `devvm secret`, `devvm secret rm NAME` | list the secrets, whether each is saved and the VMs using it, or delete a saved one ([details](../safety/network.md#saving-and-binding-secrets)) |
| `devvm secret set claude\|github [--stdin]` | save your Claude Code token or GitHub token on this machine, so that VMs get a placeholder and their proxy puts the secret in ([details](../safety/network.md#saving-and-binding-secrets)) |
| `devvm secret use\|unuse NAME` | let this folder's VM use a secret, or stop it, though a VM with Claude Code always uses `claude` ([details](../safety/network.md#saving-and-binding-secrets)) |
| `devvm layer add LAYER... [--force] [--no-export] [--clean-home]` | make this folder's VM again with more layers on top, keeping its home folder and settings, after asking, then open a shell in it ([details](../using/images.md#adding-layers-to-a-vm)) |
| `devvm layer rm LAYER... [--force] [--no-export] [--clean-home]` | make this folder's VM again without some of its layers, keeping its home folder and settings, after asking, then open a shell in it ([details](../using/images.md#taking-layers-out-of-a-vm)) |

## Links between VMs

| | |
| --- | --- |
| `devvm link` | list this folder's VM's links, each with its name, port, target VM and folder, the target's status, and whether the link applies ([details](../using/links.md#listing-links)) |
| `devvm link add TARGET PORT [--as NAME]` | link this folder's VM to PORT of the VM that TARGET names, so programs in it reach the port as `NAME:PORT` and `NAME.devvm.internal:PORT` ([details](../using/links.md#adding-a-link)) |
| `devvm link rm TARGET PORT` | remove one of this folder's VM's links ([details](../using/links.md#removing-a-link)) |
| `devvm link publish [PORTS]` | show the ports this folder's VM publishes, or offer PORTS to links from any of your other VMs ([details](../using/links.md#publishing-a-port)) |
| `devvm link unpublish PORTS` | stop offering ports to links ([details](../using/links.md#publishing-a-port)) |
| `devvm group` | show the group this folder's VM is in, with each of its VMs, the name the others reach it by, its folder and the ports it publishes ([details](../using/links.md#groups)) |
| `devvm group join NAME [--as NAME]` | put this folder's VM in the group NAME, which its first VM makes, so each VM in the group reaches the ports the others publish ([details](../using/links.md#joining-a-group)) |
| `devvm group leave` | take this folder's VM out of its group, removing the links the group made ([details](../using/links.md#leaving-a-group)) |

## Images

| | |
| --- | --- |
| `devvm images` | list the images, then the shared tool images ([details](../using/images.md#listing-images)) |
| `devvm images rm IMAGE\|KEY...` | delete images, by name or by the key `devvm images` shows ([details](../using/images.md#deleting-images)) |
| `devvm build IMAGE` | build an image and the images under it, unless they're built, such as `devvm build docker` or `devvm build foo+bar`, where IMAGE is base or layers and never a shared tool ([details](../using/images.md#layers)) |
| `devvm rebuild IMAGE` | build an image of base or layers again from its parent as it is, and delete the stale images no VM uses, while shared tools follow the tools lock instead ([details](../using/images.md#rebuilding)) |
| `devvm layer new NAME` | start a layer of your own, `~/.config/devvm/layers/NAME.sh`, with its allowlist, `NAME.policy` ([details](../using/customizing.md#your-own-layers)) |

`devvm build`, `devvm rebuild` and `devvm images rm` take `--from IMAGE` to work on a base made from that container image ([details](../using/from.md)).

## Disk space

| | |
| --- | --- |
| `devvm space` | show what devvm keeps on this machine, the disk each kind takes, and what frees it ([details](../using/space.md#devvm-space)) |
| `devvm gc` | delete the stale and unfinished images and the tool images no VM uses, after listing them, and never a VM ([details](../using/images.md#cleaning-up)) |
| `devvm retention [KEY=VALUE...]` | show the retention settings, which say what devvm deletes on its own, or change them, with `KEY=off` turning one off ([details](../using/settings.md#retention)) |

## This machine

| | |
| --- | --- |
| `devvm setup [--features LIST\|none] [--agents LIST\|none] [--layers LIST\|none]` | choose base's features and the coding agents and layers new VMs get, and in a terminal also new VMs' size and network tier ([details](../using/settings.md#devvm-setup)) |
| `devvm agent import [AGENT...]` | copy the agents' settings and logins from your home folder into devvm's agent store, which every VM gets them from, never their history, asking before it replaces the store's ([details](../using/coding-agents.md#importing-from-this-machine)) |
| `devvm agent login AGENT [--setup-token] [--device-code] [--api-key]` | log Claude Code in to your Claude subscription inside this folder's VM, or get a Claude Code token with `--setup-token` ([details](../using/coding-agents.md#claude-code-login)), or log Codex or opencode in to ChatGPT there, in a browser on this machine or with a code, or save an API key, and keep the login in devvm's agent store for every VM ([details](../using/coding-agents.md#logging-in-with-devvm-agent-login)) |
| `devvm agent save [AGENT...]` | copy the agents' settings from this folder's VM into devvm's agent store, never their logins or history, after showing what changed and asking ([details](../using/coding-agents.md#saving-a-vms-settings)) |
| `devvm doctor` | check what devvm needs on this machine and say how to get what's missing ([details](install.md#checking-what-devvm-needs)) |
| `devvm upgrade [--check] [--to VERSION]` | install devvm's newest release the way you installed devvm, after asking, or with `--check` only say whether a newer one is out ([details](../using/upgrading.md#upgrading)) |
| `devvm completion SHELL` | print the shell completion script for `bash`, `zsh` or `fish` ([details](../using/command-line.md#shell-completion)) |
| `devvm limactl ARGS...` | run Lima's `limactl` on the VMs and images in devvm's own Lima home, not `~/.lima`, for debugging, which starts VMs unconfined, whatever their tier ([details](../backends/lima.md#running-limactl)) |

## devvm itself

| | |
| --- | --- |
| `devvm help [COMMAND]` | print the help for devvm or a command, as `--help` and `-h` do ([details](../using/command-line.md#help)) |
| `devvm --version` | print devvm's version alone, as `devvm version` also does |

The folders devvm keeps its state in and the environment variables it reads are in [Settings and files](../using/settings.md).
