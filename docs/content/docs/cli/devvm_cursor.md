---
title: devvm cursor
explainer: "Open this folder's VM in Cursor over Remote-SSH"
---

Open this folder's VM in Cursor over Remote-SSH

## Synopsis

Get this folder's VM ready as devvm host does, then open its folder in the Cursor editor over Remote-SSH, with Cursor's cursor command.

```text
devvm cursor [flags]
```

## Examples

```text
  devvm cursor               # open this folder's VM in Cursor
  devvm cursor --ssh-config  # the same, adding devvm's Include line to ~/.ssh/config without asking
```

## Options

```text
  -h, --help         help for cursor
      --here         use this folder's own VM, never that of a folder it's inside
      --parent       use the VM of the nearest folder this one is inside, even across a git repo of its own
      --ssh-config   add devvm's Include line to ~/.ssh/config without asking
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm cursor](../using/ssh.md#vs-code-and-cursor) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
