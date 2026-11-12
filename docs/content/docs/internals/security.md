---
title: Security model
explainer: What devvm protects, how, and what it doesn't
weight: 7
---

devvm treats everything in a VM as untrusted, including a coding agent with root, because the VM's user has passwordless sudo. This page explains how devvm keeps that code away from the rest of your machine. [Staying safe](../safety/_index.md) lists [what a VM can reach](../safety/_index.md#what-a-vm-can-reach) and the habits that [keep you safe](../safety/_index.md#the-habits), and [Attack vectors](../safety/attack-vectors.md) goes through common attacks one by one and lists what devvm leaves open.

All of it rests on the VM's boundary.

## What devvm protects, and how

### Only the project folder

A VM mounts its project folder read-write at the same path. The only other things it mounts from the host are the shared tools and its [agents' config folders](../using/coding-agents.md#mounts), both read-only, so it never mounts your home folder, which Lima's own templates do. A [`--clone`](../using/clone.md) VM doesn't mount the project folder either. It has its own clone, and its work reaches the repo only when you fetch it. When you run devvm in a subfolder and it uses the project folder's VM, that VM mounts nothing more than it already did ([a subfolder's VM](overview.md#a-subfolders-vm)).

devvm refuses a project folder that would hand the VM more than the project:

- A folder that is or holds your home folder, devvm's settings (`~/.config/devvm`), its state (`~/.local/share/devvm`, which holds its binaries and other VMs' disks) or its Lima home (`unsafe_folder`).
- A folder inside devvm's own folders (`unsafe_folder_inside`). A VM in `~/.config/devvm/layers` could rewrite the scripts that every image build runs, and a VM in devvm's data folder could rewrite the files devvm runs on the host.

`--unsafe-folder` mounts such a folder anyway, and devvm says so. A `--clone` VM mounts nothing, so devvm doesn't refuse its folder.

### One VM per folder

A VM belongs to the folder it was made for, which devvm knows by its path and by the folder itself: its device and inode, and its birth time where the file system keeps one. When you delete a folder and make it again at the same path, or clone another repo where an old one was, devvm refuses to give the new folder the old folder's VM, so the new folder never quietly gets what's in the old VM's disk. `devvm rm` deletes the old VM and `devvm adopt` gives it to the new folder, and each does so only after you confirm ([a folder made again](../using/vms.md#a-folder-made-again)). When you move or rename the folder, the folder at its new path is the same folder, so devvm offers it the VM, and moves the VM only after you confirm ([a moved or renamed folder](../using/vms.md#a-moved-or-renamed-folder)). It never offers a VM to a copy, to a folder another user owns, or on a file system that may give a new folder the inode number of a deleted one, so a VM never moves to a folder that only looks like its own.

### A project's devcontainer.json

A project's [`devcontainer.json`](../using/devcontainer.md) belongs to the project, and so to the VM, which can rewrite it for the folder's next VM, or for the VM itself when you run `devvm update`. devvm reads it only when it makes a VM, new or again, and uses only what it can do inside the VM. Nothing in it runs on this machine, so `initializeCommand` never runs. Nothing in it reaches this machine's files, because devvm doesn't follow a link out of the folder, or this machine's environment, because devvm refuses `${localEnv:…}` before it makes anything. It can't add a mount or change the VM's network tier or other confinement.

Its commands run in the VM as the VM's user, confined like anything else in it. Its features can only be devvm's own layers, so devvm never fetches or runs a feature's script, and it refuses `docker-outside-of-docker`. devvm uses its `image` only when the image's registry gives it to anyone who asks anonymously, so it never uses a container image that only your Docker has or that only your registry login reaches. It refuses a registry named `localhost` or by an address that isn't the internet's, and its lookup connects only to the internet's addresses, so it never uses an image on this machine or its network, except through a company proxy, which resolves the registry's name itself ([attack vectors](../safety/attack-vectors.md#spreading-beyond-the-vm)).

An existing VM never changes when the file does, until you run `devvm update`, which says it reads the file again and asks before it makes the VM again. The file read again is held to the same refusals as a new VM's, and it can't replace a base you chose with `--from`, bring back a port you took out with `devvm ports rm`, or change its network tier, size, SSH agent or `.git` protection, which devvm keeps.

Its `forwardPorts` are added to the VM's ports, as `devvm ports add` adds them, even beyond a list you narrowed, both when devvm makes the VM and when `devvm update` reads the file again. They go through the one port forwarder, so they listen on this machine's loopback alone (127.0.0.1 and ::1), never take a port that a program on this machine has, and never take a port another VM forwards ([port forwarding](ports.md)). What a cloned repo's file opens is so only what `devvm ports add` could open, but it reaches whatever on this machine connects to those ports, a browser's pages included ([attack vectors](../safety/attack-vectors.md#network)).

### Read-only and confined builds

devvm builds images with nothing from the host mounted, and it confines their builds ([details](lima.md#confinement)). Every build reaches the internet. On macOS it can't reach the host's loopback, but it reaches your LAN, the host's own address on it included, and on Linux it reaches the host's loopback too. Credentials never go into images.

A tool build writes into a folder of its own and never into the shared tools, and devvm checks each tool image, moves it in and writes its record, with its hash ([details](images.md#building-tools)). Tool images for glibc, which any VM may mount, build only in devvm's own Ubuntu base, never in a `--from` image, and musl ones are keyed by the digest of the `--from` base they build in, so a container image's code never reaches another VM's tools. devvm's VMs live in a Lima home of their own, so Lima settings you made for other VMs never reach them. devvm also refuses a VM with a network it didn't give the VM, because that network would reach around the proxy ([details](lima.md#a-lima-home-of-its-own)).

### `.git` protection

The VM can't change the parts of `.git` that run code on the host, because devvm locks hooks and config. On macOS it sets a flag the VM can't clear, and on Linux it makes them read-only in the user namespace that QEMU runs in. A Linux host whose kernel doesn't let devvm use a user namespace has no locks, and devvm only watches `.git` there ([details](git-protection.md)).

### Network tiers

A VM on a confined tier, internet, allowlist or none, reaches nothing but its own proxy on the host and the [links](ports.md#links-between-vms) you added to it. On macOS its Lima process runs under a macOS sandbox, and on Linux QEMU's network is restricted ([details](lima.md#confinement)). Even root in the VM can't get around that. A link's connections don't use the VM's network, because this machine carries them over ssh, and only to a port that another VM on the same tier or a stricter one publishes. An open VM runs on its backend's own network, and its proxy applies no rules.

The proxy applies the tier's rules, and it refuses the host's own addresses and internal addresses unless a rule names them ([the proxy](network.md#the-proxy)). The one thing that VMs share through their proxies is the [shared Nix cache](network.md#the-shared-nix-cache), for those with the nix layer. Only the host writes it, with packages from cache.nixos.org whose signature it checked, and nothing a VM sends is stored, so a VM can make the host fetch but can't change what another VM gets. After each start, devvm checks that the VM still runs confined and can't connect out directly, and it stops a VM that fails the check instead of opening it ([the self-check](network.md#the-self-check)).

### ssh into VMs

ssh and editors reach a running VM by name through the entries that devvm writes ([ssh access by name](../using/ssh.md#ssh-access-by-name)). Each entry is built so that ssh connects only to that VM and the connection gives the VM nothing else of this machine's:

- **Each entry checks the VM's own host keys,** which devvm keeps in the VM's folder, `~/.local/share/devvm/vms/<hash>/run/known_hosts`, as a record of the VM's current start. devvm reads them from the VM through its backend, instead of over the network, once each start, after the VM runs, and the record names the start it was read for, so the keys of an earlier start are read again rather than trusted, whoever started the VM. devvm also removes the record before each start of its own. A VM whose Lima config changed gets new host keys when it boots. A VM whose keys devvm can't read gets no entry, and ssh refuses a host key that doesn't match. devvm's port forwarder, the sessions of links between VMs and a `--clone` VM's git all check the VM against the same record.
- **Only host keys go into the record.** devvm keeps a line the VM shows only when its key decodes and names its own type, which works for every key type ssh has, and it drops certificates, markers such as `@cert-authority`, patterns and anything else. So the VM can't add a line that would make ssh trust another host, or trust a key for another name.
- **Each connection is its own.** The entries set `ControlMaster no`, so a session never rides on a connection that another program opened. Of Lima's own ssh settings for the VM, devvm copies only the address, port, user and login key, because Lima's settings turn host key checks off.
- **The entries turn off everything else your ssh could share with the VM.** They set `ForwardX11 no`, `ForwardX11Trusted no`, `Tunnel no`, `PermitRemoteOpen none`, `AddKeysToAgent no`, `ProxyJump none`, `ProxyCommand none`, `UpdateHostKeys no` and `HashKnownHosts no`, whatever your own `Host *` says. That turns off X11, tunnels, remote SOCKS forwards and adding keys to an agent. `PermitRemoteOpen` needs OpenSSH 8.7 or newer (`ssh -V`), and an older ssh refuses the entries.
- **Your agent is forwarded only as the VM's [SSH agent](../using/ssh.md#agent-forwarding) choice says.** devvm's own connections to a VM follow the same choice, the one the VM's current start was made with, and each carries that VM's agent alone. devvm finds the VM's agent socket, which is yours, devvm's own or none, and gives it to the backend's start of that VM, to its shell and to the commands that set it up, such as the setup devvm sends and the `devcontainer.json` commands. The backend's other commands get no agent, and neither do the reads of a VM's host keys, the [self-check](network.md#the-self-check)'s probe, the checks of a `--clone` VM's clone or `devvm paste`'s writes and counts. devvm never changes its own environment, so when one run reaches several VMs, such as a group's links, no VM gets another VM's agent. devvm's background processes, such as the proxy and the port forwarder, start without an agent, and its own ssh to a VM uses the VM's login key alone. `devvm limactl` runs without your agent too, because Lima's ssh shares one connection per VM, kept open after the command ends, and an agent forwarded over it is the agent of whichever command opened it. With your agent, a `devvm limactl shell` could open that connection and pass your agent to devvm's later sessions of a VM whose choice is devvm's agent or none.

A session carries the VM's connections through your machine only where your ssh asks for it. The entries can't turn off a `RemoteForward` of yours without also refusing an `ssh -R` you type, so a `RemoteForward` still applies. The VM's sshd allows local forwarding (`-L` and `-D`, which VS Code uses), and it allows remote forwards only on addresses in `127.77.0.0/16`, which devvm keeps for links between VMs, so a remote forward on the VM's `localhost`, as `ssh -R` asks by default, is refused. It allows no tunnels or X11. Root in the VM can change all of that, so it's your ssh that decides, and [remote forwards you add](../safety/attack-vectors.md#the-network) says what's left open.

### Secrets stay on the host

devvm saves secrets in the macOS Keychain, or elsewhere in files only you can read, and it never puts a secret in a process's arguments ([where secrets are saved](network.md#where-secrets-are-saved)). A VM uses only the Claude Code token, when its image has `claude`, and the secrets you bind to it. On every tier, a VM's secrets and its agents' logins stay on your machine, and its proxy puts them only into requests for their own hosts.

The secrets work only for that VM. Its proxy needs the VM's own password, and the VM's placeholders end in a random value of the VM's own, which isn't the password, so a placeholder that the VM sends elsewhere gives nothing away. Another VM, or a program on the host that reaches the proxy's port, gets no secret. The password is in files only you can read, and never in a process's arguments or in devvm's run logs, and only you can read the proxy's logs.

Each VM's proxy reads traffic to the hosts it puts secrets in for with the VM's own CA, which devvm keeps in the VM's run folder, `~/.local/share/devvm/vms/<hash>/run/ca/` ([the VM's CA](network.md#the-vms-ca)). Your machine never trusts it, but inside its VM it vouches for any site, and no other VM trusts it. Only you can read the keys, they never leave this machine, and devvm keeps them out of Time Machine. Each CA lasts 30 days, and the next one, which the VM already trusts, takes over a week before the current one ends. The certificates that the proxy makes never outlast their CA. What a copy of a key could do, and for how long, is under [the proxy's CA keys](../safety/attack-vectors.md#this-machine).

### Set-id files on Linux hosts

Root in a VM can set the set-user-id and set-group-id bits on files in the project folder. On macOS these bits don't reach the host, because virtiofs drops them, but on Linux they do. A file that a Lima VM writes is yours, so a set-user-id file runs as you for anyone else on the machine who runs it. A set-user-id file that your machine's root owns runs as root for anyone who runs it, you included, and since the VM's writes are yours, such a file came from something else on this machine.

devvm names such files when it makes a VM and once a VM is stopped, by `devvm stop` or found stopped at a launch, but not at a launch into a running VM, because the search reads every file's metadata and takes about a second for 200,000 files. It skips `.git` folders, which hold most of a big project's files and whose files nothing runs by their path. It lists root's files apart with `setid_root_files`, and you shouldn't run them but delete them with `sudo rm`. It lists the rest with `setid_files`, and `chmod u-s,g-s FILE` clears their bits.

## What it doesn't protect

Each defense on this page leaves something open, such as what allowed hosts and secrets permit, open VMs, files the VM writes into the project folder, links between VMs, and escapes from the VM itself through bugs in Lima, Apple's Virtualization framework or QEMU. [Attack vectors](../safety/attack-vectors.md) keeps the one list of them, under [Narrowed, not closed](../safety/attack-vectors.md#narrowed-not-closed) and [Left to you](../safety/attack-vectors.md#left-to-you), and each entry there links back to its mechanism.

Found a way out of a VM? See [SECURITY.md](https://github.com/devvmhq/devvm/blob/main/SECURITY.md).
