---
title: devvm group
weight: 15
explainer: "Show the group this folder's VM is in"
---

Show the group this folder's VM is in

## Synopsis

Show the group this folder's VM is in, with each of its VMs, the name the others reach it by, its folder and the ports it publishes. The VMs in a group reach each other's published ports by name, through links the group makes in each of them.

```text
devvm group [flags]
```

## Examples

```text
  devvm group           # show this folder's VM's group
  devvm group join web  # join the group web, made when its first VM joins
  devvm group leave     # leave it, removing the links the group made
```

## Options

```text
  -h, --help     help for group
      --here     use this folder's own VM, never that of a folder it's inside
      --json     print it as one JSON object, for scripts
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm group](../using/links.md#groups) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm group join](devvm_group_join.md): Put this folder's VM in a group of VMs
- [devvm group leave](devvm_group_leave.md): Take this folder's VM out of its group
