---
title: devvm agent import
explainer: "Copy the agents' settings and logins from this machine into devvm's store"
---

Copy the agents' settings and logins from this machine into devvm's store

## Synopsis

Copy the settings and logins of AGENT, claude, codex, opencode, pi or cursor, or of every agent, from your home folder into devvm's store, never their history. It copies what a link out of an agent's folder leads to only from a folder that config.toml's follow-links names, or after naming the link and asking, which --follow-links answers. It asks before replacing what the store has.

```text
devvm agent import [AGENT...] [flags]
```

## Examples

```text
  devvm agent import                 # copy every agent's settings and logins, asking before replacing the store's
  devvm agent import claude codex    # copy only Claude Code's and Codex's
  devvm agent import --force         # replace the store's without asking, as a script must
  devvm agent import --follow-links  # copy what links out of the agents' folders lead to as well, such as into a dotfiles repo
```

## Options

```text
      --follow-links   copy what links out of the agents' folders lead to, naming each, without asking (default: ask in a terminal)
  -f, --force          don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help           help for import
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm agent import](../using/coding-agents.md#importing-from-this-machine) explains it in full.

## See also

- [devvm agent](devvm_agent.md): Fill devvm's store of the agents' settings and logins that every VM gets
