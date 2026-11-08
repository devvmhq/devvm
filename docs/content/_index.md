---
title: devvm
# The social card's title (layouts/_partials/og-image.html): the hero's headline, as the logo says devvm.
cardTitle: One VM for every project
explainer: An isolated sandbox for you or a coding agent
layout: hextra-home
# Mostly shortcodes: no Copy page menu (hugo.yaml gives the home page no Markdown copy).
contextMenu: false
---

<!-- markdownlint-disable-next-line MD034 -- a shortcode's parameter, not a bare URL -->
{{< hextra/hero-badge link="https://github.com/devvmhq/devvm" >}}
  Free and open source
{{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}}
  One VM for every project
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  An isolated sandbox with your project's toolchain, ready for you or a coding agent to start coding.
{{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Install" link="docs/getting-started/install/" icon="download" >}}
{{< hextra/hero-button text="Read the docs" link="docs/" icon="open-book" >}}

{{< art "sandbox" >}}

## Quick start {icon="rocket"}

{{< install >}}

{{< features columns="3" >}}

{{< feature title=`1\. Go to your project` icon="file-folder" level="3" art="step-cd" >}}

Open a terminal and `cd` into any project folder.

{{< /feature >}}

{{< feature title=`2\. Run devvm` icon="prompt" level="3" art="step-pick" >}}

devvm shows what the new VM gets. Confirm to make it, or customize it.

{{< /feature >}}

{{< feature title=`3\. Start your agent` icon="robot" level="3" art="step-agent" >}}

devvm opens a shell in the VM, whose prompt starts with `(devvm)`, so start your agent there.

{{< /feature >}}

{{< /features >}}

Claude Code needs your Claude login before its first start, which `devvm agent login claude` makes from the VM and keeps on your machine ([your first VM](docs/getting-started/first-vm.md#claude-codes-login)). The next time you run devvm in the folder, it opens the same VM, and `devvm tools` adds an agent to a VM you already have.

## What's supported {icon="view-grid"}

{{< features columns="3" >}}

{{< feature title="Toolchains" icon="code" level="3" art="toolchains" >}}

`devvm` has toolchains for node, python, rust, go and dotnet. It chooses the ones your project's files call for, and you can choose more when you customize a new VM or name them with `--tools` ([toolchains](docs/using/toolchains.md)).

{{< /feature >}}

{{< feature title="Coding agents" icon="robot" level="3" art="coding-agents" >}}

The [supported coding agents](docs/using/coding-agents.md#supported-agents) run in the VM, Claude Code among them. Choose one when you customize a new VM, or choose them for every new VM in `devvm setup` ([coding agents](docs/using/coding-agents.md)).

{{< /feature >}}

{{< feature title="Layers" icon="copies" level="3" art="layers" >}}

devvm ships the `docker`, `nix` and `desktop` layers, which you choose when you customize a new VM, and `devvm layer new` creates your own custom layers you can apply to any of your VMs ([layers](docs/using/images.md#layers)).

{{< /feature >}}

{{< /features >}}

## Why use devvm? {icon="shield"}

{{< beside art="blast-radius" >}}

- Developers face attacks every day, such as [supply chain attacks](https://en.wikipedia.org/wiki/Supply_chain_attack), leaked secrets, [prompt injections](https://en.wikipedia.org/wiki/Prompt_injection) and rogue agents.
- A sandbox limits the [blast radius](https://en.wikipedia.org/wiki/Blast_radius#Uses_in_software) of an attack to what's inside it.
- You know you _should_ run your code and agents in a sandbox, but most tools need configuration and setup that's hard to get right.
- `devvm` ships with sensible defaults so you can start coding in a sandbox with a single command.

{{< /beside >}}

{{< features >}}

{{< feature title="Developer ready, zero config" icon="prompt" art="zero-config" >}}

- The base image comes with [optional extras](docs/using/customizing.md#base-image-features) that you choose, such as oh-my-bash, fzf, zoxide (`z`), tree and tldr.
- Every VM gets your git name and email, so the commits made in it are authored by you or your agent.
- Ports forward on their own, so a dev server on `:5173` in the VM opens at `localhost:5173` on your machine ([ports](docs/using/ports-size.md#ports)).
- `devvm code` opens the VM in VS Code over Remote-SSH ([devvm code](docs/using/ssh.md#devvm-host-and-devvm-code)).

{{< /feature >}}

{{< feature title="Ready in seconds" icon="high-voltage" art="copies" >}}

- The first VM builds a base image, and every later VM is a copy of it that starts in seconds ([images](docs/using/images.md)).
- `devvm` builds each version of a toolchain once and shares it read-only, so all VMs use the same copy ([shared tools](docs/using/images.md#shared-tools)).

{{< /feature >}}

{{< feature title="Isolated sandbox" icon="locked" art="isolated" >}}

- The VM mounts your project folder at the same path. Apart from your agents' settings from devvm's own store, which it gets read-only, it sees nothing else in your home folder ([what a VM can reach](docs/safety/_index.md#what-a-vm-can-reach)).
- New VMs get the allowlist [network tier](docs/safety/network.md) by default, so they reach only an allowlist of hosts, through a proxy on your machine. You can choose a looser tier, internet or open, or a stricter one, none.
- On macOS, `devvm` locks `.git`'s hooks and config, so the VM can't change what git runs on your machine ([.git protection](docs/safety/git-protection.md)).
- With [`--clone`](docs/using/clone.md), the VM works on a clone of the repo of its own instead of your folder, so nothing in it can touch your working tree or `.git`, and its work reaches your repo only when you fetch it.
- No sandbox closes every gap, so [attack vectors](docs/safety/attack-vectors.md#narrowed-not-closed) lists what's left open, and [staying safe](docs/safety/_index.md#the-habits) lists the habits that close most of it.

{{< /feature >}}

{{< feature title="Made for coding agents" icon="robot" art="agents" >}}

- The agent runs in the VM, so an attack on it stays in the sandbox. It gets your settings, skills and MCP servers but never your history ([coding agents](docs/using/coding-agents.md)).
- On every network tier, their secrets, such as your Claude Code token and your Codex login, stay on your machine, because the VM gets placeholders and devvm's proxy puts the real ones into its requests on the way out.

{{< /feature >}}

{{< feature title="Make it yours" icon="wrench" column="right" art="make-it-yours" >}}

- Your own layers are setup scripts that `devvm` builds into an image: `devvm layer new mailpit` starts one, and `devvm --layers mailpit` makes a VM with it ([your own layers](docs/using/customizing.md#your-own-layers)).
- `devvm setup --layers` gives every new VM the layers you choose, so your shell setup, aliases or dotfiles can go in a layer of your own that each VM gets unless you leave it out ([extras of your own](docs/using/customizing.md#extras-of-your-own)).
- `--from` starts the base image from a container image instead of Ubuntu's, such as `node:24` or your team's own ([starting from a container image](docs/using/from.md)).
- When the project has a `devcontainer.json`, `devvm` sets up its VM from the file's container image, ports, environment and commands ([devcontainer.json](docs/using/devcontainer.md)).

{{< /feature >}}

{{< /features >}}

## FAQ {icon="speech-balloon-question"}

### What does it run on?

`devvm` runs on macOS and on Linux, on arm64 or x86_64. It needs [Lima](https://lima-vm.io/), which runs the VMs with Apple's Virtualization framework on macOS and QEMU on Linux ([backends](docs/backends/_index.md)). Homebrew installs Lima along with devvm, and on Linux, Lima also needs QEMU and access to `/dev/kvm`. [Installing](docs/getting-started/install.md) lists every requirement, and [known limits](docs/using/limits.md#linux-hosts) lists what Linux hosts lack, such as `.git` protection.

### Does the agent run on my machine?

No. You start the agent in the VM's shell, so it runs in the VM, and you never install it on your machine. There it sees only [what the VM can reach](docs/safety/_index.md#what-a-vm-can-reach).

### Is it free?

Yes. `devvm` is open source under the [MIT license](https://github.com/devvmhq/devvm/blob/main/LICENSE).

## Start coding in a sandbox {icon="prompt"}

Install `devvm`, then run `devvm` in any project folder.

{{< hextra/hero-button text="Install" link="docs/getting-started/install/" icon="download" >}}
{{< hextra/hero-button text="Read the docs" link="docs/" icon="open-book" >}}
