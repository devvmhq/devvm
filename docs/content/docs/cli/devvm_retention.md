---
title: devvm retention
weight: 45
explainer: "Show or change what devvm deletes on its own"
---

Show or change what devvm deletes on its own

## Synopsis

Show the retention settings, which say what devvm deletes on its own, or change them with KEY=VALUE, and KEY=off turns one off. The keys are logs.runs, history.age, history.count, tools.unused-age, orphans.age and nix-cache.size, and every setting is off until you set it. devvm applies them at most once a day, except orphans.age, which only devvm gc --retention applies, and they never delete a VM, an image or a shared tool any VM has.

```text
devvm retention [KEY=VALUE...] [flags]
```

## Examples

```text
  devvm retention                                       # show the settings
  devvm retention history.age=90d tools.unused-age=30d  # delete exports and unused tools once that old
  devvm retention history.age=off                       # turn a setting off
  devvm gc --retention                                  # apply the settings now
```

## Options

```text
  -h, --help   help for retention
      --json   print it as one JSON object, for scripts
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm retention](../using/settings.md#retention) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
