---
title: devvm group leave
weight: 17
explainer: "Take this folder's VM out of its group"
---

Take this folder's VM out of its group

## Synopsis

Take this folder's VM out of its group, and remove the links the group made, in this VM and in the group's other VMs, naming them. The links you made with devvm link add stay, and the group goes with its last VM.

```text
devvm group leave [flags]
```

## Examples

```text
  devvm group leave  # leave this folder's VM's group
```

## Options

```text
  -h, --help     help for leave
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm group leave](../using/links.md#leaving-a-group) explains it in full.

## See also

- [devvm group](devvm_group.md): Show the group this folder's VM is in
