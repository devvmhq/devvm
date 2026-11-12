---
title: devvm clone add
explainer: "Copy one more repo into this folder's --clone VM"
---

Copy one more repo into this folder's --clone VM

## Synopsis

Copy the repo at PATH, a folder in this folder's --clone VM's folder, into the VM at the same path, with its branches, tags and remote-tracking branches, checked out as it is here, starting the VM if it's stopped. The repo gets a remote for the VM, as each of the VM's repos has.

```text
devvm clone add PATH [flags]
```

## Examples

```text
  devvm clone add tools      # copy the repo in ./tools into the VM
  devvm clone add ../shared  # copy a repo beside this one, in the same folder of repos
```

## Options

```text
  -h, --help     help for add
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm clone add](../using/clone.md#adding-a-repo) explains it in full.

## See also

- [devvm clone](devvm_clone.md): Show or change whether this folder's VM has its own clone of the repo
