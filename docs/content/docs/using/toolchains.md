---
title: Toolchains
explainer: The toolchains VMs get, and the files that choose them
weight: 2
---

devvm has these toolchains:

| Toolchain | What it brings |
| --- | --- |
| [node](#node) | Node 24, with npm, pnpm, yarn and bun |
| [python](#python) | CPython, with pip and uv |
| [rust](#rust) | rustup, cargo and the stable toolchain |
| [go](#go) | Go |
| [dotnet](#dotnet) | the .NET SDK and its runtimes |

A new VM gets the ones its folder's files call for, which the summary of the new VM shows, unless you customize the VM and check others in the list or give others with `--tools`, as in `devvm --tools node,python` ([what a new VM gets](images.md#what-a-new-vm-gets)). Each section below says what the toolchain brings and which files make devvm choose it. A project that gets its toolchain from Nix uses the `nix` layer, which [Nix projects](#nix-projects) covers.

## node

node brings the newest release of Node 24, with npm, pnpm, yarn through corepack, and bun. devvm stays on Node 24 because newer releases drop corepack.

- **Files.** devvm chooses node for a folder with `package.json`, `bun.lock` or `bun.lockb` at its top level.
- **Versions.** A project's `.nvmrc` or `.node-version` picks its Node version, which installs into the VM the first time it's used.
- **Global installs.** `npm i -g` installs into `~/.npm-global` in the VM, which is on the `PATH`.
- **Allowlist.** On the allowlist tier, the VM can reach npm's, yarn's, pnpm's and bun's registries and Node's downloads, which are `registry.npmjs.org`, `registry.yarnpkg.com`, `repo.yarnpkg.com`, `nodejs.org` and `bun.sh`.

## python

python brings a prebuilt build of the newest CPython release, and uv, so `pip` and `uv` work as usual.

- **Files.** devvm chooses python for a folder with `pyproject.toml`, `requirements.txt`, `uv.lock`, `setup.py` or `Pipfile` at its top level.
- **Versions.** A project's `.python-version` picks its Python version, which installs into the VM the first time it's used.
- **Global installs.** The shared Python is read-only, so packages go into a project's virtual environment or into your home in the VM, and `~/.local/bin` is on the `PATH`.
- **Allowlist.** On the allowlist tier, the VM can reach PyPI and the Python downloads that uv uses, which are `pypi.org`, `files.pythonhosted.org` and `releases.astral.sh`.

## rust

rust brings rustup, cargo and the newest stable toolchain, with the `rust-src` and `rust-analyzer` components already added.

- **Files.** devvm chooses rust for a folder with `Cargo.toml` at its top level.
- **Versions.** A project's `rust-toolchain.toml` picks its toolchain, which rustup installs into the VM next to the shared one the first time it's used.
- **Global installs.** `cargo install` installs into `~/.cargo/bin` in the VM, which is on the `PATH`. `rustup component add` can't change the shared toolchain, which is why it comes with `rust-src` and `rust-analyzer`.
- **Allowlist.** On the allowlist tier, the VM can reach crates.io and Rust's toolchain downloads, which are `crates.io`, `index.crates.io`, `static.crates.io` and `static.rust-lang.org`.

## go

go brings the newest Go release, and devvm turns Go's telemetry off in the VM.

- **Files.** devvm chooses go for a folder with `go.mod` at its top level.
- **Versions.** The `toolchain` line in a project's `go.mod` picks its Go version, and Go itself downloads that version into the VM the first time it's used.
- **Global installs.** `go install` installs into `~/go/bin` in the VM, which is on the `PATH`.
- **Allowlist.** On the allowlist tier, the VM can reach Go's module proxy, its checksum database and Go's downloads, which are `proxy.golang.org`, `sum.golang.org`, `go.dev` and `dl.google.com`.

## dotnet

dotnet brings the newest release of the .NET SDK, with the .NET and ASP.NET Core runtimes that come with it, and devvm turns off .NET's telemetry and its first-run banner in the VM.

- **Files.** devvm chooses dotnet for a folder with a solution (`*.sln` or `*.slnx`), a C# or F# project (`*.csproj` or `*.fsproj`) or `global.json` at its top level.
- **Versions.** The `sdk.version` in a project's `global.json` picks its SDK, which installs into the VM the first time it's used. mise installs exactly that version and doesn't follow `rollForward`, so a `global.json` that allows a newer SDK still gets the one it names, and a `global.json` without an SDK version keeps the shared one. mise installs that SDK with Microsoft's install script, which checks only the download's size, while devvm checks the shared SDK against the SHA-512 that Microsoft publishes for it.
- **Global installs.** `dotnet tool install -g` installs into `~/.dotnet/tools` in the VM, which is on the `PATH`, and NuGet keeps its packages in `~/.nuget/packages`. The shared SDK is read-only, so `dotnet workload install` installs workloads into `~/.dotnet` in the VM instead of the SDK's folder.
- **Allowlist.** On the allowlist tier, the VM can reach NuGet, the revocation lists that NuGet checks packages' signatures against, and the SDK downloads that a `global.json` asks for, which are `api.nuget.org`, `crl3.digicert.com` on port 80, `dot.net` and `builds.dotnet.microsoft.com`. Microsoft's telemetry isn't on the allowlist.

## Nix projects

A project whose toolchain comes from Nix gets the [`nix` layer](images.md#nix), which brings Nix, direnv and nix-direnv into its VM. For a folder with `flake.nix`, `shell.nix` or `devenv.nix` at its top level, or with a `default.nix` that makes a shell with `mkShell`, the list devvm shows when you customize a new VM names that file in the `nix` row, and you choose it there. A layer isn't chosen by default, so a VM made as its summary shows, or without a terminal, gets it only with `--layers nix`, as in `devvm --layers nix` in a folder with `package.json` and `flake.nix`, which gives node on the `nix` layer.

- **Dev shells.** Put `use flake` (or `use nix` for a `shell.nix`) in the project's `.envrc` and run `direnv allow` in the VM once. From then on, each shell that changes into the folder loads the dev shell, and leaving the folder unloads it. This works in your terminal and in the terminals of editors connected over SSH.
- **The flake's tools come first.** While the dev shell is loaded, its tools are ahead of the shared tools on the `PATH`, so a flake's `node` wins over the shared node, and the shared tools come back when you leave the folder.
- **Files in the project.** nix-direnv keeps its cache of the dev shell in your home in the VM, so the project gets no `.direnv` folder. Nix itself writes a `flake.lock` next to a `flake.nix` that has none, and in a git repository it marks that file to be added, as it does on any machine.
- **One store per VM.** Each VM has its own Nix store, so a package one VM builds isn't in another's. `nix run nixpkgs#hello` works in any VM with the layer.
- **Shared downloads.** VMs share their downloads from Nix's binary cache, on every [network tier](../safety/network.md#network-tiers), an open VM's included, because every VM's Nix goes through its proxy. The first VM that needs a package gets it from cache.nixos.org through devvm, which keeps a copy on this machine, and the other VMs get that copy, so each package downloads once per machine instead of once per VM. Only devvm adds to this shared cache, and only what cache.nixos.org signed, so no VM can change what another VM gets. When the cache doesn't have a package or can't get it, Nix goes to cache.nixos.org itself. The cache keeps to 10 GiB, deleting the packages used least recently first, unless you set another size with `devvm retention nix-cache.size=20G` ([retention](settings.md#retention)). `devvm space` shows what it takes, and `devvm gc --nix-cache` empties it ([how it works](../internals/network.md#the-shared-nix-cache)).
- **Trust.** Only root is a trusted Nix user, so the VM's user can't add caches or signing keys of their own, and Nix ignores `--option substituters` from them. `sudo nix` works. Nix builds every package in its sandbox, and a build fails rather than run without it.
- **Allowlist.** On the allowlist tier, the VM can reach Nix's binary cache and its channels and flake registry, which are `cache.nixos.org`, `channels.nixos.org` and `releases.nixos.org`, and GitHub, where most flakes' inputs come from. A package that isn't in Nix's binary cache builds from source, which downloads its sources from their own hosts, and those are blocked until you allow them ([Nix through the proxy](../safety/network.md#nix)).

### Sharing a team's packages

When your team's projects use one flake for their dev shell, you can put that dev shell's packages into an image, so every VM made from the image starts with them in its store and downloads none of them. VMs made from one image share its disk, because on macOS each VM's disk is a copy-on-write clone of the image's, and on Linux hosts it's an overlay of the image's, so the packages take their space once. This works with the stable Nix that the `nix` layer installs, on every network tier.

To do it, make a [layer of your own](customizing.md#your-own-layers) that builds the dev shell while the image builds, and give it after `nix`, so that it runs on top of the `nix` layer:

```sh
devvm layer new myteam
$EDITOR ~/.config/devvm/layers/myteam.sh
devvm --layers nix,myteam   # a new VM of the image nix+myteam
```

Put these lines in `~/.config/devvm/layers/myteam.sh`, below the starter's `set -euo pipefail`, with your team's flake at the commit you want:

```bash
# The team's flake at a commit, so the image holds exactly that dev shell.
flake=github:myorg/devshell/0123456789abcdef0123456789abcdef01234567
bin=/nix/var/nix/profiles/default/bin
"$bin/nix" develop "$flake" --profile /tmp/myteam-shell --command true
# A garbage collector root in the VM's home keeps the dev shell's packages in the store.
mkdir -p ~/.cache/myteam
"$bin/nix-store" --add-root ~/.cache/myteam/dev-shell --realise "$(readlink -f /tmp/myteam-shell)"
rm -f /tmp/myteam-shell /tmp/myteam-shell-*-link
```

`nix develop` builds or downloads every package of the flake's dev shell into the image's store and records the dev shell in a profile. `nix-store --add-root` then makes `~/.cache/myteam/dev-shell` a garbage collector root, so `nix-collect-garbage` in a VM never deletes those packages. A project that loads the same dev shell, with `use flake github:myorg/devshell/<commit>` in its `.envrc`, finds every package already in its VM's store, and so does most of a project whose own flake pins the same nixpkgs revision. The image is built without your credentials, because [credentials never go into images](../internals/security.md#read-only-and-confined-builds), so the flake has to be one that anyone can fetch, such as a public repository.

What this doesn't cover:

- **Packages a VM gets after it's made** stay on that VM's own disk. The [shared downloads](#nix-projects) at least save downloading them again.
- **A new version of the flake** needs a new image. Moving `flake` to another commit edits the script, so devvm builds the image again the next time it's used, while VMs made earlier keep the packages they have. When the script names a branch instead of a commit, a changed `flake.lock` upstream doesn't change the script, so run `devvm rebuild myteam` to build the image again.
- **A flake that pins another nixpkgs revision** shares few packages with the image, and its VM downloads the rest.
- **Nix's local overlay store**, which would let VMs share one store as it changes, is left out until Nix makes it stable.

## How toolchains are shared and updated

The toolchains are [shared tools](images.md#shared-tools), like the [coding agents](coding-agents.md#supported-agents), so they aren't part of any image. devvm builds each version once and mounts it read-only into every VM that has it, and [mise](https://mise.jdx.dev) runs it, so `node`, `python`, `cargo`, `go` and `dotnet` work from any shell in the VM, including `bash -c` from agents and from editors over SSH. Because the shared copy is read-only, a toolchain can't update itself, and anything you install globally goes into your home in the VM, as each section above says.

devvm follows each toolchain's newest release, and Node 24 for node. [Which bases they run on](#which-bases-they-run-on) says what each one needs from a VM's base. [Adding and removing tools](#adding-and-removing-tools) explains how `devvm update` and `devvm tools update` move a VM to newer versions, how `devvm tools add` adds a toolchain to an existing VM, and how `devvm tools rm` takes one out. [What a new VM gets](images.md#what-a-new-vm-gets) explains what happens when a folder calls for more than one toolchain.

## Which bases they run on

Every toolchain runs on `base` from Ubuntu's own image, and on an Alpine-based image with [`--from`](from.md), which gets musl builds of the tools. dotnet also needs ICU, the Unicode library that .NET's globalization uses, so devvm installs ICU in `base`, whether it's made from Ubuntu's image or `--from` a container image. On a Debian- or Ubuntu-based image, each tool needs at least the glibc version that its prebuilt release was built against. devvm measures that version from the tool's files whenever it builds a new version, and it refuses a tool whose version is newer than the image's glibc ([shared tools and glibc](from.md#shared-tools-and-glibc)). Today the shared tools need these glibc versions, so Debian 10 and Ubuntu 20.04 and newer images run all of them:

| Shared tool | arm64 | amd64 |
| --- | --- | --- |
| node | 2.28 | 2.28 |
| pnpm and bun, with node | 2.18 and 2.17 | 2.18 and 2.17 |
| python | 2.17 | 2.17 |
| uv, with python | 2.28 | 2.17 |
| rust, with rustup | 2.17 | 2.17 |
| go | none, because its programs need no glibc | none |
| dotnet | 2.27 | 2.27 |
| claude and opencode | 2.17 | 2.17 |
| codex | 2.28, for its voice helper | 2.28 |

## Adding and removing tools

A new VM gets the shared tools chosen when devvm makes it, the toolchains and the [coding agents](coding-agents.md#supported-agents), and keeps those versions. On each launch, devvm warns when there are newer ones and names `devvm update`, which moves the VM to them, and also makes the VM again when its image has changed, after asking ([outdated VMs](images.md#outdated-vms)).

In a terminal, `devvm tools` shows the same list as for a new VM, with this folder's VM's toolchains and agents already chosen and its layers greyed out, because they're built into the VM's image, which keeps them until `devvm layer add` or `devvm layer rm` makes it again. The Layers heading names `devvm layer add` and `devvm layer rm`, which change a VM's layers by making it again ([adding layers to a VM](images.md#adding-layers-to-a-vm)). Choose the tools and confirm, and devvm adds and takes out what changed, as `devvm tools add` and `devvm tools rm` do, then opens a shell in the VM. When a new agent's config folders need the running VM to start again, devvm asks first. When nothing changed, devvm says so and opens nothing.

Without a terminal, or with `--no-input` or `--json`, `devvm tools` shows the tools of this folder's VM in a table, one row per tool, with node's pnpm and bun and python's uv in rows of their own:

| Column | |
| --- | --- |
| `TOOL` | the tool's name, such as `node` or `pnpm` |
| `VERSION` | the version the VM has |
| `NEWER` | the version in devvm's tools lock when the VM has another build, which is usually a newer version, or `-` when the VM has the lock's build |
| `BUILT` | `yes` when that newer version is built, `no` when `devvm tools update` builds it first, or `-` |

The table changes nothing and starts no VM. devvm reads the tools lock as it is, without going online, so a release that came out since devvm last checked isn't in it yet.

### devvm tools --json

`devvm tools --json` prints the same table as one JSON object, as [output for scripts](command-line.md#output-for-scripts) describes. Each of the `tools` has the tool's name and the VM's version. A tool that the lock has another build of also has `newer`, which is the lock's version, and `newer_built`, which says whether that version is built.

```json
{
  "version": 1,
  "tools": [
    {
      "tool": "node",
      "version": "24.10.0",
      "newer": "24.11.0",
      "newer_built": false
    },
    {
      "tool": "pnpm",
      "version": "10.18.0"
    }
  ]
}
```

The commands below change the VM's tools from a script or the command line. They don't change the VM's disk, because its tools are mounted, so a change takes effect when devvm then opens a shell in the VM. Each takes the tools' names as separate words, as in `devvm tools add python claude`, and devvm refuses names joined with `+` or a comma.

- `devvm tools add TOOL...` builds the tools that the share lacks, and they join the VM's list at once. It says which of the tools the VM has already and leaves those as they are. A new agent's config folders are mounted too, which needs the VM to start again. A stopped VM gets them as it starts, and for a running VM devvm asks before restarting it, because anything running in it stops ([restarting](vms.md#restarting-and-stopping)). If you answer no, there's no terminal or you pass `--no-input`, the VM gets them at its next start.
- `devvm tools rm TOOL...` takes tools out. What the VM installed itself, such as other versions or global packages, stays.
- `devvm tools update` checks upstream for newer versions, builds what's missing, and moves the VM to them. When the VM already has the newest versions, it builds any of them that are missing. It changes only the shared tools, and never makes the VM again.
- `devvm update` does the same first, without asking, and then asks to make the VM again only when the VM's image has changed ([outdated VMs](images.md#outdated-vms)), or its `devcontainer.json` has ([updating a VM from the file](devcontainer.md#updating-a-vm-from-the-file)).

`--tools`, `--agents` and `--layers` choose only what a new VM gets, so on a VM that exists they change nothing, and devvm warns and names `devvm tools`. Layers aren't shared tools, so `devvm tools` can't add or take out a layer such as `docker` or `nix`. A VM gets its layers when devvm makes it, so `devvm layer add` and `devvm layer rm` change them by making the VM again, after asking ([adding layers to a VM](images.md#adding-layers-to-a-vm)).
