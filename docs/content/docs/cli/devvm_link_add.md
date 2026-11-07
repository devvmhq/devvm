---
title: devvm link add
weight: 30
explainer: "Link this folder's VM to a port of another VM"
---

Link this folder's VM to a port of another VM

## Synopsis

Link this folder's VM to PORT of the VM TARGET names, a folder's name or path or a VM's name, once that VM publishes the port with devvm link publish. Programs in this VM then reach it as NAME:PORT and NAME.devvm.internal:PORT, where NAME is the target's folder name or --as NAME.

```text
devvm link add TARGET PORT [flags]
```

## Examples

```text
  devvm link add db 5432          # reach port 5432 of the VM for the folder db as db:5432
  devvm link add db 5432 --as pg  # reach it as pg:5432 instead
  devvm link add ../api 8080      # name the target by its folder's path
```

## Options

```text
      --as NAME   the NAME the VM reaches the link by (default: the target's folder name)
  -h, --help      help for add
      --here      use this folder's own VM, never that of a folder it's inside
      --parent    use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm link add](../using/links.md#adding-a-link) explains it in full.

## See also

- [devvm link](devvm_link.md): List this folder's VM's links to your other VMs
