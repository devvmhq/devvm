---
title: devvm doctor
weight: 12
explainer: "Check what devvm needs on this machine"
---

Check what devvm needs on this machine

## Synopsis

Check what Lima needs on this machine, and say how to get what's missing, because devvm never installs it itself. Then list the features Lima gives VMs. doctor fails when something is missing, but never for a warning, such as one about low free space.

```text
devvm doctor [flags]
```

## Examples

```text
  devvm doctor         # check what devvm needs
  devvm doctor --json  # the same as JSON, for scripts
```

## Options

```text
  -h, --help   help for doctor
      --json   print what it found as JSON, and succeed whatever is missing
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm doctor](../getting-started/install.md#checking-what-devvm-needs) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
