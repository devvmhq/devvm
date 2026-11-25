---
title: devvm clone
explainer: "Show or change whether this folder's VM has its own clone of the repo"
---

Show or change whether this folder's VM has its own clone of the repo

## Synopsis

Show whether this folder's VM has its own clone of the repo, or of each repo in a folder of repos, on, or mounts this folder, off. devvm clone on and devvm clone off make the VM again from its image, after asking in a terminal without --no-input, then open a shell in it, and devvm keeps its home folder and its settings, while everything else on its disk is lost. On copies the repos' branches into the new clone, and off first offers to fetch the clone's work into this machine's repos, unless you give --no-clone-check.

```text
devvm clone [on|off] [flags]
```

## Examples

```text
  devvm clone             # show whether the VM has its own clone of the repo
  devvm clone on          # make the VM again with its own clone of the repo, after asking
  devvm clone off         # fetch the clone's work, then make the VM again with this folder mounted
  devvm clone on --force  # make it again without asking, as a script must
  devvm clone fetch       # fetch the VM's branches into each repo on this machine
```

## Options

```text
      --clean-home          give the VM the image's home folder instead of keeping its own
  -f, --force               don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help                help for clone
      --here                use this folder's own VM, never that of a folder it's inside
      --json                print it as one JSON object, for scripts
      --no-clone-check      don't check a --clone VM's clone for unfetched commits and uncommitted changes first, which are lost
      --no-export           don't export the agents' history first
      --no-worktree-check   with --clean-home, don't check the agents' worktrees in the VM for uncommitted changes first, which are lost
      --parent              use the VM of the nearest folder this one is inside, even across a git repo of its own
      --repos LIST          the repos a --clone VM of a folder of repos gets: all, none or a LIST of their paths, such as frontend,backend (default: a picker in a terminal)
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
- [devvm clone add](devvm_clone_add.md): Copy one more repo into this folder's --clone VM
- [devvm clone fetch](devvm_clone_fetch.md): Fetch the work of this folder's --clone VM into each of its repos
