---
title: devvm agent save
weight: 6
explainer: "Copy the agents' settings from this folder's VM into devvm's store"
---

Copy the agents' settings from this folder's VM into devvm's store

## Synopsis

Copy the settings of AGENT, claude, codex, opencode or pi, or of every agent this folder's VM has, from the VM into devvm's store, so every VM with the agent gets them, never its logins or history. It shows what changed and asks first, so it runs only in a terminal without --no-input, and devvm starts a stopped VM for it.

```text
devvm agent save [AGENT...] [flags]
```

## Examples

```text
  devvm agent save         # show what changed in every agent's settings, then save them
  devvm agent save claude  # save only Claude Code's
```

## Options

```text
  -h, --help     help for save
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm agent save](../using/coding-agents.md#saving-a-vms-settings) explains it in full.

## See also

- [devvm agent](devvm_agent.md): Fill devvm's store of the agents' settings and logins that every VM gets
