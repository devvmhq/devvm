---
title: devvm tools add
weight: 60
explainer: "Add shared tools to this folder's VM, then open a shell in it"
---

Add shared tools to this folder's VM, then open a shell in it

## Synopsis

Add shared tools to this folder's VM, then open a shell in it. TOOL is a toolchain, such as node, or a coding agent, such as claude, each a separate word. devvm builds any of them that aren't built, and for a new agent in a running VM it asks before restarting the VM.

```text
devvm tools add TOOL... [flags]
```

## Examples

```text
  devvm tools add claude        # add Claude Code to the VM
  devvm tools add python codex  # add python and Codex
```

## Options

```text
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

[The docs page for devvm tools add](../using/toolchains.md#adding-and-removing-tools) explains it in full.

## See also

- [devvm tools](devvm_tools.md): Show or choose this folder's VM's shared tools
