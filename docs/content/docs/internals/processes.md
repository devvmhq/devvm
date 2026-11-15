---
title: Processes
explainer: What each of devvm's processes holds, reads, writes and is confined by
weight: 10
---

This page goes through every process that devvm runs on this machine for a VM, one section each: what it holds, such as secrets and keys, the input a VM controls that it reads, what it writes and how far that can grow, how it's confined, and who starts it and waits for it. Then it lists every listener on this machine whose bytes a VM controls, and why devvm is written in Go. The pages each section links to have the details, and the [security model](security.md) has what these processes protect.

Every process here runs as you. devvm runs its own long-running processes as itself, `devvm` with a hidden command, so their code is the binary you installed, and the other long-running programs it runs are OpenSSH's `ssh` and `ssh-agent`, Lima and, on Linux, QEMU.

| Process | Started by | Ends | Confinement |
| --- | --- | --- | --- |
| [devvm itself](#devvm-itself) | you | when its command ends | none |
| [A VM's proxy](#a-vms-proxy), `devvm _proxy` | each start of the VM | with the VM's hostagent, or `devvm stop` | none |
| [A build's proxy](#a-builds-proxy), `devvm _proxy` | a build behind a company proxy | with the build | none |
| [The port forwarder](#the-port-forwarder), `devvm _ports` | each start of a project VM, and each change of its ports | with the VM's hostagent, or `devvm stop` | none |
| [The link session](#the-link-session), `devvm _links` | a start or a change of links, for a client with links | when the client stops or has no links, or `devvm stop` | none |
| [ssh masters and relays](#ssh-masters-and-relays) | the forwarder, the link session and devvm | with the process that started them | none |
| [The QEMU wrapper](#the-qemu-wrapper-and-its-keeper), `devvm _qemu` | Lima, on Linux hosts | with QEMU | QEMU's restricted network, and a user namespace for a VM with `.git` pins |
| [The daily tools check](#the-daily-tools-check), `devvm _refresh-tools` | a launch of an existing VM, once a day | when the check is done | none |
| [The desktop relay](#the-desktop-relay) | `devvm desktop` | with `devvm desktop` | none |
| [The SSH agents](#the-ssh-agents) | you, or `devvm` for devvm's own | devvm's own: at logout or a reboot | none |
| [Lima's hostagent](#limas-hostagent) | `limactl start` | when the VM stops | a Seatbelt profile on macOS, for a confined VM or a build |

## How devvm starts them

devvm starts the proxy, the port forwarder, the link session and the daily tools check detached, in a session of their own, so they outlive the run that started them and the terminal it ran in (`daemon.Detach`). Each gets no input and devvm's environment without `SSH_AUTH_SOCK`, so none of them uses your SSH agent of its own accord, and its output goes to its own log or nowhere. The one exception is a VM's proxy, which relays the agent that the VM's start forwards, named in its config ([the SSH agents](#the-ssh-agents)). devvm marks every file it was given past standard input, output and error close-on-exec as it starts, so a terminal or a pipe that devvm inherited never stays open in them ([things learned](design.md#things-learned)).

devvm finds such a process again by its pid file and its arguments, and it signals a process only while both still say it's that process, so a pid that the system gave another program since is never signalled (`daemon.Proc`). A stop sends `SIGTERM`, waits two seconds, and sends `SIGKILL` only to a process whose arguments are still the ones devvm started. Each signal goes to the process's group, which its own session makes it the leader of, so what it started ends with it, such as the forwarder's and the link session's ssh masters and relays, even when it's killed and can't stop them itself (`daemon.SignalGroup`). A forwarder or a link session that gets `SIGTERM` from anything else, such as `kill`, stops its ssh before it exits, waiting up to two seconds for that, as devvm does for its cleanups. A process that devvm waits for, such as the proxy, says it's ready in a status file of its own, which holds its pid and its address, so a file that another process left never counts ([how long a proxy runs](network.md#how-long-a-proxy-runs)).

The run that starts a process reaps it if it ends first, and once that run ends, nothing on this machine waits for it. Each one ends on its own instead: the proxy and the forwarder follow the VM's hostagent through its pid file, `ha.pid`, which is the VM's lifeline, the link session asks the VM's backend every 5 seconds whether the VM still runs the start it was made for, and `devvm stop`, `devvm rm` and `devvm gc --orphans` stop a VM's proxy, forwarder and link session while they still run.

## devvm itself

- **Holds:** everything you keep with devvm. It reads secrets from the macOS Keychain with the `security` command or from files only you can read, and it writes each VM's proxy password and the company proxy's credentials into files only you can read, never into a process's arguments ([where secrets are saved](network.md#where-secrets-are-saved)).
- **Reads from a VM:** the output of the commands it runs there, such as a VM's host keys, its settings to save and a history export, and the files a VM can write or swap, such as the project folder's `devcontainer.json`. It reads such a file never following a link out of the folder, and with a cap on its size, and each kind of output has a cap of its own, such as 128 MiB for a VM's settings and 2 GiB for a history export ([coding agents](agents.md#saving-a-vms-settings)).
- **Writes:** its run log, `~/.local/share/devvm/logs/runs/<date>-<time>-<pid>.log`, with at most 4 MiB of output for each command it runs in a VM, and it keeps the newest 20 logs, or as many as `logs.runs` sets. It writes state files whole, renamed into place ([state files](overview.md#state-files)).
- **Listens:** only in `devvm desktop` ([the desktop relay](#the-desktop-relay)) and while `devvm agent login` waits for a browser's login ([listeners](#listeners-a-vm-reaches)).
- **Confinement:** none. devvm needs your Keychain and your files, and it's the program you chose to run.

## A VM's proxy

One proxy runs for each running VM, as `devvm _proxy -config FILE`, with the VM's `proxy.json` in its run folder, `~/.local/share/devvm/vms/<hash>/run/` ([the proxy](network.md#the-proxy)).

- **Holds:** the VM's password, the VM's CA and the next one, with their keys, and the company proxy's credentials. It looks up each bound secret's value itself, through the Keychain or the secret's file, and keeps a value for 5 minutes and an agent's login for 30 seconds. It reads the agents' login files in devvm's store and refreshes their OAuth logins there, and it saves a login that the VM makes into the store and rewrites the VM's bindings ([logins made in a VM](network.md#logins-made-in-a-vm)).
- **Reads from the VM:** every request the VM sends. It takes at most 64 KiB of a request's headers, in an intercepted or transparent connection too, within 30 seconds, requires the VM's password before anything else, and reads a ClientHello of at most 16 KiB and 256 bytes within 10 seconds of its first byte. It also reads files the VM can't write but that change while it runs: your policy files and the VM's bindings, each at most 1 MiB, and the links' list, `links.json`, at most 1 MiB, which `devvm _links` writes. For a VM that forwards an SSH agent, it also reads the agent's requests that the VM sends, each at most 256 KiB ([the SSH agents](#the-ssh-agents)).
- **Writes:** its event log, `network.jsonl`, and its own log, `proxy.log`, in the VM's logs folder, `~/.local/share/devvm/vms/<hash>/logs/`, each rotated before it passes 10 MiB, with one `.1` file kept ([log files](network.md#log-files)). For a VM with the nix layer, it fills the [shared Nix cache](network.md#the-shared-nix-cache), at most 10 GiB unless its config sets another cap. It also writes the login files and bindings above.
- **Bounds:** at most 1024 `CONNECT` tunnels at once, at most 2048 connections of every kind with the VM's password, plain HTTP and idle ones included, at most 128 more that haven't sent the password yet, and at most 2048 requests to hosts under way, its tunnels' included. It keeps at most 1024 of the certificates it made for intercepted hosts for each CA, dropping the one used longest ago. A connection to the proxy, and the HTTP inside an intercepted or transparent connection, waits at most 10 minutes between requests, and HTTP/2 inside one carries at most 100 requests at once. A request without the VM's password gets its `407` and its connection closed. A plain HTTP request whose client has stopped sending goes on while its answer keeps coming, and ends after a minute with nothing to pass on, so a client that went away frees what its request holds ([the proxy](network.md#the-proxy)). Nothing bounds how long a request or a tunnel lasts, so long downloads, WebSockets and slow streams go on.
- **Listens:** on `127.0.0.1`, on the VM's proxy port, and, for a VM that forwards an SSH agent, on the VM's agent socket, `~/.local/share/devvm/run/agent-<hash>.sock` ([listeners](#listeners-a-vm-reaches)).
- **Confinement:** none. It runs as you, with your Keychain and your files.
- **Starts and ends:** devvm starts it before each start of the VM, on every tier, and waits up to 5 seconds for it to say it's listening. It exits when the hostagent it follows ends, or 15 minutes after it starts if the hostagent never appears, and `devvm stop` stops it ([how long a proxy runs](network.md#how-long-a-proxy-runs)). When its hostagent ends, it stops listening and closes every connection it accepted, the tunnels it carries and the WebSocket upgrades it passes on included, and refuses one that arrives after that. It then waits for the handlers to return, 5 seconds at most, because Go's HTTP server neither closes a connection that a handler took from it nor waits for a handler. `devvm stop`'s `SIGTERM` ends it at once.

## A build's proxy

A build gets a proxy of its own only behind a company proxy, because a build VM otherwise reaches the internet directly ([a company proxy](design.md#a-company-proxy)). It's the same `devvm _proxy`, with a config in the build VM's `run/build-proxy` folder, which devvm makes again for each build.

- **Holds:** a password made for the build, and the company proxy's credentials. It has no CA, no secrets, no links and no Nix cache.
- **Reads from the build:** its requests, with the same checks as a VM's proxy, under a policy that allows any host on ports 80 and 443, but not this machine's own or internal addresses.
- **Writes:** `events.jsonl`, rotated as a VM's event log is, and `proxy.log`, in the build proxy's folder. devvm deletes the folder after a build that succeeds and keeps it after one that fails.
- **Confinement:** none.
- **Starts and ends:** the build starts it, keeps its process and stops it when the build ends, and a cleanup stops it when the build fails or is interrupted. It also follows the build VM's hostagent, as a VM's proxy does.

## The port forwarder

One forwarder runs for each running project VM that forwards ports, as `devvm _ports` ([its process and files](ports.md#its-process-and-files)).

- **Holds:** the key that the backend logs in to the VM with, which on Lima is Lima's `_config/user`, and the VM's host keys for its current start. Its ssh uses no agent.
- **Reads from the VM:** the VM's report of the ports it listens on, which comes over one ssh session about every 10 seconds, as the VM paces it. A line of the report has at most 127 bytes and its newline, and a longer line, or a report of more than 4096 lines, ends the session, which the forwarder opens again, so nothing is cut. The forwarder applies at most one report every 500 ms. It also reads the bytes of each forwarded connection, which it carries as they are, and the other VMs' records of their ports.
- **Writes:** `ports.forwarded` in the VM's run folder, and its log, `ports.log`, in the VM's logs folder. The VM chooses some of what goes in the log, so the forwarder makes at most 200 writes at once and then one every 10 seconds, each cut at 512 bytes and without control characters other than newlines and tabs, and devvm empties the log at the forwarder's next start once it's over 1 MiB.
- **Bounds:** at most 256 forwards, and at most 256 connections at once. When accepting a connection fails while the port still listens, such as when the forwarder runs out of open files, it tries again after a wait that starts at 5 ms and doubles with each failure in a row, up to a second, and logs only the first failure of a run, as the link session and a login's forward do.
- **Listens:** on `127.0.0.1` and `::1` for each forwarded port ([listeners](#listeners-a-vm-reaches)).
- **Confinement:** none. Its ssh processes get the higher limit of open files that devvm runs with.
- **Starts and ends:** devvm starts it once the backend says the VM runs, and again when the VM's ports change. It exits once the VM's hostagent is gone, which it checks every 2 seconds, and `devvm stop`, `devvm rm` and `devvm gc --orphans` stop it.

## The link session

One link session runs for each running client VM with [links](ports.md#links-between-vms), as `devvm _links VM` ([the link session](ports.md#the-link-session)).

- **Holds:** the login key and the pinned host keys of the client and of each target it connects to.
- **Reads from the VMs:** the bytes of each link's connection, both ways, and the messages of the client's sshd, such as its refusal of a forward, each cut at 512 bytes.
- **Writes:** `links.json` in the client's run folder, the client's event log, `network.jsonl`, with the proxy's own writer, so it rotates by the same rule, and its log, `links.log`, in the client's logs folder, which it writes as the forwarder writes its own and which devvm empties at the next session's start once it's over 1 MiB.
- **Bounds:** at most 256 of the client's connections at once.
- **Listens:** on a unix socket for each link, mode 600, in `~/.local/share/devvm/run` (mode 700).
- **Confinement:** none.
- **Starts and ends:** a start of the client and a change of its links start it. It ends once the client no longer runs the start it was made for, which it checks every 5 seconds, or has no links left, and `devvm stop` stops it with the client.

## ssh masters and relays

The forwarder and the link session each reach a VM over one ssh connection of their own, its master. The forwarder's master runs the VM's report of its ports, and the link session keeps one master to the client, which asks for the links' remote forwards, and one to each target. Every master checks the VM's pinned host key, sets `ControlPersist=no`, and sends a keepalive every 10 seconds, giving up after 3 that get no answer.

Each connection that the forwarder or the link session carries gets an ssh process of its own, a relay, which runs `ssh -W`. A link's relay can only ride on its target's master. A forwarder's relay rides on the VM's master when it can, and it checks the same pinned host key either way, so it reaches only that VM.

- **Holds:** the login key, and no agent (`IdentityAgent=none`).
- **Reads from the VM:** the bytes of the connection it carries.
- **Writes:** nothing of its own. A master's messages go to its process's log.
- **Ends:** a relay ends when either side ends, when its forward or its link goes, and 30 seconds after the program on this machine closes its side if the VM sends nothing in that time ([how it reaches the VM](ports.md#how-it-reaches-the-vm)). Each process that starts a master or a relay waits for it, and kills it when it ends that connection.

`devvm desktop` and `devvm agent login` start relays of their own, one ssh process for each connection, which rides on no master (`ControlMaster=no`, `ControlPath=none`). A login's relay ends as the forwarder's do, and a login carries at most 16 connections at once, closing any past that, so whatever else on this machine connects to its port can't start ssh processes without end. A viewer's ssh process gets 2 seconds to end once its viewer leaves, and is then killed.

## The QEMU wrapper and its keeper

On Linux hosts, Lima starts each VM's QEMU through a wrapper that devvm writes into `~/.local/share/devvm/run/qemu/`, which runs `devvm _qemu exec` ([confinement](lima.md#confinement)).

- **`devvm _qemu exec`** reads Lima's arguments for QEMU and adds `restrict=on` to Lima's one network, except for an open VM and a build with the internet, and for a VM confined to its proxy it adds a `guestfwd` to that proxy. It refuses arguments of any other shape (`qemu_refused`). It then runs QEMU in its place, or, for a VM with `.git` pins, which a VM whose [.git protection](git-protection.md#on-linux-hosts) is on has, starts the keeper and waits for it. Only such a VM gets a user namespace.
- **The keeper, `devvm _qemu pinned`,** runs in a user namespace and a mount namespace of its own, as you, with the capability to mount in that namespace alone. It makes `.git`'s hooks and config read-only there, and it pins a path again after any change in a folder it watches, and every 2 seconds. It records each pin it makes again in `git-pins.jsonl` in the VM's run folder, and on QEMU's error output, which Lima logs. It starts QEMU through `devvm _qemu drop`, which clears its ambient, permitted, effective and inheritable capabilities, keeping the bounding set, sets `no_new_privs`, and runs QEMU.
- **`devvm _qemu probe DIR`** checks, in a namespace of its own, that a file in DIR can be pinned.
- **`devvm _qemu connect`** is what QEMU's `guestfwd` runs for each connection that the VM makes to `192.168.5.4:3128`. It connects to the VM's proxy and carries the bytes both ways, so it reads only what the VM sends to its proxy, which the proxy checks. Each one is a process of its own, about 7.7 MB of memory, so it carries at most 256 of the VM's connections at once, about 2 GB for a VM that holds them all, each holding a slot, which is a lock file in the `proxy.slots` folder of the VM's run folder. A connection past that gets a `503` and is closed before it reaches the proxy, and QEMU's error output, which Lima logs, says so at most once a minute (`qemu_connects_full`). A VM that was running when devvm was upgraded has no slots, and so no limit, until its next start. It has no idle timeout and no limit on the bytes it carries, because the proxy bounds what each connection carries, and a long download or a quiet stream must go on.

The keeper dies with `devvm _qemu exec`, and QEMU with the keeper. `devvm _qemu connect` ends when its connection ends, and its slot is free again.

## The daily tools check

`devvm _refresh-tools` asks for the shared tools' newest versions once a day, so a later launch can offer them ([shared tools](../using/toolchains.md)).

- **Holds:** the company proxy's password, when `config.toml` sets the proxy, in the environment of the mise it runs.
- **Reads:** nothing from a VM. It downloads devvm's pinned mise, checked by its SHA-256, mise reads the tools' versions from the internet, and the check fetches .NET's release data.
- **Writes:** the shared tools' lock, under its lock, the time of the check, and its output to `~/.local/share/devvm/logs/tools-check.log`, which each check starts empty.
- **Starts and ends:** a launch of an existing VM starts it when the shared tools have a lock file and a day has passed since the last check, and doesn't wait for it. `DEVVM_NO_TOOL_CHECK` turns it off.

## The desktop relay

`devvm desktop` runs the relay in its own process, for as long as it runs ([on this machine](desktop.md#on-this-machine)).

- **Holds:** the VM's desktop password, which never goes into the VM.
- **Reads from the VM:** the VM's side of VNC's handshake, which is a version, at most 255 security types and a result, within 15 seconds, and then the VM's screen, which it passes to the viewer as it is.
- **Writes:** lines to the run log when each viewer connects and disconnects, and for each connection it refuses, and each ssh process's messages.
- **Bounds:** at most 8 connections at once, counting those still in their handshake, one check of a password at a time, and a second's wait after a wrong one.
- **Listens:** on `127.0.0.1` alone, on the VM's desktop port ([listeners](#listeners-a-vm-reaches)).
- **Starts and ends:** it ends when you press Ctrl+C, or when the VM no longer runs, which it checks every 5 seconds, and it waits for each viewer's connection.

## The SSH agents

A VM whose [SSH agent](../using/ssh.md#agent-forwarding) choice is `on` gets your own agent, which devvm doesn't start, stop or confine. A VM whose choice is `devvm` gets devvm's own agent, which is OpenSSH's `ssh-agent`, run as `ssh-agent -a ~/.local/share/devvm/agent.sock`.

- **Holds:** only the keys you add with `devvm ssh-agent add`, each for 12 hours unless you chose another lifetime. devvm refuses a key in a folder that a VM mounts.
- **Reads from the VM:** only the requests that the VM's proxy passes on. A VM can use the keys while it's connected, and it can't copy them.
- **Starts and ends:** devvm starts it when a VM that uses it starts, or when you add a key, and doesn't stop it, so it runs until you log out or restart this machine. devvm keeps no pid file for it.

A VM that devvm starts never reaches either agent's socket itself, and the exceptions are below. `ssh-agent` answers every request that reaches it by its own rules, and a forwarded connection may send any of them, so a VM with the agent's own socket could remove every key with `ssh-add -D`, lock the agent with a password of its own with `ssh-add -x`, or add keys to it without bound. Every VM that uses the agent would lose it then, and with your own agent, so would you. Instead, the VM's proxy listens on a socket of the VM's own, `~/.local/share/devvm/run/agent-<hash>.sock`, mode 600, and the VM's start forwards that socket, as devvm's own connections to the VM and its [ssh entry](../using/ssh.md#ssh-access-by-name) do, so an editor forwards it too. The proxy passes on only three requests to the agent: listing its keys, signing with one, and `session-bind@openssh.com`, which ssh sends as it forwards an agent and as it logs in with one, so a key you added with destination constraints (`ssh-add -h`) keeps them. It answers any other request with `SSH_AGENT_FAILURE` itself, and logs the first one on each connection in its own log, so removing a key, locking the agent, adding a key and loading a smartcard's keys never reach the agent. It reads each message whole, at most 256 KiB, as `ssh-agent` does, and ends a connection that sends a longer one. Each VM's proxy carries at most 16 connections at once, each with a connection of its own to the agent, and it closes a connection that sends no whole request for 2 minutes. The cap is each VM's own, not the agent's, so VMs that share an agent can hold 16 connections each, and enough of them could still use up the agent's open files, 256 by default on macOS. A request that the agent asks you about, for a key added with `--confirm`, waits as long as the agent does, and `ssh-agent` answers one request at a time, so while it waits for your answer, every other VM that shares the agent, and you, wait too.

devvm records in the VM's state the start it made, and that a start whose choice forwards an agent forwarded the relay, even a start that devvm ran without `SSH_AUTH_SOCK`, which has no agent to relay. For such a start, devvm's own connections and the ssh entry name the relay even while the proxy starts again or is gone, or relays nothing, so the VM then has no agent, never the agent's own socket, even when a later command runs from a shell that has one. A VM running a start that devvm didn't record, such as one started with `devvm limactl start`, gets no agent in its ssh entry, and devvm's own connections get its proxy's relay of the agent that devvm last started it with, or none, never the agent's own socket. Only a VM that an earlier devvm started without the relay keeps the agent's own socket, in devvm's connections and its ssh entry, until its next start.

The proxy relays the agent that the VM's start chose, and it keeps relaying that agent until the VM's next start, even when it starts again for a running VM, so a change of choice waits for the next start, as it does for the VM's backend. The proxy deletes the socket when it ends with the VM's hostagent, devvm deletes it when it stops the proxy, and `devvm gc --orphans` deletes one that a killed proxy left, which nothing listens on.

## Lima's hostagent

Each running VM has a hostagent, `limactl hostagent`, which `limactl start` runs and which runs the VM: on macOS it runs Apple's Virtualization framework in its own process, and on Linux it starts QEMU.

- **Holds:** Lima's login key, `_config/user` in devvm's Lima home, which every VM there authorizes, and the VM's disk.
- **Reads from the VM:** everything the VM's network sends, on macOS. On Linux, QEMU carries it.
- **Writes:** its logs, `ha.stdout.log` and `ha.stderr.log`, in the VM's Lima folder, which devvm doesn't rotate. devvm replaces each proxy password in `ha.stderr.log`, and in the serial consoles' logs, `serial*.log`, with as many `*` once the start ends, in the first 64 MiB of each log.
- **Listens:** on the VM's ssh port on this machine's loopback, on macOS, where on Linux QEMU listens. It also listens on unix sockets in the VM's Lima folder and, for an open VM or a build, on loopback ports for Lima's DNS resolver ([listeners](#listeners-a-vm-reaches)).
- **Confinement:** on macOS, a confined VM's hostagent and a build's run under a Seatbelt profile, `sandbox.sb` in the VM's run folder, which denies `com.apple.dnssd.service`. A confined VM's profile lets it connect only to the VM's ssh port and its proxy on the host's loopback, to the unix sockets in the VM's Lima folder and, when the VM forwards an agent, to that SSH agent, and it lets the hostagent listen only on the VM's ssh port and those unix sockets. A build's profile reaches either the internet but not the host's loopback, apart from its ssh port and its build proxy's port, or nothing, and it doesn't limit what the hostagent listens on. Every profile limits only the network, so the hostagent can still read and write your files. An open VM's hostagent and every hostagent on Linux run unconfined, and on Linux, QEMU's restricted network confines a confined VM instead ([confinement](lima.md#confinement)).
- **Starts and ends:** devvm records the hostagent it started in `started` in the VM's run folder and checks that the VM still runs on it ([the self-check](network.md#the-self-check)), and the proxy and the forwarder follow its `ha.pid`. Nothing waits for it, and `limactl stop`, which `devvm stop` runs, ends it.

## Listeners a VM reaches

These are the listeners on this machine that carry bytes a VM controls. Every one listens on this machine's loopback or on a unix socket, so other machines never reach them, but every program and every user on this machine can connect to a TCP port on its loopback.

| Listener | Process | Gate | What the VM controls |
| --- | --- | --- | --- |
| each forwarded port, on `127.0.0.1` and `::1` | the port forwarder | none: whatever connects reaches the VM's server, 256 connections at once | everything its server sends back |
| the VM's ssh port, on the loopback, one of the 17 ports every browser refuses ([why](lima.md#what-devvm-sets-on-a-vm)) | the hostagent on macOS, QEMU on Linux | the VM's sshd, which each of devvm's connections checks against the VM's pinned host key, and browsers' own refusal of the port | everything the VM's sshd sends, which root in the VM can make speak HTTP |
| Lima's DNS resolver, on loopback ports the system picks | the hostagent, for an open VM and every build, never for a VM confined to its proxy | none | its lookups |
| the unix sockets in the VM's Lima folder: the network socket on macOS, which carries the VM's network frames, and Lima's ssh control socket, `ssh.sock` | the hostagent, and Lima's ssh | the folder's mode | its network frames, and everything its sshd sends |
| the VM's proxy port, on `127.0.0.1` | the VM's proxy | the VM's password, before anything else | its requests |
| a build's proxy port, on `127.0.0.1` | the build's proxy | the build's password | its requests |
| the desktop port, on `127.0.0.1` | `devvm desktop` | the VM's desktop password, checked on this machine | its screen, once a viewer is in |
| `127.0.0.1:1455`, with `::1:1455` and port 1457 on both when they're free | `devvm agent login`, while Codex's or opencode's browser login to ChatGPT waits, but not with `--device-code` | none: whatever connects reaches the agent's login server in the VM, 16 connections at once | the login server's answers |
| each link's unix socket | the link session | the socket's mode, 600 in a folder of mode 700 | the client's bytes, through its sshd's remote forward or its proxy, and the target's answers |
| the ssh masters' control sockets, `run/port-<hash>.ctl` and `run/link-<hash>.ctl` in `~/.local/share/devvm` | the forwarder's and the link session's ssh masters | the folder's mode: any process of yours that connects gets a channel into the VM | everything the VM sends on that channel |
| the VM's agent socket, `run/agent-<hash>.sock` in `~/.local/share/devvm` | the VM's proxy, for a VM that forwards an SSH agent | the socket's mode, 600 in a folder of mode 700 | the agent's requests, of which the proxy passes on only listing keys, signing and binding a session to a host |

A forwarded port and the login's port carry whatever connects to them into the VM, a browser's pages included, and that's what [attack vectors](../safety/attack-vectors.md#network) lists as left open.

## Why Go

devvm stays in Go, and its own history shows why. None of its first 712 commits fixes a memory-safety bug, devvm's own code never imports `unsafe` and it builds without cgo, and none fixes a data race that got past Go's race detector, which every test run uses. Its security and reliability fixes fall mostly into five classes, which a language's types don't prevent:

- **Lifecycles and resources,** such as a relay that outlived its link, a VM's QEMU left running without its hostagent, and a VM that could make devvm read more than it should.
- **Policy semantics,** such as a 6to4 address judged by its IPv4 address, and a placeholder swapped only when it's the whole value.
- **Secret leaks,** such as a proxy password in Lima's log, and a resolver's error in a VM's refusal reason.
- **Races between processes,** such as a proxy that had just started looking gone, and two runs choosing ports or making a folder's VM at once.
- **File-system trust,** such as a project folder replaced after devvm found it, a folder that a VM names as a git folder, a link under a folder's path, and a project folder mounted by its path rather than as the folder itself.

A port to Rust would cost what keeps devvm easy to keep:

- **One static binary,** built with `CGO_ENABLED=0` for every platform from one machine. Go's `crypto/tls` needs no C toolchain, while rustls's usual providers, aws-lc-rs and ring, have C in them.
- **A small dependency tree:** Go's standard library, Go's own `x` modules, Cobra and pflag, a TOML parser and go-i18n, with the few modules that cobra/doc brings in, where a Rust proxy on the usual async, HTTP and TLS crates brings in about a hundred.
- **One owner for each concern,** which depguard, forbidigo and the [structure test](../contributing/structure.md#the-structure-test) enforce, and which none of them could see across a language boundary.

The proxy is the only part that could be split off, and it's bound to the rest: it looks up secrets and refreshes agents' logins with devvm's own code, its config is a schema that every release's state keeps, and it reads its built-in policies from devvm's binary.

The answer changes only for a requirement Go can't meet, such as intercepting QUIC. Even then, only that piece would move, as a process of its own.
