---
title: devvm clone
weight: 8
explainer: "Show or change whether this folder's VM has its own clone of the repo"
---

Show or change whether this folder's VM has its own clone of the repo

## Synopsis

Show whether this folder's VM has its own clone of the repo, on, or mounts this folder, off. devvm clone on and devvm clone off make the VM again from its image, after asking in a terminal without --no-input, then open a shell in it, and devvm keeps its home folder and its settings, while everything else on its disk is lost. On copies the repo's branches into the new clone, and off first offers to fetch the clone's work into this repo, unless you give --no-clone-check.

```text
devvm clone [on|off] [flags]
```

## Examples

```text
  devvm clone             # show whether the VM has its own clone of the repo
  devvm clone on          # make the VM again with its own clone of the repo, after asking
  devvm clone off         # fetch the clone's work, then make the VM again with this folder mounted
  devvm clone on --force  # make it again without asking, as a script must
```

## Options

```text
      --clean-home       give the VM the image's home folder instead of keeping its own
  -f, --force            don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help             help for clone
      --here             use this folder's own VM, never that of a folder it's inside
      --json             print it as one JSON object, for scripts
      --no-clone-check   don't check a --clone VM's clone for unfetched commits and uncommitted changes first, which are lost
      --no-export        don't export the agents' history first
      --parent           use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm clone](../using/clone.md#switching-a-vm-that-exists) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
