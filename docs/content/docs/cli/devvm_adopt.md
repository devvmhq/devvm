---
title: devvm adopt
weight: 2
explainer: "Give this folder the VM made for an earlier folder or path"
---

Give this folder the VM made for an earlier folder or path

## Synopsis

Give this folder the VM made for an earlier folder at this path, or for this folder before you moved or renamed it, with its files, secrets and allowlist. With VM, its name as devvm ls lists it, devvm moves that VM here from a folder that's gone, such as one you copied to another disk. devvm asks first in a terminal without --no-input.

```text
devvm adopt [VM] [flags]
```

## Examples

```text
  devvm adopt                           # give this folder the VM, after asking
  devvm adopt --force                   # give it without asking, as a script must
  devvm adopt devvm-web-71a99b1076d92c  # move that VM here from a folder that's gone, after asking
```

## Options

```text
  -f, --force   don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help    help for adopt
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm adopt](../using/vms.md#a-folder-made-again) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
