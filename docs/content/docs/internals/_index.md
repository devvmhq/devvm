---
title: How it works
explainer: How devvm works, for reviewers and contributors
weight: 95
---

These pages explain how devvm does what the other sections describe, for readers who want to check a claim, such as security reviewers and agents. They name the files devvm ships and the messages it prints. You don't need them to use devvm.

- [Overview](overview.md): what devvm does, step by step, when it makes, opens, stops and deletes a VM, and the state files it keeps.
- [Installing and updating](install.md): what the install script downloads and checks, `devvm doctor --json`, devvm's own files, and upgrading.
- [Images and caching](images.md): how images and shared tools are built, keyed and cached, and how devvm checks what they start from.
- [Port forwarding](ports.md): how devvm's forwarder forwards a VM's ports to your machine.
- [The desktop](desktop.md): how `devvm desktop` serves a VM's desktop on your machine, checks its password and keeps your clipboard out, and why devvm doesn't forward X11.
- [Network tiers](network.md): each tier's confinement, the proxy, the self-check, and how secrets get into requests.
- [.git protection](git-protection.md): what devvm locks, when it checks the locks, and what they can't stop.
- [Security model](security.md): what devvm protects on your machine and how, and what it doesn't protect.
- [Backends](backends.md): each backend's features, native or emulated, and what happens when one is missing.
- [Lima](lima.md): devvm's Lima home, confinement, clones and port forwarding on Lima.
- [Design notes](design.md): why devvm is built this way, what failed, and what's still open.
