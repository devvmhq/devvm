---
title: devvm agent login
weight: 5
explainer: "Log an agent in, in this folder's VM, and save the login in devvm's store"
---

Log an agent in, in this folder's VM, and save the login in devvm's store

## Synopsis

Log AGENT in inside this folder's VM, and keep the login in devvm's store for every VM with the agent, which gets only placeholders. claude logs in to your Claude subscription in a browser on any machine, whose code you paste back, or gets a long-lived Claude Code token with --setup-token. codex and opencode log in to ChatGPT in a browser on this machine, or with a code with --device-code, or save an API key with --api-key, which is pi's only login.

```text
devvm agent login AGENT [flags]
```

## Examples

```text
  devvm agent login claude                                   # log Claude Code in to your Claude subscription
  devvm agent login claude --setup-token                     # get a Claude Code token with claude setup-token instead
  devvm agent login codex                                    # log Codex in to ChatGPT in a browser on this machine
  devvm agent login codex --device-code                      # log in with a code, in a browser on any machine
  devvm agent login opencode --api-key --provider anthropic  # save opencode's Anthropic API key, asking for it
  devvm agent login pi --api-key --provider openai           # save pi's OpenAI API key, asking for it
```

## Options

```text
      --api-key             save an API key instead, asking for it
      --device-code         log in with a code to type into a browser on any machine, so nothing is forwarded
  -h, --help                help for login
      --here                use this folder's own VM, never that of a folder it's inside
      --parent              use the VM of the nearest folder this one is inside, even across a git repo of its own
      --provider PROVIDER   with opencode's or pi's --api-key, the PROVIDER it's for: anthropic, openai or openrouter
      --setup-token         with claude, get a long-lived Claude Code token with claude setup-token instead
      --stdin               read the secret from a line of input, without asking
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm agent login](../using/coding-agents.md#logging-in-with-devvm-agent-login) explains it in full.

## See also

- [devvm agent](devvm_agent.md): Fill devvm's store of the agents' settings and logins that every VM gets
