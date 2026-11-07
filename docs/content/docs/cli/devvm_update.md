---
title: devvm update
weight: 63
explainer: "Bring this folder's VM up to date, then open a shell in it"
---

Bring this folder's VM up to date, then open a shell in it

## Synopsis

Move this folder's VM to the newest shared tools, as devvm tools update does, then open a shell in it. When its image changed since devvm made the VM, or the folder's devcontainer.json did, devvm then asks to make the VM again from the image's new build and the file as it is now, keeping its home folder, the project folder and its settings, and everything else on its disk is lost. When its image has an agent, devvm first exports the agents' history, unless you give --no-export.

```text
devvm update [flags]
```

## Examples

```text
  devvm update               # move the VM to the newest tools, and to its image's new build after asking
  devvm update --force       # the same without asking, as a script must
  devvm update --clean-home  # start from the image's home folder too
```

## Options

```text
      --clean-home   give the VM the image's home folder instead of keeping its own
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help         help for update
      --here         use this folder's own VM, never that of a folder it's inside
      --no-export    don't export the agents' history first
      --parent       use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm update](../using/images.md#outdated-vms) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
