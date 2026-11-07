---
title: devvm code
weight: 9
explainer: "Open this folder's VM in VS Code over Remote-SSH"
---

Open this folder's VM in VS Code over Remote-SSH

## Synopsis

Get this folder's VM ready as devvm host does, then open its folder in VS Code over Remote-SSH, with VS Code's code command.

```text
devvm code [flags]
```

## Examples

```text
  devvm code               # open this folder's VM in VS Code
  devvm code --ssh-config  # the same, adding devvm's Include line to ~/.ssh/config without asking
```

## Options

```text
  -h, --help         help for code
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

[The docs page for devvm code](../using/ssh.md#vs-code) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
