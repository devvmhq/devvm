---
title: Docker Sandboxes
explainer: devvm and Docker Sandboxes, side by side
weight: 1
---

Docker Sandboxes, the `sbx` command, is the tool closest to devvm: each sandbox is a VM with its own kernel on your machine, the project folder is mounted read-write, outbound traffic goes through a proxy on your machine, and API keys and agents' logins stay outside the VM. This page sets the two side by side. Last checked on 2026-11-08 against Docker's docs, which each row links to.

## What's the same

- **A VM with its own kernel.** Docker's sandboxes are microVMs with their own Linux kernel and Docker Engine, and the agent runs as a user with sudo ([isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)). devvm's VMs are full Lima VMs, whose user also has sudo.
- **The folder mounted, or a clone.** Both mount the project folder read-write by default, and both have a mode in which the VM works on a clone of its own instead ([isolation](https://docs.docker.com/ai/sandboxes/security/isolation/), [working on a clone](../using/clone.md)).
- **Placeholders for API keys.** In both, the VM holds a placeholder, and a proxy on your machine puts the real value into requests ([credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/), [secrets](../safety/network.md#secrets)).
- **Agents' logins on your machine.** Both keep the logins of the agents they support outside the VM and give the VM placeholders, and in both, one login for each agent serves every VM. Docker Sandboxes does this for Claude Code, Codex, Cursor and Droid ([credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/)), and devvm for Claude Code, Codex and opencode ([coding agents](../using/coding-agents.md#logins-made-in-a-vm)).

## What differs

| | devvm | Docker Sandboxes |
| --- | --- | --- |
| Source and account | open source (MIT), no account | closed source, a Docker sign-in ([install](https://docs.docker.com/ai/sandboxes/install/)) |
| Usage data | none; a release check at most once a day, which you can turn off | usage data, with your Docker username when you're signed in, unless you turn it off ([FAQ](https://docs.docker.com/ai/sandboxes/faq/)) |
| Hosts | macOS 13 or newer and Linux | macOS 14 or newer on Apple silicon, Windows 11 and Ubuntu 24.04 or newer, plus Docker's cloud ([install](https://docs.docker.com/ai/sandboxes/install/)) |
| Network default | the allowlist tier | a preset you choose at first run ([network rules](https://docs.docker.com/ai/governance/local/network-rules/)) |
| Rules | host and port | host, and HTTP method, path and query ([network rules](https://docs.docker.com/ai/governance/local/network-rules/)) |
| `.git` in the folder | hooks and config locked on macOS, and read-only in the VM on Linux where the kernel allows it, or watched where it doesn't ([.git protection](../safety/git-protection.md)) | writable by the agent in direct mode, which the docs warn about, along with CI and editor config ([isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)) |
| Claude Code's login | you log in with `devvm agent login claude`, or in the VM; devvm keeps the login and refreshes it on your machine, and the VM gets placeholders | you log in with `/login` inside the sandbox, and a proxy on the host takes the tokens from the login's token exchange, keeps them in your host's keychain and gives the sandbox placeholders; that one login serves every sandbox ([Claude Code](https://docs.docker.com/ai/sandboxes/agents/claude-code/), [credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/), [one login](https://github.com/docker/sbx-releases/issues/74)) |
| SSH agent | off by default | forwarded by default ([isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)) |
| MCP servers | run in the VM | local stdio servers run on your machine ([security](https://docs.docker.com/ai/sandboxes/security/)) |
| Ports | every port from 1024 up forwarded to `localhost` unless you limit them | none until you publish one ([usage](https://docs.docker.com/ai/sandboxes/usage/)) |
| Toolchains | shared read-only between VMs, detected from the project's files | in each sandbox's own image |
| Agents | Claude Code, Codex, opencode, pi and Cursor's CLI | Claude Code, Codex, Copilot, Cursor, Devin, Docker Agent, Droid, Gemini, Kiro and OpenCode ([agents](https://docs.docker.com/ai/sandboxes/agents/)) |
| Snapshots | none | a sandbox saved as a template ([template save](https://docs.docker.com/reference/cli/sbx/template/save/)) |
| Secrets from other stores | saved with `devvm secret set` | 1Password, AWS Secrets Manager or a command ([credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/)) |
| Secrets on Linux | in files only you can read | in a file only you can read when no Secret Service runs ([credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/)) |

## Choosing between them

Choose Docker Sandboxes if you need Windows, rules by HTTP method and path, one of the agents devvm doesn't support, or a sandbox you can move to Docker's cloud. Choose devvm if you want open source without an account, toolchains shared between projects, or `.git` protected from the VM in the folder you work in.
