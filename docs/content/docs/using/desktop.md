---
title: A VM's desktop
explainer: Add a desktop to a VM and open it
weight: 12
---

A VM has no screen of its own, but with the `desktop` layer it gets an Xfce desktop, which `devvm desktop` opens in a window on this machine. This page covers adding the desktop, opening it, its password and the clipboard, and what it costs. [The desktop](../internals/desktop.md) explains how devvm keeps it to this machine.

## Adding the desktop

The desktop is one of devvm's [built-in layers](images.md#built-in-layers), so only the VMs you choose it for get it, and `base` and the other VMs stay without it. To add it to a new VM, check `desktop` under Layers in the list devvm shows when you customize a new VM, or name it with `--layers`:

```sh
devvm --layers desktop          # a VM of base with the desktop
devvm --layers docker,desktop   # the desktop on top of the docker layer
```

devvm builds the `desktop` image the first time a VM needs it, and VMs that choose it share that image. `--layers` is the exact list of the VM's layers, so name every layer you want the VM to have.

To give every new VM the desktop, choose it with `devvm setup --layers desktop`, or add it to the layers you already chose, such as `devvm setup --layers docker,desktop`. New VMs then get it unless you leave it out when you customize them or give `--layers` without it ([layers for new VMs](settings.md#layers-for-new-vms)). VMs that exist keep the layers they were made with.

A VM made without the desktop has none, and `devvm desktop` says so. To give it one, run `devvm layer add desktop`, which makes the VM again from an image with the desktop on top of its own layers, such as `docker+desktop`. The VM keeps its home folder, the project folder and its settings, and loses the rest of its disk, such as the packages you installed, so devvm asks first ([adding layers to a VM](images.md#adding-layers-to-a-vm)). `devvm layer rm desktop` takes the desktop out again in the same way ([taking layers out of a VM](images.md#taking-layers-out-of-a-vm)).

```sh
devvm layer add desktop   # make this folder's VM again with the desktop, after asking
devvm layer rm desktop    # make it again without the desktop
```

The desktop works on base, and on a base made `--from` a Debian, Ubuntu or Alpine image.

## Opening it

```sh
devvm desktop              # serves this folder's VM's desktop and opens it in Screen Sharing
devvm desktop --no-open    # only shows its address and password
devvm desktop --clipboard  # shares the clipboard both ways, for this run
```

`devvm desktop` gets the VM ready as `devvm host` does, and starts it if it's stopped. Then it starts the VM's desktop, unless it's running already, and shows its address and password:

```text
devvm-website-0123456789abcd's desktop is at
vnc://127.0.0.1:41234
Password: k3mxq7tw
It stays there until you press Ctrl+C.
```

On macOS, devvm opens the address in Screen Sharing, which comes with macOS and asks for the password. Screen Sharing can keep the password in your keychain, and the address stays the same each time. On Linux, open the address in a VNC viewer of your own, such as TigerVNC's `vncviewer` or Remmina.

The desktop stays at that address until you press Ctrl+C, or until the VM stops, and then `devvm desktop` ends. The desktop goes on running in the VM, so the windows you left open are there the next time you run `devvm desktop`. It's 1600 by 1000 pixels.

The desktop is only ever on this machine, and only while `devvm desktop` runs:

- devvm serves it on this machine's `127.0.0.1` alone, so nothing else on your network reaches it. Each VM keeps its port, which no other VM and no program on this machine has. When a program has taken it, devvm chooses another one and keeps that.
- In the VM, the desktop listens on no port at all, so a VM that [forwards every port](ports-size.md#ports) never forwards it.
- On a confined [network tier](../safety/network.md#network-tiers), the VM still reaches only its proxy while you use its desktop.

## Browsers

The desktop has no browser of its own, so install the one you want in the VM, such as Google Chrome, Chromium or Firefox. Browsers reach the web through the VM's proxy without asking you for a password, and so do the desktop's other programs. Browsers ask for a proxy's password rather than read it from the VM's proxy settings, so the desktop's programs use the VM's local proxy instead. The local proxy is a small proxy in the VM, at `http://127.0.77.1:3128`, which adds the VM's password to each request and sends it on to the VM's proxy. A browser follows the desktop's proxy settings unless you've given it one of its own.

The local proxy changes nothing about what the VM may reach. A host that the VM's [network tier](../safety/network.md#network-tiers) doesn't allow is still blocked, and a plain `http://` page shows the proxy's reason and the `devvm network allow` command that allows it. An `https://` page shows the browser's own error instead, such as Chrome's `ERR_TUNNEL_CONNECTION_FAILED`, so run `devvm-why` in the desktop's terminal to see what was blocked.

The local proxy listens on the VM's loopback alone, and devvm never forwards it to this machine, even on a VM that [forwards every port](ports-size.md#ports). It starts with the desktop and runs until the VM stops.

## The password

Each VM has its own password of eight lowercase letters and digits. devvm makes it the first time you run `devvm desktop` and keeps it with the VM's state on this machine, so `devvm rm` deletes it. The password never goes into the VM, and it isn't in devvm's run log. devvm checks it on this machine before a viewer reaches the VM, whatever the VM's own desktop asks for.

Other users and programs on this machine can also connect to `127.0.0.1`, so the password is what keeps them out. VNC passwords are eight characters at most, and devvm lets a viewer try one password a second, so guessing it would take thousands of years.

## The clipboard

The clipboard isn't shared unless you add `--clipboard`. With sharing on, what you copy on this machine goes into the VM, where a coding agent can read it, and what the VM copies lands on this machine's clipboard. Without `--clipboard`, devvm keeps this machine's clipboard out of the VM, even when something in the VM has changed its desktop's settings, and the VM's desktop doesn't send its clipboard.

`--clipboard` turns sharing on in both directions for that run, and the next run without it turns sharing off again. devvm can't stop a VM whose desktop was changed from sending its clipboard to Screen Sharing, so when you don't want the VM's clipboard on this machine, also turn off Use Shared Clipboard in Screen Sharing's Edit menu.

## What it costs

- About 370 MB of disk for the `desktop` image, which every VM with the layer shares, while `base` and the VMs without the layer stay that much smaller.
- About 500 MB of the VM's memory while its desktop runs, which it does from your first `devvm desktop` until the VM stops. A VM whose desktop you never open uses none.

## When the desktop doesn't open

- **The VM has no desktop:** it was made without the `desktop` layer. Run `devvm layer add desktop` to make it again with the layer, as [adding the desktop](#adding-the-desktop) explains.
- **The desktop didn't start:** devvm shows the end of its run log. The desktop's own log is `~/.cache/devvm-desktop.log` in the VM.
- **Screen Sharing didn't open:** open the address devvm shows in Screen Sharing yourself, by typing it into Finder's Go > Connect to Server, or in another VNC viewer.
- **Screen Sharing says the password is wrong:** use the password `devvm desktop` shows for this VM. Each VM has its own.
