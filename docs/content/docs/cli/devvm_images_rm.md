---
title: devvm images rm
weight: 23
explainer: "Delete images, by name or by key"
---

Delete images, by name or by key

## Synopsis

Delete images, each by its name or by the key devvm images shows, after asking in a terminal without --no-input. Every one must be built, or devvm deletes nothing. VMs made from an image keep working, except on Linux hosts, where devvm refuses an image whose disk VMs use.

```text
devvm images rm IMAGE|KEY... [flags]
```

## Examples

```text
  devvm images rm foo+bar              # delete the foo+bar image, after asking
  devvm images rm 7a76021c46ef         # delete a stale image by its key
  devvm images rm base --from node:24  # delete base made from node:24
```

## Options

```text
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
      --from IMAGE   the container IMAGE base starts from, such as node:24, instead of Ubuntu's cloud image (default: $DEVVM_FROM)
  -h, --help         help for rm
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm images rm](../using/images.md#deleting-images) explains it in full.

## See also

- [devvm images](devvm_images.md): List the images and the shared tools
