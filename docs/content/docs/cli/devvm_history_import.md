---
title: devvm history import
weight: 20
explainer: "Put an export of agent history into this folder's VM"
---

Put an export of agent history into this folder's VM

## Synopsis

Put the agent history in FILE, a .tar.gz that devvm history export or devvm rm saved, into this folder's VM, or the newest export devvm saved for this folder. It leaves out the agents' logins and the files devvm copies in, and keeps every file the VM already has.

```text
devvm history import [FILE] [flags]
```

## Examples

```text
  devvm history import                  # import the newest export saved for this folder
  devvm history import ~/claude.tar.gz  # import a file of your own
```

## Options

```text
  -h, --help     help for import
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm history import](../using/coding-agents.md#importing-history) explains it in full.

## See also

- [devvm history](devvm_history.md): List the saved exports of agents' history
