---
title: devvm group join
weight: 16
explainer: "Put this folder's VM in a group of VMs"
---

Put this folder's VM in a group of VMs

## Synopsis

Put this folder's VM in the group NAME, which its first VM makes. Each VM in the group then reaches the ports the others publish with devvm link publish, by each one's folder name or the name it joined with, as in db:5432 and db.devvm.internal:5432.

```text
devvm group join NAME [flags]
```

## Examples

```text
  devvm group join web            # join the group web
  devvm group join web --as api2  # join it as api2 instead of this folder's name
```

## Options

```text
      --as NAME   the NAME the group's other VMs reach this VM by (default: its folder name)
  -h, --help      help for join
      --here      use this folder's own VM, never that of a folder it's inside
      --parent    use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm group join](../using/links.md#joining-a-group) explains it in full.

## See also

- [devvm group](devvm_group.md): Show the group this folder's VM is in
