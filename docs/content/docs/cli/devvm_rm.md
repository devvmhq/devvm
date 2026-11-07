---
title: devvm rm
weight: 46
explainer: "Delete this folder's VM, or a VM whose folder is gone"
---

Delete this folder's VM, or a VM whose folder is gone

## Synopsis

Delete this folder's VM and what devvm keeps for it, or the VM you name when its folder is gone, after asking in a terminal without --no-input, which --force never answers when its folder is on a disk that isn't connected. When its image has an agent, devvm first exports the agents' history, unless you give --no-export. A --clone VM's work that this repo hasn't fetched is offered for fetching first, unless you give --no-clone-check.

```text
devvm rm [VM] [flags]
```

## Examples

```text
  devvm rm                           # delete this folder's VM, after asking
  devvm rm --force                   # delete it without asking, as a script must
  devvm rm --no-export               # delete it without exporting the agents' history
  devvm rm --no-clone-check          # delete a --clone VM without checking its clone, losing its unfetched work
  devvm rm devvm-web-71a99b1076d92c  # delete a VM whose folder is gone, by its name as devvm ls lists it
```

## Options

```text
  -f, --force            don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help             help for rm
      --here             use this folder's own VM, never that of a folder it's inside
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

[The docs page for devvm rm](../using/vms.md#deleting-a-vm) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
