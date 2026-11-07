# Changelog

## 0.1.1-rc.1

A release candidate for 0.1.1, for trying the changes below before the release. Install it with `devvm upgrade --to 0.1.1-rc.1`, or with the install script's `--version 0.1.1-rc.1`. Homebrew stays on 0.1.0.

- **A dotnet toolchain:** the .NET SDK is a shared toolchain, chosen by a folder's `*.sln`, `*.slnx`, `*.csproj`, `*.fsproj` or `global.json`, and a `global.json`'s `sdk.version` picks the SDK. devvm downloads Microsoft's archive for each platform and checks its SHA-512, global tools and workloads go to the VM's home folder, and the .NET CLI's telemetry is off ([toolchains](https://next.devvm.dev/docs/using/toolchains/)).
- **A network that inspects TLS:** devvm's own mise, which looks up the shared tools' versions on this machine, now trusts the CAs in `~/.config/devvm/ca` as well as the system's, so resolving works behind a proxy that re-signs HTTPS. When a certificate isn't trusted, devvm says so and points to the docs instead of saying to check the connection. On macOS, the CA also has to be trusted in the Keychain.
- **Release candidates:** devvm can ship prereleases, such as this one. `devvm upgrade --to` installs one, and devvm upgrade, the release check and the install script's default never pick one on their own.
- **Lima 2.2 or newer:** devvm needs Lima 2.2 at least, and it's tested with Lima 2.3. `devvm doctor` says when Lima is older.
- **Updated pins:** base's docker is 29.9.3, mise 2026.11.3, rustup 1.29.3, and tealdeer 1.9.1, with a newer oh-my-bash. The shared tools' backends are named, so mise's registry can't switch opencode to a source with no downloads. base installs ICU, which .NET needs, so base and every image built on it are built again once.

## 0.1.0

Released 2026-11-06.

The first release of devvm, which gives each project folder an isolated VM of its own, ready for you or a coding agent to work in. It runs on macOS 13 or newer and on Linux, on arm64 and x86_64, with Lima 2.0 or newer. It's tested on Apple silicon Macs and on Linux on both arm64 and x86_64, but not yet on Intel Macs.

- **A VM for each project folder:** running `devvm` in a folder makes its VM, or opens the one it has, and gives you a shell in it at the same path as on your machine. The VM mounts only that folder read-write, and with `--clone` it gets a clone of the repo of its own instead, whose work you fetch with `git fetch devvm`.
- **Ready in seconds:** every VM is a copy of a base image that devvm builds once, and the toolchains and coding agents are shared tools that VMs mount read-only. devvm detects node, python, rust and go from the project's files, and the built-in `docker`, `nix` and `desktop` layers, your own layers from `devvm layer new`, a project's `devcontainer.json` and `--from` with a container image change what a VM starts from.
- **Coding agents in the sandbox:** Claude Code, Codex and opencode run in the VM with your settings, skills and MCP servers. devvm keeps their settings and logins in a store of its own on your machine, `devvm agent import` copies them in from an agent you already use, `devvm agent login` makes an agent's login from the VM, and `devvm agent save` brings a VM's settings back to the store after you review the diff.
- **Network tiers and secrets:** every VM goes through a proxy that devvm runs for it, and its tier, which is open, internet, allowlist or none, decides what the proxy lets it reach. New VMs get the allowlist tier. On every tier, your secrets, such as your GitHub token and Claude Code token, stay on your machine, because the proxy puts them into the VM's requests.
- **`.git` protection:** on macOS, devvm locks a project's git hooks and config with a flag that even root in the VM can't clear, so code in the VM can't make your next git command on this machine run something.
- **Reaching VMs:** each running VM can be reached by name from ssh and from editors such as VS Code (`devvm code`), with your SSH agent only when you allow it. A VM forwards the ports you ask for, `devvm link` lets one VM reach a service that another VM publishes, and `devvm desktop` opens a VM's desktop in Screen Sharing on macOS, or serves it to a VNC viewer of your own on Linux.
- **Managing VMs and space:** `devvm ls`, `devvm stop`, `devvm restart`, `devvm update` and `devvm rm` manage VMs, and `devvm images`, `devvm gc`, `devvm space` and `devvm retention` show and clean up the images, shared tools and exports devvm keeps. devvm never deletes a project VM on its own, because it may hold unsaved work.
- **Installing and upgrading:** install devvm with Homebrew (`brew trust --formula devvmhq/tap/devvm`, then `brew install devvmhq/tap/devvm`) or with the install script, which checks the release's minisign signature and checksum and asks before it installs what devvm needs, such as Lima. `devvm doctor` checks this machine, and `devvm upgrade` installs a newer release once it has checked the signature.
- **For scripts:** many commands take `--json`, and `--no-input` makes devvm fail where it would ask, naming the flag that answers the question.
