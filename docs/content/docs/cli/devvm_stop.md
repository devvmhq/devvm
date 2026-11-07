---
title: devvm stop
weight: 58
explainer: "Stop this folder's VM"
---

Stop this folder's VM

## Synopsis

Stop this folder's VM and its proxy. devvm, devvm restart or a command that needs the VM running starts it again.

```text
devvm stop [flags]
```

## Examples

```text
  devvm stop  # stop this folder's VM
```

## Options

```text
  -h, --help     help for stop
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm stop](../using/vms.md#restarting-and-stopping) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
