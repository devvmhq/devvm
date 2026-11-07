---
title: devvm layer rm
weight: 27
explainer: "Make this folder's VM again without some of its layers, then open a shell in it"
---

Make this folder's VM again without some of its layers, then open a shell in it

## Synopsis

Make this folder's VM again from an image without each LAYER, its other layers kept in their order, then open a shell in it, after asking in a terminal without --no-input. devvm keeps its home folder, the project folder and its settings, and everything else on its disk is lost. When its image has an agent, devvm first exports the agents' history, unless you give --no-export.

```text
devvm layer rm LAYER... [flags]
```

## Examples

```text
  devvm layer rm desktop              # take the desktop layer out of the VM, after asking
  devvm layer rm docker nix           # take out docker and nix
  devvm layer rm desktop --force      # take it out without asking, as a script must
  devvm layer rm docker --clean-home  # take out docker, and start from the image's home folder
```

## Options

```text
      --clean-home   give the VM the image's home folder instead of keeping its own
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help         help for rm
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

[The docs page for devvm layer rm](../using/images.md#taking-layers-out-of-a-vm) explains it in full.

## See also

- [devvm layer](devvm_layer.md): Make image layers of your own, or change this folder's VM's layers
