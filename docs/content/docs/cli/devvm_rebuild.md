---
title: devvm rebuild
weight: 43
explainer: "Build an image again, and delete the old one"
---

Build an image again, and delete the old one

## Synopsis

Build an image again from its parent as it is, then delete the stale images no VM uses, the old one and those built on it among them. When images are built on it, devvm asks first in a terminal without --no-input.

```text
devvm rebuild IMAGE [flags]
```

## Examples

```text
  devvm rebuild foo           # build foo again with the newest packages
  devvm rebuild base --force  # rebuild base without asking, as a script must
```

## Options

```text
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
      --from IMAGE   the container IMAGE base starts from, such as node:24, instead of Ubuntu's cloud image (default: $DEVVM_FROM)
  -h, --help         help for rebuild
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm rebuild](../using/images.md#rebuilding) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
