---
title: Port forwarding
explainer: How devvm forwards a VM's ports to this machine
weight: 4
---

This page explains how devvm's forwarder forwards a VM's ports to this machine, which ports it never takes, and how it runs. [Ports and size](../using/ports-size.md#ports) covers choosing a VM's ports with `--ports` and `devvm ports`.

devvm forwards a VM's ports with its own forwarder, never with Lima's own forwards. [Lima](lima.md#ports) explains why, and what devvm sets on Lima instead.

## What it forwards

[Ports](../using/ports-size.md#ports) lists which ports the forwarder forwards, for `--ports all`, the default, and for a list such as `--ports 5173,8000-8010`, and how soon. A listed port also carries a server on the VM's loopback (`127.0.0.1` or `::1`) to this machine. You can list port 22, which forwards the VM's sshd to port 22 on this machine, while ssh by name keeps using the VM's own ssh port.

The forwarder never forwards a server on an address in `127.77.0.0/16`, on any VM, because that's where a VM listens for its [links to other VMs](#links-between-vms), so a link never appears on this machine's `localhost`. A server of the VM's own on `127.0.0.1` with the same port is forwarded as usual. It also never forwards a server on `127.0.77.1`, where the desktop's [local proxy](desktop.md#the-local-proxy) listens, because the local proxy adds the VM's password to what it sends to the VM's proxy, and on this machine's `localhost` other users and programs could use it.

## What it never takes

The forwarder never takes a port that a program on this machine has on any address. That program keeps the port and its `localhost` connections, and the forward waits until the VM's server stops and starts again.

The forwarder also never takes a port that devvm records for a VM, even while that port is free. Those are the ssh, proxy and desktop ports that devvm chooses for VMs, running or not, and the VM's own ssh port. devvm chooses and records every VM's ssh port, whatever its tier, so no forwarder takes it in the seconds before Lima binds it ([the threat check](design.md#ports-on-lima)). When devvm chooses one of those ports, it also avoids every port that any VM lists, running or not, so a stopped VM's listed ports stay free for it.

When a start, or a change of a running VM's ports, finds that a program on this machine has a listed port, devvm doesn't forward that port and warns you (`ports_taken`). The VM starts or runs on all the same, and the next start or change that finds the port free forwards it again. [Ports](../using/ports-size.md#ports) says what happens to a program that binds a port on all addresses after the forwarder has forwarded it.

### Ports other VMs forward

A host port that two VMs could forward belongs to the running VM that forwards it, never to a stopped one, so a stopped VM's listed ports are free to the others until it starts again. Each forwarder writes the ports it forwards to `ports.forwarded` in its VM's run folder, `~/.local/share/devvm/vms/<hash>/run/`, after each change, with its pid on the first line and the ports it forwards on the second. The next three lines hold the ports it tried and couldn't take, by why: those a program on this machine has, those past its limit of 256 forwards, and those this machine refused for another reason, such as a port below 1024 on Linux. It writes an empty record when it starts. A record counts only while that pid is the VM's running forwarder, as `ports.pid` and the process's arguments say, so the record of a VM that stopped or crashed, whose forwarder ended with it, holds nothing. devvm also removes the record under the VM's `ports.lock` each time it starts the VM's forwarder, so a pid used again never makes an old record count.

At each report from its VM, every forwarder reads the other VMs' states and records again, and it never takes these ports:

- a port that another VM's running forwarder forwards now
- a port that another VM lists while that VM has a claim on it, unless this VM lists it too. A VM has a claim while its forwarder runs, and while devvm holds its `ports.lock` to start the forwarder again, so no other VM takes its listed ports between the old forwarder's stop and the new one's start.

A VM that lists a port comes before one that forwards every port. Besides its VM's reports, every forwarder checks the ports it forwards against those rules every 10 seconds on its own clock and closes a forward whose port is now another VM's, so a VM that keeps its session open without reporting can't keep a port. Closing a forward also closes the connections it accepted that are still open, such as a browser's keep-alive connections, so none of them reaches the old VM afterwards. A VM that forwards every port therefore gives up a port within about 10 seconds of a running VM listing it.

A port the forwarder never takes for those reasons, and that its VM listens on, waits: the forwarder tries it again at each report, which comes at least every 10 seconds, and forwards it once it's free, even when an earlier try found it taken. A port the other VM gives up can therefore take up to about 20 seconds to reach this VM: up to 10 for the other forwarder's check and up to 10 for this one's next report. When a start or a change of ports finds that another VM forwards one of the VM's listed ports now, whether that VM lists it or forwards every port, devvm keeps the port in the VM's list and warns you (`ports_held`), and `devvm ports` warns again, with the other VM in its JSON's `held_by`. A port that another VM forwards isn't a program's, so it never gets `ports_taken`. A listed port of a running VM that its forwarder tried and couldn't take is only logged by the forwarder, and `devvm ports` warns about it, as [When it can't forward](#when-it-cant-forward) describes.

When two VMs that list the same port start at once, neither waits for the other, because neither forwards it yet. The first forwarder to bind the port on this machine's loopback has it, because the system lets one listener have it, and the other forwarder's try fails. That forwarder sees the winner's record at its next report and waits for the port from then on. No lock is needed for this, and no port is shared.

## How it reaches the VM

The forwarder reaches the VM over the VM's ssh, with a key and a host key that devvm pins, and it asks for no forwarding into this machine. Only a [link session](#the-link-session) asks a VM for a remote forward, on a connection of its own. Every connection starts on this machine, and the VM opens none to it. The VM sees each forwarded connection only as one to its own loopback, and on a confined tier the VM still reaches only its proxy.

A connection to a forwarded port, such as 5173, takes this path:

```mermaid
flowchart TD
  browser["your browser, on this machine,<br>where every connection starts"] -- "localhost:5173" --> fwd["devvm's forwarder,<br>on this machine"]
  fwd -- "ssh, with a key and<br>a host key devvm pins" --> sshd["the VM's sshd"]
  sshd -- "127.0.0.1:5173,<br>its own loopback" --> server["your server, in the VM"]
```

The forwarder is devvm's own, the same on every backend. The backend only says how the forwarder reaches a running VM's sshd, which is a user, a key and a port on this machine's loopback, and which file the forwarder follows to know the VM still runs, as Lima's [Ports](lima.md#ports) section describes.

Each forwarded connection gets an ssh process of its own, which carries it over the forwarder's ssh connection to the VM, and the forwarder carries at most 256 connections at once, refusing the next one. When a forward closes, because the VM's server stopped or its port is now another VM's, the forwarder closes that forward's open connections and ends their ssh processes too. A VM that never closes its end of a connection therefore can't keep that connection's ssh process, or one of the 256, after its port's forward has gone.

A connection's ssh process also ends once the program on this machine has closed its side and the VM has sent nothing for 30 seconds, whether or not the VM closes its end. A program that only stops sending, and waits for an answer, still gets one for as long as the VM keeps sending it, however slowly the program reads it, but the VM has to send something at least every 30 seconds, its first byte included. So a program that stops sending and then waits longer than that for the answer to begin, such as `nc -N` in front of a slow database query, has its connection cut. The same holds for every link's connections ([each connection](#each-connection)).

## Its process and files

The forwarder runs as a hidden devvm command, `devvm _ports`, one process for each VM. devvm starts it after each start of the VM, once the backend says the VM runs, and again with the new ports when you change a running VM's ports. `devvm stop`, `devvm rm` and `devvm gc --orphans` stop it, and a VM that stops some other way ends it, because the forwarder exits once the backend's process for the VM is gone. Which ports a VM forwards is in the VM's state, which devvm reads each time it starts the forwarder, so `devvm ports` changes the state and never the backend's config, and the backend never hears of them. The forwarder's log is `ports.log` in the VM's logs folder, `~/.local/share/devvm/vms/<hash>/logs/`, and its `ports.pid`, `ports.status`, `ports.lock` and `ports.forwarded` are in the VM's run folder, `~/.local/share/devvm/vms/<hash>/run/`, beside the VM's host keys for its current start, `known_hosts`, which the forwarder checks the VM against, as devvm's ssh config does ([ssh into VMs](security.md#ssh-into-vms)). Its control socket is `~/.local/share/devvm/run/port-<hash>.ctl`, named by a hash of the VM's name, outside the VM's folder, because a unix socket's path has fewer than 104 characters.

devvm reads every list of ports the same way, whether it comes from `--ports`, `devvm ports` or the VM's state. Each entry is a port from 1 to 65535 or a range of them, such as `8000-8010`. When the VM's state holds a list that devvm can't read this way, such as a file edited by hand, the VM doesn't start and devvm names the value it couldn't read with `port_invalid`, so a bad entry never becomes a port the VM didn't ask for.

## When it can't forward

When a forwarder doesn't start, devvm warns you and names its log (`ports_forward_failed`), and the VM runs on without forwarded ports.

When a running forwarder tries a listed port and can't take it, it logs the port with the system's error and records why in `ports.forwarded`. `devvm ports` warns about each such port by why:

- `ports_taken`, when a program on this machine has the port, which the system reports as the address being in use. The forwarder tries the port again only once the VM's server on it stops and starts again.
- `ports_limit`, when the VM already forwards 256 ports, the most a forwarder forwards at once, so the forwarder didn't try it. The forwarder tries the port again at the VM's next report once it forwards fewer.
- `ports_refused`, when this machine denied the forwarder the port, such as Linux keeping ports below 1024 for root. The forwarder tries the port again only once the VM's server on it stops and starts again.

Any other error, such as running out of open files for a moment, says nothing about the port, so the forwarder logs it alone, records nothing, and tries the port again at the VM's next report.

The forwarder listens on both `127.0.0.1` and `::1`, and it treats an error on `::1` as it would treat one on `127.0.0.1`, closing the port's `127.0.0.1` listener as well. A program on `::1` makes the port in use, and any other error has the port tried again at the next report, so an error that passes never leaves a forward on IPv4 alone. A machine without IPv6 on its loopback gets the port on `127.0.0.1` alone, and so does one that refuses the forwarder `::1` while it allows `127.0.0.1`, such as a security module denying IPv6 sockets, which the forwarder logs.

`devvm ports` reads these from the forwarder's record and binds nothing to find out, so it never takes a port that the forwarder is opening at that moment. A port that another VM forwards isn't one of these, so it gets `ports_held` instead. A record that the running forwarder didn't write, or none, gives none of these warnings, and neither does a record from an older devvm's forwarder after an upgrade, which lists every port it couldn't take on one line, without saying why, so devvm never says a port is in use when it may not be.

## Links between VMs

A [link between VMs](../using/links.md) carries a client VM's connections to a port that a target VM publishes. Like the forwarder, it goes over ssh, every connection starts on this machine, and it uses neither VM's network, so no backend's confinement changes and a confined VM still reaches nothing past its proxy over its own network. [The design note](design.md#links-between-vms) has the rules, the threat check and what the prototype showed.

### The addresses

Each target that a client links to gets an address of its own in `127.77.0.0/16` in that client, starting at `127.77.0.1`. devvm chooses it when the client's first link to that target is made and keeps it for every port of that target, so two targets can share a port. The client's `/etc/hosts` holds a block between `# devvm links begin` and `# devvm links end`, with a line `127.77.x.y NAME NAME.devvm.internal` for each target. The VM's setup replaces the block whole at each launch, and again when links change on a running client. The setup writes the file in place, never renaming over it, and only when the block changed.

A link listens at its target's address on the target's own port, never on the client's `127.0.0.1` or `::1`. At each start, the setup sets `net.ipv4.ip_unprivileged_port_start` to `0` where it's `1024`, which is on Lima's Alpine VMs, so a link to a port below 1024 listens on that port too. Lima's Ubuntu and Debian VMs already have `0`.

The addresses are internal to devvm. `devvm link` and its JSON never show them, and the forwarder never forwards them ([above](#what-it-forwards)).

### The link session

For each running client with links, devvm runs a hidden command, `devvm _links VM`, as one detached process on this machine. Its pid is in `links.pid` in the VM's run folder, `~/.local/share/devvm/vms/<hash>/run/`, and its log is `links.log` in the VM's logs folder, `~/.local/share/devvm/vms/<hash>/logs/`. It keeps an ssh connection of its own to the client, never the forwarder's, which starts with no forwards and asks for each link as a remote forward from `127.77.x.y:PORT` in the client to a unix socket on this machine. That's the only remote forward devvm asks a VM for. It reads the client's links again every second and adds or cancels one forward at a time as they change, and a cancel leaves the connections that are open. A link whose address and port a program in the client already holds fails alone, with `link_port_taken` in the session's log, which the next launch shows. Each line that the session's ssh processes print starts with `ssh:` in its log, so no line of theirs reads as one of the session's own, and devvm link add and the launch read only the session's own lines.

The process ends once the client no longer runs the start it was made for, or has no links left, and `devvm stop` stops it with the client, which ends its connections and removes its sockets.

Every VM's sshd allows remote forwards on `127.77.0.0/16` alone, with `AllowTcpForwarding all`, `GatewayPorts clientspecified` and `PermitListen 127.77.*:*`, written once into every image, so links never change or reload sshd. sshd matches the address a request names against that rule as text, so a remote forward on `localhost`, `127.0.0.1`, `0.0.0.0` or `::1` is refused. Root in the client can change sshd, so those settings protect only an honest VM. What keeps the VM from choosing where its connections go on this machine is the host's ssh client, which accepts a forwarded channel only for a forward it asked for and connects it only to that link's socket.

### Each connection

A program in the client that connects to `db:5432` takes this path:

```mermaid
flowchart TD
  app["your program, in the client"] -- "db:5432, which /etc/hosts<br>names as 127.77.0.1" --> csshd["the client's sshd"]
  csshd -- "the link session's ssh,<br>which this machine opened" --> links["devvm _links,<br>on this machine"]
  links -- "ssh, with the target's key<br>and pinned host key" --> tsshd["the target's sshd"]
  tsshd -- "127.0.0.1:5432,<br>its own loopback" --> server["the server, in the target"]
```

1. The client's sshd carries the connection over the link session to this machine, where ssh connects it to the link's unix socket. The socket is mode 600, in `~/.local/share/devvm/run` (mode 700), and named by a hash of the client, the address and the port, because a socket's path has fewer than 104 characters.
2. `devvm _links` checks the link again. The link must still exist, the target must be the VM it was made for, by its ID, the port must be published, the [tier rule](../using/links.md#links-and-network-tiers) must hold, and the target must be running, as its backend says. For a client on a confined tier, the target's tier must also be enforced as the target runs, as `devvm ls` shows it, because a target that got a confined tier while it ran stays outside its proxy until its next start, and one whose self-check failed may reach the internet directly. devvm never starts the target for a connection. The link session asks the target's backend whether the target is running and how its ssh reaches it, and the connections to that target share that answer for one second, so a burst of connections asks the backend once and devvm notices a target that stops within a second. For a client on a confined tier, the check of the target's tier asks the backend for each connection, so it notices at once that the target stopped, and devvm logs the connection as refused because the target isn't running, not because its tier isn't in force. An answer that a connection closing cut short isn't shared, so the other connections ask again. devvm makes every other check again for each connection, including the check of the target's tier as it runs.
3. It carries the connection over one ssh connection that it keeps to each target, with the target's key and pinned host key, and the target's sshd connects to `127.0.0.1:PORT` in the target. That connection to a target ends when the target stops, and the next one reads the target's host keys again, because a VM whose Lima config changed gets new ones when it boots.

An HTTP client that uses the VM's proxy settings sends `db:5432` to the VM's proxy instead, and the proxy connects to the same link socket on this machine ([links through the proxy](network.md#links-through-the-proxy)). Steps 2 and 3 are the same for it, so the link's checks, its cap and its events hold for both ways in.

At most 256 of a client's connections are carried at once, and the next one is refused. A link's socket holds at most 128 connections waiting to be accepted on macOS, and `devvm _links` accepts nothing while it starts a process, so it accepts a burst of connections whole before it starts any of their ssh processes: it hands them on once 5 ms pass with nothing more to accept, once the first has waited 50 ms, or once 257 wait. Each one it accepted is carried, or refused when 256 are carried already. The ssh connection to a target takes in two file descriptors for each connection it carries, more than macOS's default limit of 256 open files allows, so `devvm _links` and the port forwarder give the ssh processes they start the higher limit that devvm runs with. Each connection carried or refused is written to the client's [event log](network.md#log-files), which `devvm network log` shows, so every VM gets an event log for its links' connections, whatever its tier.

### When links change

`devvm link add` and `devvm link rm` record the change in the client's state on this machine, never in a VM. On a running client they send its setup again, which rewrites `/etc/hosts`, and they start `devvm _links` if it isn't running. `devvm _links` reads the links again every second and writes the list that the client's proxy reads, so the proxy reaches a new link as soon as its forward is in place. A stopped client gets the change when it next starts. Links change nothing else in the VM: no link name is in its `NO_PROXY` ([the VM's setup](network.md#the-vms-setup)), so no daemon restarts, and on Lima no link changes the VM's Lima config or its ssh host keys.

When a link goes, `devvm _links` stops listening for it and ends the connections that are open through it, with their ssh processes. Each connection's ssh process waits for the target's sshd to end its channel, so ending it is what keeps a target that never closes its end from holding that process, and one of the client's 256 connections, after the link has gone. When a target starts again, the connections still open through its old ssh connection end once the next connection to the target arrives, which makes a new ssh connection, or once the old one dies. A link whose name now leads to another target at the same address and port, because one link was removed and another added within the same second, ends the old target's connections the same way.

`devvm rm` of a target removes every link to it from every client's state and updates the running clients, and `devvm gc --orphans` removes links to VMs whose state is gone and the sockets that a killed link session left.

### Groups

A [group](../using/links.md#groups) is recorded in each of its VMs' own state, with the name the others reach it by, so a VM made again in a folder, which gets new state, is in none. The links a group makes are links in each VM's state like the ones `devvm link add` makes, marked as the group's, so the link session, the proxy's list and every check of a connection treat them alike. devvm works out each VM's group links again from its own links and the other VMs' published ports whenever one of them joins or leaves, publishes or unpublishes a port, or adds or removes a link of its own, and it records and applies only the VMs whose links changed, as [above](#when-links-change). [The design note](design.md#groups) has the rules and the threat check.
