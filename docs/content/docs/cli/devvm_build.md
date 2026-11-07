---
title: devvm build
weight: 7
explainer: "Build an image and the images under it, unless they're built"
---

Build an image and the images under it, unless they're built

## Synopsis

Build an image and the images under it, unless they're built. IMAGE is base, or layers joined with + bottom first, so foo+bar is bar on foo on base. An image holds no shared tools: a VM gets those when devvm makes it, or with devvm tools.

```text
devvm build IMAGE [flags]
```

## Examples

```text
  devvm build docker   # build base and the docker layer on it
  devvm build foo+bar  # build base, foo and foo+bar
```

## Options

```text
      --from IMAGE   the container IMAGE base starts from, such as node:24, instead of Ubuntu's cloud image (default: $DEVVM_FROM)
  -h, --help         help for build
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm build](../using/images.md#layers) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
