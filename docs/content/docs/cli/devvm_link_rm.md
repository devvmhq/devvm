---
title: devvm link rm
weight: 32
explainer: "Remove one of this folder's VM's links"
---

Remove one of this folder's VM's links

## Synopsis

Remove this folder's VM's link to PORT of TARGET, where TARGET is the link's name or what devvm link add took.

```text
devvm link rm TARGET PORT [flags]
```

## Examples

```text
  devvm link rm db 5432  # stop reaching db:5432
  devvm link rm pg 5432  # remove a link by the name given with --as
```

## Options

```text
  -h, --help     help for rm
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm link rm](../using/links.md#removing-a-link) explains it in full.

## See also

- [devvm link](devvm_link.md): List this folder's VM's links to your other VMs
