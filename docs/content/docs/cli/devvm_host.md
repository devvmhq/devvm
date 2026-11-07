---
title: devvm host
weight: 21
explainer: "Get this folder's VM ready for ssh and editors, and show its host name"
---

Get this folder's VM ready for ssh and editors, and show its host name

## Synopsis

Get this folder's VM ready for ssh and editors as devvm does before it opens a shell, starting the VM if it's stopped. Then show the host name that ssh and VS Code's Remote-SSH reach it by, such as devvm-website for a folder named website.

```text
devvm host [flags]
```

## Examples

```text
  devvm host         # get the VM ready and show its host name
  ssh devvm-website  # then connect by that name, for a folder named website
```

## Options

```text
  -h, --help         help for host
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

[The docs page for devvm host](../using/ssh.md#devvm-host-and-devvm-code) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
