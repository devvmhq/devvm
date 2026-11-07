---
title: devvm tools rm
weight: 61
explainer: "Take shared tools out of this folder's VM, then open a shell in it"
---

Take shared tools out of this folder's VM, then open a shell in it

## Synopsis

Take shared tools out of this folder's VM, each TOOL a separate word, then open a shell in it. What the VM installed itself stays.

```text
devvm tools rm TOOL... [flags]
```

## Examples

```text
  devvm tools rm python         # take python out of the VM
  devvm tools rm python claude  # take out python and Claude Code
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

[The docs page for devvm tools rm](../using/toolchains.md#adding-and-removing-tools) explains it in full.

## See also

- [devvm tools](devvm_tools.md): Show or choose this folder's VM's shared tools
