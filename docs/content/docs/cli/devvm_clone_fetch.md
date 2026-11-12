---
title: devvm clone fetch
explainer: "Fetch the work of this folder's --clone VM into each of its repos"
---

Fetch the work of this folder's --clone VM into each of its repos

## Synopsis

Fetch every branch of each repo in this folder's --clone VM into the repo on this machine, as remote-tracking branches of its remote for the VM, such as devvm/main, starting the VM if it's stopped. When a repo's HEAD in the VM is detached at commits no branch has, fetch those into a new local branch, saved/devvm/HEAD, and each stash entry the VM made into saved/devvm/stash-N. Then list the branches that are new or moved, each with the repos it's in.

```text
devvm clone fetch [flags]
```

## Examples

```text
  devvm clone fetch         # fetch every repo's branches, and list what's new
  devvm clone fetch --json  # list what's new for a script
```

## Options

```text
  -h, --help     help for fetch
      --here     use this folder's own VM, never that of a folder it's inside
      --json     print it as one JSON object, for scripts
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm clone fetch](../using/clone.md#fetching-the-vms-work) explains it in full.

## See also

- [devvm clone](devvm_clone.md): Show or change whether this folder's VM has its own clone of the repo
