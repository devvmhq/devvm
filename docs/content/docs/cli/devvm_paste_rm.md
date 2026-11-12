---
title: devvm paste rm
explainer: "Delete the files devvm paste left in this folder's VM"
---

Delete the files devvm paste left in this folder's VM

## Synopsis

Delete the images and files that devvm paste put in this folder's VM's paste folder, after asking, and nothing else in that folder. The VM must be running.

```text
devvm paste rm [flags]
```

## Examples

```text
  devvm paste rm          # delete them after asking
  devvm paste rm --force  # delete them without asking, as a script must
```

## Options

```text
  -f, --force    don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help     help for rm
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm paste rm](../using/coding-agents.md#pasting-images) explains it in full.

## See also

- [devvm paste](devvm_paste.md): Send the clipboard's image, PDF or copied file to this folder's VM
