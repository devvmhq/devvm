---
title: devvm history export
weight: 19
explainer: "Save this folder's VM's agent history to a .tar.gz"
---

Save this folder's VM's agent history to a .tar.gz

## Synopsis

Save the history of this folder's VM's agents, their transcripts and sessions without their logins, to FILE, a .tar.gz, or to a new file in ~/.local/share/devvm/exports. devvm starts a stopped VM for it.

```text
devvm history export [FILE] [flags]
```

## Examples

```text
  devvm history export                  # save it in ~/.local/share/devvm/exports
  devvm history export ~/claude.tar.gz  # save it to a file of your own
```

## Options

```text
  -h, --help     help for export
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm history export](../using/coding-agents.md#history) explains it in full.

## See also

- [devvm history](devvm_history.md): List the saved exports of agents' history
