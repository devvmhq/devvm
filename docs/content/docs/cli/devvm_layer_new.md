---
title: devvm layer new
weight: 26
explainer: "Start a layer of your own"
---

Start a layer of your own

## Synopsis

Start a layer of your own, ~/.config/devvm/layers/NAME.sh, a script to edit, and NAME.policy next to it, the hosts that VMs with the layer can reach on the allowlist tier. NAME is lowercase letters, digits and dashes, and not base or a built-in layer's or shared tool's name.

```text
devvm layer new NAME [flags]
```

## Examples

```text
  devvm layer new mailpit  # start the mailpit layer, then edit its script
  devvm build mailpit      # build its image
  devvm --layers mailpit   # create a VM with the layer
```

## Options

```text
  -h, --help   help for new
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm layer new](../using/customizing.md#your-own-layers) explains it in full.

## See also

- [devvm layer](devvm_layer.md): Make image layers of your own, or change this folder's VM's layers
