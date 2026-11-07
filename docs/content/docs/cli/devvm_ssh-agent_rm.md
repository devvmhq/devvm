---
title: devvm ssh-agent rm
weight: 57
explainer: "Remove a key from devvm's own SSH agent"
---

Remove a key from devvm's own SSH agent

## Synopsis

Remove a key added with devvm ssh-agent add from devvm's own SSH agent, and from ~/.local/share/devvm/agent-keys.json, so devvm doesn't load it again. The key file stays as it is, and it needn't still exist.

```text
devvm ssh-agent rm KEY_FILE [flags]
```

## Examples

```text
  devvm ssh-agent rm ~/.ssh/deploy_key  # take the key out of devvm's own agent
```

## Options

```text
  -h, --help   help for rm
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm ssh-agent rm](../using/ssh.md#devvms-own-agent) explains it in full.

## See also

- [devvm ssh-agent](devvm_ssh-agent.md): Show or change SSH agent forwarding for this folder's VM
