---
title: devvm restart
weight: 44
explainer: "Restart this folder's VM and open a shell in it"
---

Restart this folder's VM and open a shell in it

## Synopsis

Stop this folder's VM, then start it and open a shell in it as devvm does. Changes that apply only when a VM starts, such as a new size or an agent's folders, apply then.

```text
devvm restart [flags]
```

## Examples

```text
  devvm restart  # restart this folder's VM and open a shell in it
```

## Options

```text
  -h, --help     help for restart
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm restart](../using/vms.md#restarting-and-stopping) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
