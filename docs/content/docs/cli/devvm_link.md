---
title: devvm link
weight: 29
explainer: "List this folder's VM's links to your other VMs"
---

List this folder's VM's links to your other VMs

## Synopsis

List this folder's VM's links to services in your other VMs, each with its name, port, target VM and folder, the target's status, and whether the link applies. Programs in the VM reach a link as NAME:PORT and NAME.devvm.internal:PORT.

```text
devvm link [flags]
```

## Examples

```text
  devvm link               # list this folder's VM's links
  devvm link add db 5432   # reach port 5432 of the VM for the folder db as db:5432
  devvm link publish 5432  # offer this VM's port 5432 to links from your other VMs
```

## Options

```text
  -h, --help     help for link
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

[The docs page for devvm link](../using/links.md#listing-links) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm link add](devvm_link_add.md): Link this folder's VM to a port of another VM
- [devvm link publish](devvm_link_publish.md): Show or add the ports this folder's VM offers to links
- [devvm link rm](devvm_link_rm.md): Remove one of this folder's VM's links
- [devvm link unpublish](devvm_link_unpublish.md): Stop offering ports of this folder's VM to links
