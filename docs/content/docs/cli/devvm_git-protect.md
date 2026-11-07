---
title: devvm git-protect
weight: 14
explainer: "Show or change .git protection for this folder's VM"
---

Show or change .git protection for this folder's VM

## Synopsis

Show this folder's VM's .git protection, which locks the git hooks and config that run code on this machine, as on, strict, off, or clone for a --clone VM. devvm git-protect on, strict or off changes it at once on a stopped VM, and on a running one when it next stops, so in a terminal devvm offers to restart it now.

```text
devvm git-protect [on|strict|off] [flags]
```

## Examples

```text
  devvm git-protect      # show the VM's .git protection
  devvm git-protect off  # unlock .git, once the VM is stopped
  devvm git-protect on   # lock .git again as it is then
```

## Options

```text
  -h, --help     help for git-protect
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

[The docs page for devvm git-protect](../safety/git-protection.md#turning-it-on-or-off) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
