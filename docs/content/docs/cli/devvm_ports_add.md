---
title: devvm ports add
weight: 41
explainer: "Forward more ports from this folder's VM"
---

Forward more ports from this folder's VM

## Synopsis

Add LIST's ports and ranges, such as 5173,8000-8010, to those this folder's VM forwards, or with -g to those new VMs forward. A VM that forwards every port keeps doing so.

```text
devvm ports add LIST [flags]
```

## Examples

```text
  devvm ports add 5432       # forward one more port
  devvm ports add 8000-8010  # forward a range
```

## Options

```text
  -g, --global   change what new VMs forward instead
  -h, --help     help for add
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm ports add](../using/ports-size.md#ports) explains it in full.

## See also

- [devvm ports](devvm_ports.md): Show or change which ports this folder's VM forwards
