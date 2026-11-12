---
title: Compared to other tools
explainer: How devvm differs from other sandboxes for coding agents
weight: 50
---

devvm gives each project folder a VM of its own on your machine, with the project's toolchains shared read-only between VMs, a proxy on this machine that confines the VM's network, and your secrets kept outside the VM. Other tools sandbox coding agents in other ways: in a microVM, in an OS sandbox around the agent's process, or on a vendor's servers. This page sets devvm beside the ones you're most likely to weigh it against.

> [!NOTE]
> Last checked on 2026-11-08, against each tool's own docs, which the [sources](#sources) list. These tools change every week, so check a tool's own docs before you rely on a row here, and [open an issue](https://github.com/devvmhq/devvm/issues) when a row is out of date.

## Boundary and where it runs

| | Boundary | Runs on | Project files | Open source | Account and cost |
| --- | --- | --- | --- | --- | --- |
| devvm | a full VM with its own kernel, on Lima | macOS 13 or newer and Linux, on arm64 or x86_64 | mounted read-write at the same path, or with `--clone`, the VM's own clone of the repo as `git status` shows it here | yes, MIT | no account, free |
| [Docker Sandboxes](docker-sandboxes.md) | a microVM with its own kernel and Docker Engine | macOS 14 or newer on Apple silicon, Windows 11, Ubuntu 24.04 or newer, and Docker's cloud | mounted read-write, a private clone, or none | no | a Docker sign-in; free locally, cloud paid by use |
| NVIDIA OpenShell | containers on Kubernetes, Podman or Docker with Landlock and seccomp, or a microVM you choose | Linux, macOS on Apple silicon through Docker Desktop, and Windows through WSL2 as an experiment | copied in and out | yes, Apache-2.0 | no account, free |
| microsandbox | a microVM with its own kernel, running as root inside | macOS on Apple silicon, Linux and Windows | mounted read-write by default | yes, Apache-2.0 | no account, free locally |
| smolvm | a microVM with its own kernel | macOS on Apple silicon, Linux and Windows, and its own cloud | mounted | yes, Apache-2.0 | no account locally; cloud plans from $0 a month plus use |
| Claude Code's sandbox | an OS sandbox around Claude Code's shell commands, not its file tools, hooks or MCP servers | macOS, Linux and WSL2 | the folder itself | the sandbox yes, Apache-2.0; Claude Code no | an Anthropic account |
| Codex CLI | an OS sandbox around Codex's commands | macOS, Linux, WSL2 and Windows | the folder itself, writable in workspace-write mode | yes, Apache-2.0 | a ChatGPT account or an API key |
| nono | an OS sandbox, no VM | macOS, Linux and WSL2 | the folder itself | yes, Apache-2.0 | no account, free |
| GitHub Codespaces | a VM for each codespace, on GitHub's servers | the browser or your editor | the repo cloned into the codespace | no | a GitHub account; a monthly quota free, then paid by use |

## Network and credentials

| | Network by default | Rules by | API keys | Agent logins | Agents |
| --- | --- | --- | --- | --- | --- |
| devvm | the allowlist tier: devvm's built-in hosts, your secrets' hosts and yours, enforced outside the VM, or the internet tier while the VM learns the hosts it needs | host and port, and HTTP method and path | placeholders in the VM, real values added by the proxy on this machine, for agents' providers and for any API key you save as your own secret | made through the proxy or imported, kept on this machine and refreshed there for Claude Code, Codex and opencode, with placeholders in the VM | Claude Code, Codex, opencode, pi and Cursor's CLI |
| Docker Sandboxes | a preset you choose at first run: open, balanced or locked down | host and port, and HTTP method, path and query | placeholders in the VM, real values added by a proxy on the host | kept on the host for Claude Code, Codex, Cursor and Droid, with placeholders in the VM; you log in to Claude Code inside the sandbox, and one login serves every sandbox | Claude Code, Codex, Copilot, Cursor, Devin, Docker Agent, Droid, Gemini, Kiro and OpenCode |
| NVIDIA OpenShell | deny for hosts no rule names; method and path rules only log by default | host, port and the calling program, and method and path if you turn them on | placeholders from providers | not documented; providers take API keys from your environment | Claude Code, OpenCode, Codex and GitHub Copilot CLI, in your own image |
| microsandbox | public internet open, private addresses refused | domain, address and port | placeholders, over TLS that the host intercepts | its Claude Code guide logs in inside the VM | any, with a guide for Claude Code |
| smolvm | off | host and address range | placeholders in headers, and in form bodies if you turn it on | not documented | any |
| Claude Code's sandbox | an allowlist that starts empty | domain | masked, if you turn it on | runs on your machine | Claude Code |
| Codex CLI | off in workspace-write mode | domain, with its optional proxy | not documented | runs on your machine | Codex |
| nono | open | domain and path, and method and path for a credential's service | placeholders for each service you name | runs on your machine | Claude Code, Codex, Copilot, OpenCode and others |
| GitHub Codespaces | open | none | environment variables in the codespace | in the codespace | any |

## How devvm differs

Beyond the tables, these are the differences you're most likely to weigh:

- **Shared toolchains.** Of the tools on this page, only devvm shares toolchains read-only between VMs, so a second project that needs node uses the same copy as the first, and nothing in a VM can change it. devvm detects the toolchains from the project's files and reads a project's `devcontainer.json`.
- **`.git` protection.** devvm locks the hooks and config of a folder's `.git` against the VM, and those of each repo nested up to four folders below it, so the VM can't add a hook that runs on this machine, which Docker Sandboxes' docs say its direct mode doesn't prevent. On Linux the locks need no root, and where the kernel doesn't allow them, such as on Ubuntu 23.10 and newer, devvm watches `.git` instead and names what changed ([.git protection](../safety/git-protection.md)).
- **A clone of your repo as it is.** With `--clone`, the VM works on a clone of its own instead of your folder, and the clone gets your local branches, tags, uncommitted changes, stash, the repo's settings and hooks, its submodules and its Git LFS files. `devvm clone fetch` brings the VM's work back as branches, and `devvm rm` checks for work you haven't fetched before it deletes the VM ([working on a clone](../using/clone.md)).
- **Confinement from outside the VM.** devvm enforces the network policy from outside the VM, so it holds even against root in the VM. Transparent mode also sends tools that ignore the proxy settings, such as static Go binaries and containers, to the VM's proxy, on every tier but open.
- **Learning a VM's hosts.** A VM can [learn the hosts it needs](../safety/network.md#learning-the-hosts-a-vm-needs) on the internet tier, and `devvm network lock` then keeps the ones you choose, much as OpenShell shows you the hosts it blocked so that you can approve them.
- **Your own secrets.** Any API key that a tool sends in a header can be a secret of your own, so the VM gets a placeholder, and its proxy puts the real value only into requests to the hosts, methods and paths you give it ([your own secrets](../safety/network.md#your-own-secrets)).
- **Agents' logins on this machine.** devvm keeps each agent's login on this machine, so a confined VM never holds it, and Docker Sandboxes does the same for Claude Code, Codex, Cursor and Droid. In both, one login for each agent serves every VM. On devvm's open tier, a program in the VM can go around the proxy and keep a login that it makes there.
- **Images from the clipboard.** `devvm paste` sends an image, a PDF or a file you copied on this machine into the VM, and puts its path in the VM on your clipboard for the agent's prompt. It never reads text from the clipboard ([pasting images](../using/coding-agents.md#pasting-images)).

## What devvm doesn't do

- devvm runs on macOS and Linux, on Lima, and Windows support is coming soon. Docker Sandboxes, microsandbox, smolvm and Codex CLI run on macOS, Linux and Windows.
- Its rules apply to the whole VM, while OpenShell's rules can also name the program that may reach each host.
- It has no snapshots of a VM, which smolvm's checkpoints and Docker Sandboxes' templates give.
- It has no SDK or API for driving VMs from a program, though most commands have `--json` output.
- It checks at most once a day whether a newer release is out, which `release-check = false` turns off, and it sends nothing else.

[Known limits](../using/limits.md) and [Attack vectors](../safety/attack-vectors.md) list the rest.

## Other tools

- **Clawk** is closer to devvm in shape, with a VM for each folder on your machine. It denies outbound traffic by default, but it forwards your SSH agent, and its tip for Claude Code signs every sandbox in with your Claude Code token.
- **agent-vm** also gives each folder a VM on your machine, and it runs Lima as devvm does. It makes every `.git` read-only for the VM from your machine and asks you before the VM writes files such as `.envrc` and `.vscode/tasks.json`, but the VM reaches the whole internet unless you add domain rules.
- **Lima's own recipe** for coding agents mounts only the project folder, or syncs it and asks before the VM's changes reach your machine, and it leaves the network and API keys to you.
- **Dev Containers** run in a container, which shares this machine's kernel or Docker's VM, and set no network policy. Claude Code's reference container adds a firewall allowlist, and its docs warn that it doesn't stop a malicious project from sending out what's in the container, Claude Code's login included.
- **Hosted sandboxes,** such as E2B, Vercel Sandbox, Fly.io Sprites and exe.dev, run on the vendor's servers, so your code and keys go there, and each needs an account.

## Sources

Each tool's own pages, as of 2026-11-08:

- Docker Sandboxes: [overview](https://docs.docker.com/ai/sandboxes/), [isolation](https://docs.docker.com/ai/sandboxes/security/isolation/), [security](https://docs.docker.com/ai/sandboxes/security/), [credentials](https://docs.docker.com/ai/sandboxes/configuration/credentials/), [agents](https://docs.docker.com/ai/sandboxes/agents/), [Claude Code](https://docs.docker.com/ai/sandboxes/agents/claude-code/), [install](https://docs.docker.com/ai/sandboxes/install/), [FAQ](https://docs.docker.com/ai/sandboxes/faq/), [network rules](https://docs.docker.com/ai/governance/local/network-rules/), [releases](https://github.com/docker/sbx-releases)
- NVIDIA OpenShell: [overview](https://docs.nvidia.com/openshell/about/overview), [architecture](https://docs.nvidia.com/openshell/about/architecture), [support matrix](https://docs.nvidia.com/openshell/about/support-matrix), [sandboxes](https://docs.nvidia.com/openshell/how-it-works/sandboxes/overview), [providers](https://docs.nvidia.com/openshell/how-it-works/providers/overview), [best practices](https://docs.nvidia.com/openshell/latest/security/best-practices), [repo](https://github.com/NVIDIA/OpenShell)
- microsandbox: [security](https://docs.microsandbox.dev/security/overview), [secrets](https://docs.microsandbox.dev/security/secrets), [networking](https://docs.microsandbox.dev/networking/overview), [volumes](https://docs.microsandbox.dev/sandboxes/volumes), [Claude Code guide](https://docs.microsandbox.dev/how-to/agents/claude-code), [repo](https://github.com/superradcompany/microsandbox)
- smolvm: [repo](https://github.com/smol-machines/smolvm), [security model](https://github.com/smol-machines/smolvm/blob/main/docs/security-model.md), [credentials](https://github.com/smol-machines/smolvm/blob/main/docs/credential-substitution.md), [pricing](https://smolmachines.com/pricing)
- Claude Code's sandbox: [sandboxing](https://code.claude.com/docs/en/sandboxing), [sandbox-runtime](https://github.com/anthropics/sandbox-runtime)
- Codex CLI: [sandboxing](https://learn.chatgpt.com/docs/sandboxing), [approvals and security](https://learn.chatgpt.com/docs/agent-approvals-security), [repo](https://github.com/openai/codex)
- nono: [networking](https://nono.sh/docs/cli/features/networking), [repo](https://github.com/nolabs-ai/nono)
- GitHub Codespaces: [security](https://docs.github.com/en/codespaces/reference/security-in-github-codespaces), [billing](https://docs.github.com/en/billing/concepts/product-billing/github-codespaces)
- Others: [Clawk](https://github.com/clawkwork/clawk), [agent-vm](https://github.com/sylvinus/agent-vm), [Lima's AI recipe](https://lima-vm.io/docs/examples/ai/), [Claude Code's devcontainer](https://code.claude.com/docs/en/devcontainer), [E2B](https://docs.e2b.dev/network/internet-access), [Vercel Sandbox](https://vercel.com/docs/sandbox), [Fly.io Sprites](https://docs.fly.io/sprites/concepts/networking), [exe.dev](https://exe.dev/docs/what-is-exe)
