---
title: devvm tools update
weight: 62
explainer: "Move this folder's VM to the newest shared tools, then open a shell in it"
---

Move this folder's VM to the newest shared tools, then open a shell in it

## Synopsis

Check for newer versions of the shared tools, build them, and move this folder's VM to them, then open a shell in it. Nothing in the VM's disk changes, because its tools are mounted.

```text
devvm tools update [flags]
```

## Examples

```text
  devvm tools update  # move the VM to the newest shared tools
```

## Options

```text
  -h, --help     help for update
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm tools update](../using/toolchains.md#adding-and-removing-tools) explains it in full.

## See also

- [devvm tools](devvm_tools.md): Show or choose this folder's VM's shared tools
