---
title: devvm ls
weight: 34
explainer: "List the project VMs"
---

List the project VMs

## Synopsis

List the project VMs, sorted by name. Each row has the VM's status, image, .git protection, network tier, links to your other VMs and folder.

```text
devvm ls [flags]
```

## Examples

```text
  devvm ls         # list every project VM
  devvm ls --json  # the same list as JSON, for scripts
```

## Options

```text
  -h, --help   help for ls
      --json   print it as one JSON object, for scripts
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm ls](../using/vms.md#listing-vms) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
