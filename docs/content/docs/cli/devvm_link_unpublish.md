---
title: devvm link unpublish
weight: 33
explainer: "Stop offering ports of this folder's VM to links"
---

Stop offering ports of this folder's VM to links

## Synopsis

Stop offering PORTS, each as devvm link publish lists it, to links from your other VMs. The server in the VM keeps running.

```text
devvm link unpublish PORTS [flags]
```

## Examples

```text
  devvm link unpublish 5432  # stop offering port 5432
```

## Options

```text
  -h, --help     help for unpublish
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm link unpublish](../using/links.md#publishing-a-port) explains it in full.

## See also

- [devvm link](devvm_link.md): List this folder's VM's links to your other VMs
