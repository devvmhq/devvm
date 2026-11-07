---
title: The desktop
explainer: How devvm desktop serves a VM's desktop
weight: 4
---

This page explains how `devvm desktop` serves a VM's desktop on this machine, how it checks a viewer's password and keeps the clipboard out of the VM, and why devvm uses VNC and never forwards X11. [A VM's desktop](../using/desktop.md) covers adding the desktop and opening it.

## In the VM

The built-in `desktop` layer installs Xfce, TigerVNC's `Xvnc`, `socat` for [the local proxy](#the-local-proxy) and a command of devvm's, `devvm-desktop`, into the `desktop` image, which only the VMs with the layer are made from ([the desktop layer](images.md#the-desktop-layer)). Its packages come from the distribution's archive as it is when the layer builds, and it downloads nothing else. When you run `devvm desktop`, devvm first checks that the VM has `devvm-desktop`, and refuses a VM without it (`desktop_no_layer`). Then it runs `devvm-desktop start off`, or `start on` with `--clipboard`, as the VM's user.

`devvm-desktop` starts `Xvnc` on display `:1` unless it's running. `Xvnc` listens on the socket `/run/devvm-desktop/vnc.sock` alone, in a folder of the VM's user's own (mode 700), the socket itself mode 600. It listens on no TCP port (`-rfbport -1`), and takes no X11 connections over TCP (`-nolisten tcp`), so devvm's port forwarder never sees it, even on a VM that forwards every port. It asks for no password of its own (`-SecurityTypes None`), because devvm checks the viewer's password on this machine first. The clipboard settings (`AcceptCutText`, `SendCutText`, `SetPrimary`, `SendPrimary`) are on or off as asked, and on a desktop that runs already, `devvm-desktop` changes them in place with `vncconfig`. Then it starts an Xfce session on the display, in a D-Bus session of its own, with the session's proxy settings pointed at [the VM's local proxy](#the-local-proxy). Nothing starts the desktop at boot, so a VM whose desktop you never open runs none. Its log is `~/.cache/devvm-desktop.log` in the VM.

## The local proxy

Every request from the VM to its [proxy](network.md#the-proxy) carries the VM's password in `Proxy-Authorization`, and the VM's proxy settings carry it in their URL, `http://devvm:KEY@192.168.5.2:PORT`. Command-line tools take the password from that URL, but Chrome, Chromium and Firefox ignore it and ask for the proxy's username and password instead, on every site. So the desktop's programs use a local proxy in the VM, `devvm-local-proxy`, which needs no password and adds the VM's.

The VM's setup writes `devvm-local-proxy` into `/usr/local/bin` at each launch, mode 700 and the VM's user's, with the VM's proxy address and password in it, and installs it whole, so a connection that's already running keeps the copy it started with. The VM already holds the same password in its environment. `devvm-desktop` runs `devvm-local-proxy start` before it starts `Xvnc`, which starts `socat` unless it's running. `socat` listens on `127.0.77.1:3128` alone, which is on the VM's loopback, takes connections only from `127.0.0.0/8`, carries at most 256 at once, and runs `devvm-local-proxy` on each connection itself. Its log is `~/.cache/devvm-local-proxy.log`. The address comes from devvm (`proxy.LocalProxyAddr` and `proxy.LocalProxyPort`), which the setup gets as `DEVVM_LOCAL_PROXY`, and `devvm-desktop` sets `http_proxy`, `https_proxy`, `HTTP_PROXY` and `HTTPS_PROXY` to `http://127.0.77.1:3128` for the Xfce session. On Xfce, Chrome and Chromium read those variables for their proxy, and Firefox reads them first when its proxy setting is the system's, which is its default.

For each connection, `devvm-local-proxy` reads the request line and the headers, waiting at most 30 seconds for each line and taking at most 100 headers. It takes only what a proxy is sent:

- a `CONNECT` to a host and port, which becomes a tunnel;
- a request for an absolute `http://` or `https://` URL, such as `GET http://example.com/`.

Anything else gets `400`, and nothing of it reaches the VM's proxy. That includes a request in origin form, such as `GET /`, which is what a web page sends when it reaches the local proxy as a server, so a page in the browser can't use it. The local proxy drops the client's own `Proxy-Authorization` and `Proxy-Connection` headers and adds the VM's password. For a request that isn't a `CONNECT`, it drops `Connection` and `Keep-Alive` and asks for `Connection: close`, because the password goes with each request's head and the local proxy reads only the first, so each connection carries one request. Then it connects to the VM's proxy, and to nothing else, sends the head and carries the rest both ways with `socat`. The VM's proxy decides as it does for any request: a host the tier allows is reached, and one it doesn't allow gets the proxy's `403` with its reason.

devvm's [port forwarder](ports.md#what-it-forwards) never forwards a server on `127.0.77.1`, on any port, because a forward would put a proxy that adds the VM's password on this machine's `localhost`, where other users and programs could reach it.

## On this machine

`devvm desktop` listens on `127.0.0.1` alone, on the VM's port. That's the port its state records in its `ports` section (`host.desktop`) while no program on this machine has it, and otherwise a new one that no program and no VM has, which devvm chooses and records in its place under the claims lock, as it does every port it chooses for a VM. New ports come from 20000 up to just below the range this machine gives outgoing connections, which starts at 32768 on Linux by default and 49152 on macOS, because Linux refuses a listener on a port that a live outgoing connection holds. devvm reads that range from the system, and when it can't, or the range leaves fewer than 1000 ports above 20000, it picks from 20000 to 32767. The port is among those devvm records for VMs, with their ssh and proxy ports, so no VM's [port forwarder](ports.md#what-it-never-takes) takes it and no new VM gets it for its ssh or its proxy. devvm listens without address reuse, so a program that has the port on any address keeps it. The listener stays open until you press Ctrl+C, or until the VM no longer runs, which devvm checks every 5 seconds (`desktop_vm_stopped`). Nothing else listens for the desktop on this machine, at any other time.

Each connection to the port goes through devvm's relay, which runs in `devvm desktop`:

1. The relay speaks VNC's protocol (RFB 3.8, and 3.7 and 3.3 for older viewers) as a server, and offers VNC's own authentication alone. It sends a random challenge and checks the viewer's answer against the VM's password, with one check at a time, so a wrong answer holds every other viewer's check for a second. A viewer whose answer is wrong is refused and never reaches the VM.
2. Only then does the relay connect to the VM: an ssh channel to the VM's socket (`ssh -W /run/devvm-desktop/vnc.sock`), through devvm's ssh config's entry for the VM, with its pinned host key. The channel opens no session, and it carries no agent and no forwarding either way (`IdentityAgent=none`, `ForwardAgent=no`, `ForwardX11=no`, `ClearAllForwardings=yes`). The relay does the viewer's side of the VM's handshake, asking for no authentication, and tells the viewer it's in.
3. From then on it carries the viewer's messages to the VM and the VM's to the viewer. Without `--clipboard`, it reads each of the viewer's messages and drops its clipboard ones (`ClientCutText`, extended clipboard ones included). It ends a connection that sends a message it doesn't know, so nothing it can't read reaches the VM.

At most 8 viewers connect at once. The VM's password is 8 characters of lowercase letters and digits without `0`, `o`, `1`, `l` and `i`, about 40 bits, because VNC's authentication uses 8 characters at most. devvm makes it the first time and keeps it in the VM's state, in its `desktop` section (`made.password`), in a file that only you can read. It never goes into the VM, and devvm prints it only with the address, never into the run log.

## What the VM can and can't change

Root in the VM can change its desktop, or run another server on the socket in its place. The relay doesn't depend on the VM's server for its guarantees:

- **The password:** the relay checks it on this machine before it opens the channel, so a VM's server that asks for none still gets no viewer without it.
- **The address:** the relay listens on this machine's loopback, so no server in the VM reaches other machines through it. A server root starts on a TCP port of the VM's is an ordinary server of the VM's, which the VM's [ports](../using/ports-size.md#ports) forward to this machine's loopback, or don't, as for any other.
- **This machine's clipboard:** without `--clipboard`, the relay drops it, whatever the VM's server accepts.
- **No way out:** every connection starts on this machine, and the VM opens none to it through the relay. The channel is a local forward that devvm's ssh asks for, which the VM's sshd connects to the VM's own socket. On a confined tier, the VM's own connections still reach only its proxy.

The relay can't stop a VM's server from sending its clipboard to the viewer, because that would mean reading every screen update the server sends. So the VM's clipboard stays off this machine only while its server is as devvm set it up, or while the viewer doesn't take it, which Screen Sharing's Use Shared Clipboard setting decides.

## Why not X11

ssh's X11 forwarding gives programs in the VM a connection to this machine's display, and an X11 program on a display can read the keys typed into other windows on it, read their contents and send them input. So devvm turns X11 forwarding off on both sides of the VM's ssh. Its ssh config's entries set `ForwardX11 no`, and the VM's sshd has `X11Forwarding no` ([ssh into VMs](security.md#ssh-into-vms)). With VNC, the VM's programs draw on a display of the VM's own, and this machine only shows a picture of it, so nothing in the VM reaches this machine's windows.
