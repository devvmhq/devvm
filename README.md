<!-- markdownlint-disable MD033 MD041 -- the logo, which follows GitHub's dark or light theme -->
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/static/branding/devvm-logo-on-dark.svg">
  <img src="docs/static/branding/devvm-logo-on-light.svg" alt="devvm" width="250">
</picture>
<!-- markdownlint-enable MD033 MD041 -->

# One VM for every project

An isolated sandbox with your project's toolchain, ready for you or a coding agent to start coding.

[Install](https://devvm.dev/docs/getting-started/install/) · [Read the docs](https://devvm.dev/docs/) · [Staying safe](https://devvm.dev/docs/safety/)

## Install

On macOS, install devvm with Homebrew, which also installs Lima:

```sh
brew trust --formula devvmhq/tap/devvm
brew install devvmhq/tap/devvm
```

On Linux, install it with the install script, which also works on macOS and asks before it installs Lima, QEMU or access to `/dev/kvm`:

```sh
curl -fsSL https://github.com/devvmhq/devvm/releases/latest/download/install.sh | bash
```

Then run `devvm doctor`, which checks what devvm needs on your machine and says how to get anything that's missing.

Each release is signed with devvm's minisign key, `RWSjzFsn7ETaijKJ2y7F4DtZobXmyv+LmdclJwZP/KmAkLUwOCzWF/2b`, and [Checking the install script](https://devvm.dev/docs/internals/install/#checking-the-install-script) shows how to check the script with it before you run it.

- **Requirements:** macOS 13 or newer, or Linux, on arm64 or x86_64, with Lima 2.2 or newer ([Lima's requirements](https://devvm.dev/docs/backends/lima/#requirements)).
- **Tested on** Apple silicon Macs and on Linux on arm64 and x86_64, but not yet on Intel Macs.
- [Installing](https://devvm.dev/docs/getting-started/install/) has the full list of requirements and the install script's options, and [Upgrading and uninstalling](https://devvm.dev/docs/using/upgrading/) covers what comes later.

## Quick start

```sh
cd ~/some-project
devvm
```

1. **Go to your project.** Open a terminal and `cd` into any project folder.
2. **Run devvm.** It shows what this folder's new VM gets, with the toolchains your project's files call for. Confirm to make the VM, or customize it to choose its toolchains, coding agents, layers, network tier and size.
3. **Start your agent.** devvm opens a shell in the VM, at the same path as on your machine, and its prompt starts with `(devvm)`, so run `claude` there.

- Running `devvm` in the folder again opens the same VM.
- [Your first VM](https://devvm.dev/docs/getting-started/first-vm/) has the rest.

> [!NOTE]
> The first run starts with a few questions that set devvm's defaults for every new VM and its base image: the size and coding agents new VMs get, and the base image's optional extras. It builds the base image once, in a few minutes. Later VMs start in seconds.

<!-- two separate notices -->

> [!TIP]
> Claude Code needs your Claude login before its first start, which `devvm agent login claude` makes from the VM and keeps on your machine ([Claude Code's login](https://devvm.dev/docs/getting-started/first-vm/#claude-codes-login)).

## What's supported

- **Toolchains:** node, python, rust, go and dotnet. devvm detects them from the project's files, so a folder with `package.json` gets node ([toolchains](https://devvm.dev/docs/using/toolchains/)).
- **Coding agents:** Claude Code, Codex, opencode and pi run in the VM, with your settings, skills and MCP servers. devvm keeps their settings and logins in a store of its own, and `devvm agent import` copies them in from an agent you already use on your machine ([coding agents](https://devvm.dev/docs/using/coding-agents/)).
- **Layers:** the built-in `docker`, `nix` and `desktop` layers, and your own from `devvm layer new` ([layers](https://devvm.dev/docs/using/images/#layers)).
- **Backend:** devvm runs its VMs on [Lima](https://lima-vm.io) ([backends](https://devvm.dev/docs/backends/)).

## Why use devvm?

- **Isolated:** each project folder gets a VM of its own, which mounts only the project folder, at the same path as on your machine ([what a VM can reach](https://devvm.dev/docs/safety/#what-a-vm-can-reach)).
- **Network tiers:** new VMs get the allowlist tier by default, so they reach only the hosts you allow ([network tiers](https://devvm.dev/docs/safety/network/)).
- **Secrets stay home:** on every tier, your secrets, such as your GitHub token and Claude Code token, stay on your machine.
- **Agents in the sandbox:** you start the agent in the VM's shell, so you never install it on your machine.
- **Ready in seconds:** every VM is a copy of a base image, and toolchains are shared read-only between VMs ([images](https://devvm.dev/docs/using/images/)).
- **No sandbox closes every gap:** a few habits close most of what devvm can't close on its own, so read [Staying safe](https://devvm.dev/docs/safety/) before you trust a VM with a project.

## Working on a clone

By default, the VM mounts your project folder, so the VM and your editor share the same files. With `devvm --clone`, the VM gets a clone of the repo of its own, so nothing in it can touch your working tree or `.git`, and its work reaches your repo only when you fetch it:

```sh
devvm --clone                 # make the VM with its own clone, and open a shell in it
git fetch devvm               # later, on this machine, fetch the VM's work as devvm/<branch>
git log -p main..devvm/main   # review it before you merge it
```

[Working on a clone](https://devvm.dev/docs/using/clone/) has the details.

## Docs

The docs are at [devvm.dev](https://devvm.dev/), and their source is in `docs/`.

- [Getting started](https://devvm.dev/docs/getting-started/): installing devvm, your first VM and the commands you use most.
- [Using devvm](https://devvm.dev/docs/using/): images and tools, clones, links between VMs, ports and size, coding agents, SSH and editors, and troubleshooting.
- [Staying safe](https://devvm.dev/docs/safety/): the habits that keep you safe, network tiers, `.git` protection and attack vectors.
- [Backends](https://devvm.dev/docs/backends/): Lima, which runs the VMs, and what it gives them.
- [CLI reference](https://devvm.dev/docs/cli/): every command, with its arguments and flags.
- [How it works](https://devvm.dev/docs/internals/): how devvm does what it does, for checking a claim or reviewing its security.
- [Contributing](https://devvm.dev/docs/contributing/): building, testing and writing docs.

## Security and contributing

- Report a vulnerability as [SECURITY.md](SECURITY.md) says.
- [CONTRIBUTING.md](CONTRIBUTING.md) and the [Contributing](https://devvm.dev/docs/contributing/) pages cover building, testing and writing docs.

## License

[MIT](LICENSE)
