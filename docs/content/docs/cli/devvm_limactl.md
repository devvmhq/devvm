---
title: devvm limactl
weight: 28
explainer: "Run Lima's limactl for debugging, which starts VMs unconfined, whatever their tier"
---

Run Lima's limactl for debugging, which starts VMs unconfined, whatever their tier

## Synopsis

Run limactl with ARGS on devvm's own Lima home, where the lima backend's VMs and images live, passing on every argument, --help included. A VM started this way runs unconfined, whatever its tier, so start VMs with devvm.

```text
devvm limactl [ARGS...]
```

## Examples

```text
  devvm limactl list                                # list the lima backend's VMs and images
  devvm limactl shell devvm-website-71a99b1076d92c  # open a shell in a running VM through Lima
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm limactl](../backends/lima.md#running-limactl) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
