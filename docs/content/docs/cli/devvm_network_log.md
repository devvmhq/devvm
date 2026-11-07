---
title: devvm network log
weight: 38
explainer: "Show what this folder's VM connected to and what its proxy blocked"
---

Show what this folder's VM connected to and what its proxy blocked

## Synopsis

Show the network log of this folder's VM, oldest first: one line per connection or request its proxy allowed or blocked, and one line per connection to one of its links and each that devvm refused. --blocked lists the blocks instead, one row per host and port, with the devvm network allow command that allows each.

```text
devvm network log [flags]
```

## Examples

```text
  devvm network log            # show the last 50 connections and requests
  devvm network log --follow   # keep showing new ones until Ctrl+C
  devvm network log --blocked  # list the blocks, with the command that allows each
```

## Options

```text
      --blocked   list the blocks by host and port, with the command that allows each
      --follow    keep showing new ones as they happen, until interrupted
  -h, --help      help for log
      --here      use this folder's own VM, never that of a folder it's inside
      --json      print it as one JSON object, for scripts
  -n, --lines N   show the last N connections and requests (default 50)
      --parent    use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm network log](../safety/network.md#logs) explains it in full.

## See also

- [devvm network](devvm_network.md): Show or change the network tier of this folder's VM
