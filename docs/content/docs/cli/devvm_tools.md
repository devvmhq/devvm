---
title: devvm tools
weight: 59
explainer: "Show or choose this folder's VM's shared tools"
---

Show or choose this folder's VM's shared tools

## Synopsis

In a terminal, choose the toolchains and coding agents of this folder's VM in a list, which shows its layers too, then open a shell in it. Without a terminal, or with --json, show its shared tools, each with its version and any newer version in devvm's tools lock, which changes nothing, starts no VM and doesn't go online.

```text
devvm tools [flags]
```

## Examples

```text
  devvm tools                    # choose the VM's shared tools, or show them without a terminal
  devvm tools add python claude  # add python and Claude Code to the VM
  devvm tools update             # move the VM to the newest versions
```

## Options

```text
  -h, --help     help for tools
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

[The docs page for devvm tools](../using/toolchains.md#adding-and-removing-tools) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm tools add](devvm_tools_add.md): Add shared tools to this folder's VM, then open a shell in it
- [devvm tools rm](devvm_tools_rm.md): Take shared tools out of this folder's VM, then open a shell in it
- [devvm tools update](devvm_tools_update.md): Move this folder's VM to the newest shared tools, then open a shell in it
