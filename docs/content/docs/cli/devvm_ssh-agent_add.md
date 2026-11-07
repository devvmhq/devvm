---
title: devvm ssh-agent add
weight: 56
explainer: "Load a key into devvm's own SSH agent"
---

Load a key into devvm's own SSH agent

## Synopsis

Load a private key into devvm's own SSH agent, starting the agent first, and record it in ~/.local/share/devvm/agent-keys.json, so devvm loads it again when a VM needs it. devvm refuses a key in a folder a VM mounts.

```text
devvm ssh-agent add KEY_FILE [flags]
```

## Examples

```text
  devvm ssh-agent add ~/.ssh/deploy_key                 # load a key for 12 hours at a time
  devvm ssh-agent add ~/.ssh/deploy_key --lifetime 30m  # for 30 minutes at a time
  devvm ssh-agent add ~/.ssh/deploy_key --confirm       # ask before each use of the key
```

## Options

```text
      --confirm             ask before each use of the key, through an ssh-askpass program
  -h, --help                help for add
      --lifetime DURATION   how long the agent keeps the key once loaded, a DURATION such as 8h, or forever (default 12h)
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm ssh-agent add](../using/ssh.md#devvms-own-agent) explains it in full.

## See also

- [devvm ssh-agent](devvm_ssh-agent.md): Show or change SSH agent forwarding for this folder's VM
