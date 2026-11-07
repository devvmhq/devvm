---
title: devvm layer add
weight: 25
explainer: "Make this folder's VM again with more layers, then open a shell in it"
---

Make this folder's VM again with more layers, then open a shell in it

## Synopsis

Make this folder's VM again from an image with each LAYER added on top, in the order given, then open a shell in it, after asking in a terminal without --no-input. devvm keeps its home folder, the project folder and its settings, and everything else on its disk is lost. When its image has an agent, devvm first exports the agents' history, unless you give --no-export.

```text
devvm layer add LAYER... [flags]
```

## Examples

```text
  devvm layer add desktop              # add the desktop layer to the VM, after asking
  devvm layer add docker nix           # add docker, then nix on top of it
  devvm layer add desktop --force      # add it without asking, as a script must
  devvm layer add docker --clean-home  # add docker, and start from the image's home folder
```

## Options

```text
      --clean-home   give the VM the image's home folder instead of keeping its own
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help         help for add
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

[The docs page for devvm layer add](../using/images.md#adding-layers-to-a-vm) explains it in full.

## See also

- [devvm layer](devvm_layer.md): Make image layers of your own, or change this folder's VM's layers
