---
title: devvm gc
weight: 13
explainer: "Delete stale images, unused tools, old files and the Nix cache over its cap"
---

Delete stale images, unused tools, old files and the Nix cache over its cap

## Synopsis

Delete the stale and unfinished images, the shared tools devvm images lists as unused, the files devvm itself no longer needs, and the shared Nix cache's least recently used files while it's over its cap, after listing them and asking in a terminal without --no-input. Its flags delete more, such as --tool-images for the current shared tool images no VM has. gc never deletes a VM.

```text
devvm gc [--tool-images [TOOL...]] [flags]
```

## Examples

```text
  devvm gc                             # delete stale images and unused tools, after asking
  devvm gc --tool-images               # also the current shared tool images no VM has
  devvm gc --history --older-than 30d  # also the history exports 30 days old or more
  devvm gc --force                     # delete without asking, as a script must
```

## Options

```text
      --caches                then free devvm's share of Lima's own caches, after asking
  -f, --force                 don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help                  help for gc
      --history               also delete the saved exports of agents' history
      --nix-cache             also delete the whole shared Nix cache
      --older-than DURATION   with --history, only those older than DURATION, such as 30d
      --orphans               also delete what devvm keeps for VMs their backend no longer has
      --retention             then apply the retention settings
      --tool-images           also delete the current shared tool images no VM has, or only those of the TOOLs named
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm gc](../using/images.md#cleaning-up) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
