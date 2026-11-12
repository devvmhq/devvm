---
title: devvm paste
explainer: "Send the clipboard's image, PDF or copied file to this folder's VM"
---

Send the clipboard's image, PDF or copied file to this folder's VM

## Synopsis

Put the image, the PDF, or the one copied file, that's on this machine's clipboard into this folder's VM, which must be running, as a new file in its paste folder, and copy that file's path to the clipboard. A copied file is sent after asking, showing its path on this machine, and only a file in this folder, which the VM already reads, is sent without asking, with --send-file. Then paste the path into the agent with your terminal's paste shortcut.

```text
devvm paste [flags]
```

## Examples

```text
  devvm paste              # send the copied image, and copy its path in the VM
  devvm paste --json       # the same, printing the path and what was sent as JSON
  devvm paste --send-file  # send a copied file in this folder without asking, as a script must
  devvm paste rm           # delete the files devvm paste left in the VM
```

## Options

```text
  -h, --help        help for paste
      --here        use this folder's own VM, never that of a folder it's inside
      --json        print it as one JSON object, for scripts
      --parent      use the VM of the nearest folder this one is inside, even across a git repo of its own
      --send-file   send a copied file in this folder without asking
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm paste](../using/coding-agents.md#pasting-images) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm paste rm](devvm_paste_rm.md): Delete the files devvm paste left in this folder's VM
