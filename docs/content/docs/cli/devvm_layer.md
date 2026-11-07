---
title: devvm layer
weight: 24
explainer: "Make image layers of your own, or change this folder's VM's layers"
---

Make image layers of your own, or change this folder's VM's layers

## Synopsis

Make image layers of your own, which are scripts in ~/.config/devvm/layers that an image runs on top of base, or add layers to this folder's VM and take them out. An image names them as it names devvm's built-in layers, bottom first, such as docker+NAME, and a new VM gets them with --layers.

```text
devvm layer [flags]
```

## Examples

```text
  devvm layer new mailpit  # start a layer named mailpit
  devvm layer add docker   # make this folder's VM again with the docker layer added
  devvm layer rm docker    # make this folder's VM again without it
```

## Options

```text
  -h, --help   help for layer
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm layer](../using/customizing.md#your-own-layers) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm layer add](devvm_layer_add.md): Make this folder's VM again with more layers, then open a shell in it
- [devvm layer new](devvm_layer_new.md): Start a layer of your own
- [devvm layer rm](devvm_layer_rm.md): Make this folder's VM again without some of its layers, then open a shell in it
