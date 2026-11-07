---
title: devvm space
weight: 54
explainer: "Show what devvm keeps on this machine and the space it takes"
---

Show what devvm keeps on this machine and the space it takes

## Synopsis

Show each kind of thing devvm keeps on this machine, with the disk it takes, what devvm gc frees of it now and what frees the rest, then Lima's own caches apart. It changes nothing and starts no VM.

```text
devvm space [flags]
```

## Examples

```text
  devvm space         # show what devvm keeps and what frees it
  devvm space --json  # the same as JSON, for scripts
```

## Options

```text
  -h, --help   help for space
      --json   print it as one JSON object, for scripts
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm space](../using/space.md#devvm-space) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
