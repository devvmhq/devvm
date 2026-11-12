# Changelog

## Unreleased

### Breaking

- **Your layers run after devvm's prelude:** devvm puts one line of its own in place of your layer's `#!` line, which gives the layer `$devvm_arch`, `$devvm_libc` and `devvm_sha`. A layer that sets its own `devvm_arch` or another `devvm_` name keeps its own. In a layer without a `#!` line, the line numbers in bash's errors are one more than the file's ([your own layers](https://next.devvm.dev/docs/using/customizing/#your-own-layers)).

### After you upgrade

- Base, every image built on it and every shared tool image are built again once, because base now has `git-lfs` and each build script now starts with devvm's prelude on one line, so that the line numbers in a build's errors are the script's own. A VM made from an older image says it's outdated until `devvm update` makes it again. Every existing VM also says there are newer shared tools, at the same versions, until `devvm update`, and the first update or new VM builds each of its shared tools again, one at a time, which takes several minutes. An existing `--clone` VM gets `git-lfs` once it's made again, with `devvm clone off`, then `devvm update`, then `devvm clone on`.

### New

- **Pins in your own layers:** a layer of yours gets `$devvm_arch`, `$devvm_libc` and `devvm_sha TOOL`, as devvm's own scripts do, so it can pin a download for each architecture and fetch the right one with `devvm-fetch "$URL" "$(devvm_sha TOOL)" OUT` ([the example](https://next.devvm.dev/docs/using/customizing/#example-mailpit)).
- **Cursor's CLI as an agent:** `--agents cursor` gives a VM Cursor's CLI, run as `cursor-agent` or `agent`, at the release devvm pins, with its self-update off. `devvm agent login cursor` logs it in from the VM, in a browser on any machine, and the login stays on this machine, as the other agents' do ([Cursor CLI login](https://next.devvm.dev/docs/using/coding-agents/#cursor-cli-login)).
- **`--clone` copies your repo as `git status` shows it:** the VM's clone gets every local branch, every tag, an annotated one staying annotated, your notes and replace refs, and your remotes' remote-tracking branches, such as `origin/main`, with `origin/HEAD` still pointing at its branch. It's checked out as your repo is, a detached HEAD included, with what you've staged staged and what you haven't changed in its working tree, deleted files and `git add -N` files included. A rebase, merge, cherry-pick, revert or bisect in progress is in progress there too, so `git rebase --continue` and the like finish it in the VM, an autostash included, and your stash goes, a `git stash -u` entry with its untracked files. Untracked and ignored files, such as `.env`, still stay on this machine, and so do another devvm VM's remote-tracking branches, refs such as `refs/original`, `ORIG_HEAD`, a stash entry that may be from `git stash --all`, which holds ignored files, a filtered file's uncommitted change, such as git-crypt's, and a tracked file that you turned into a folder, and devvm names each one you'd otherwise expect in the VM ([what's copied](https://next.devvm.dev/docs/using/clone/#whats-copied)).
- **`--clone` copies your repo's settings and hooks:** the VM's clone gets your remotes, with no credentials in their URLs, the upstream of each branch it gets, the submodules' settings, sparse checkout's, a `core.hooksPath` inside the repo, such as husky's, and the repo's own `user.name` and `user.email`, so `git fetch origin` and `git pull` work there wherever the VM reaches the remote and has its credentials. As `origin` is real there, code in a VM that may use your GitHub token can push to it, which a network rule can stop. Your hooks go as they are, when git runs them from the repo's hooks folder. Credential helpers, extra HTTP headers, `core.sshCommand`, aliases and filters stay on this machine, and devvm names a remote or hook it leaves out ([the settings](https://next.devvm.dev/docs/using/clone/#the-settings)).
- **The VM's work without a branch comes back:** `devvm clone fetch` saves commits that only the VM's detached HEAD has as `saved/devvm/HEAD`, and each stash entry the VM made as `saved/devvm/stash-N`, its untracked files apart in `saved/devvm/stash-N-untracked`, so `git stash apply` never writes them unseen, and its `--json` lists these branches in `saved`. `devvm rm` and `devvm clone off` name the same commits and entries and offer to fetch them there, and they name a worktree in the VM whose detached HEAD has commits nothing else has, and ask before it goes. They don't count the uncommitted changes copied in as the VM's work while the VM leaves them as they came ([deleting the VM](https://next.devvm.dev/docs/using/clone/#deleting-the-vm)).
- **`--clone` of a partial clone:** a repo from `git clone --filter` is copied with the objects it has, and the VM's git fetches the rest from its remote when it needs them. devvm says when the VM's network policy blocks that remote, the remote is on this machine or stays here, or a branch or tag points to an object the clone never fetched, and when `devvm clone fetch` needs an object the VM's clone lacks, it says how to get it. A partial clone that's sparse, shallow or a worktree is copied as well ([a partial clone](https://next.devvm.dev/docs/using/clone/#a-partial-clone)).
- **Git LFS in `--clone` VMs:** every VM has `git-lfs`, set up in each clone the copy makes and nowhere else, and the copy sends the LFS objects your checked-out files point to, so they have their content in the VM. `devvm clone fetch`, `devvm rm` and `devvm clone off` bring back the LFS objects the VM's work points to, each checked against its pointer, without `git-lfs` on this machine, and name any that didn't come back. A plain `git fetch devvm` brings none, and `devvm rm` names the objects only the VM has and asks before they go. A VM made before has no `git-lfs` until it's made again, and its LFS files stay pointers ([Git LFS](https://next.devvm.dev/docs/using/clone/#git-lfs)).
- **`--clone` copies your submodules:** each submodule you've initialised, nested ones too, gets a clone of its own in the VM at the commit it has checked out here, so `git submodule status` there shows what it shows here, and a `devvm` remote in its repo here. `devvm clone fetch` fetches each one's branches, and `devvm rm` checks each one for work you haven't fetched, even one you deinitialise later, as devvm records the submodules it copied in the VM's state, a field that an older devvm ignores. An uninitialised submodule stays an empty folder ([submodules](https://next.devvm.dev/docs/using/clone/#submodules)).

### Changed

- **`--clone` refuses a repo with a grafts file** before it makes the VM, naming what to do ([requirements](https://next.devvm.dev/docs/using/clone/#requirements)).
- **A folder of repos with `--clone`:** each repo's clone gets every local branch, tag and remote-tracking branch, not just the current and default branches, and a repo whose HEAD is detached is detached in the VM too, rather than on its default branch.
- **A git worktree with `--clone`:** its VM's clone gets the repo's default branch, remote-tracking branches and tags too, besides the worktree's branch and its upstream, and devvm says that the repo's other local branches stay on this machine.

### Fixed

- **`--clone` of a shallow or SHA-256 repo:** a repo from `git clone --depth 1` failed after devvm made the VM, with `shallow update not allowed`, and a SHA-256 repo with `the receiving end does not support this repository's hash algorithm`. devvm now copies the branches without `git push`, so both work, and a VM whose copy failed gets its clone the next time you run devvm in its folder ([a copy that didn't finish](https://next.devvm.dev/docs/using/clone/#a-copy-that-didnt-finish)).
- **dotnet on the allowlist tier:** when NuGet couldn't fetch DigiCert's revocation list, it asked DigiCert's OCSP responder, which the allowlist blocked. `ocsp.digicert.com` on port 80 is on dotnet's allowlist now ([dotnet](https://next.devvm.dev/docs/using/toolchains/#dotnet)).
- **History you packed on a Mac:** `devvm history import FILE` refused a `.tar.gz` made with macOS's `tar`, because its AppleDouble `._` files, which hold files' extended attributes, sit outside the agents' history. The import now leaves them out ([importing history](https://next.devvm.dev/docs/using/coding-agents/#importing-history)).

## 0.1.1-rc.2

A release candidate for 0.1.1, adding to 0.1.1-rc.1. Install it with `devvm upgrade --to 0.1.1-rc.2`, the install script's `--version 0.1.1-rc.2`, or `brew install devvmhq/tap/devvm-rc`. Homebrew's `devvm` stays on 0.1.0.

### After you upgrade

- Base and every image built on it are built again once, for transparent mode's programs.
- Run `devvm update` in an existing VM's folder to give it transparent mode, which new VMs get by default: its image predates the programs it needs.
- On a Linux host, run `devvm mount-cache none` for each existing VM, to stop the 9p cache bug below. Each launch reminds you until you do.
- If you used `devvm git-protect strict`, use `devvm clone on` for code you don't trust: strict is gone.
- Make new API keys for any VM you don't trust that had keys passed to it as they were.

### New

- **Transparent mode (#35), on by default:** on every tier but open, tools that ignore the proxy settings, such as `wrangler dev`, static Go binaries and containers, reach the VM's proxy and its allowlist anyway. A VM made before needs `devvm update` first, and `devvm network transparent off` turns it off for a VM ([what it covers](https://next.devvm.dev/docs/safety/network/#tools-that-ignore-the-proxy)).
- **Folders of repos with `--clone` (#3):** one VM gets a clone of each repo you pick, at the same paths. `devvm clone fetch` brings back every repo's work, and `devvm clone add PATH` adds one ([folders of repos](https://next.devvm.dev/docs/using/clone/#folders-of-repos)).
- **Rules by HTTP method and path:** `--method` and `--path` on `devvm network allow`, `deny` and `rm`, such as `devvm network deny api.github.com --method POST,PUT,PATCH,DELETE`.
- **Your own secrets:** `devvm secret set NAME --host HOST --env VARIABLE`, then `devvm secret use NAME`. The VM only ever sees a placeholder, and `--method` and `--path` limit where the real value goes.
- **Learning a VM's hosts (#17):** `--network learn`, then `devvm network lock` to keep the hosts you choose.
- **`.git` protection on Linux (#4, #20):** `.git` is locked read-only around QEMU, without root. Where a host can't, such as Ubuntu with AppArmor, devvm watches `.git`, warns at each launch, and names what changed.
- **Nested repos (#3):** a VM protects the `.git` of each repo up to 4 folders down. `devvm doctor` suggests `safe.bareRepository explicit`.
- **pi and more providers (#23, #31, #32):** pi is a fourth agent, and Gemini, Groq, Mistral, xAI and DeepSeek keys stay on this machine.
- **Prereleases on Homebrew (#19):** `brew install devvmhq/tap/devvm-rc`.

### Changed

- **The 9p cache on Linux (#36):** Lima's default cache could write files to this machine empty or cut short, and made SQLite and Gradle fail. New Linux VMs use `none`, and `devvm mount-cache none` switches existing ones. Under `none`, keep SQLite WAL databases and other files that programs map for writing on the VM's own disk, or use `--clone`. macOS doesn't change.
- **A `--clone` VM in a folder another VM mounts** is refused, and `devvm rm` asks before deleting one made earlier.
- **Logins:** a VM's copy of Codex's and Claude Code's login files keeps only the fields devvm knows. opencode, like pi, gets no key devvm doesn't bind, unless you list the provider in `config.toml`'s `[pass-keys]` (#30).
- **Warnings** print once per command.
- **Nix** is 2.35.5.

### Removed

- **`devvm git-protect strict` (#37):** it couldn't keep git on this machine out of submodules the VM adds, so it's a usage error now. Strict VMs become `on`, and devvm takes out the `diff.ignoreSubmodules = all` it set, keeping a value of your own ([VMs that were strict](https://next.devvm.dev/docs/safety/git-protection/#vms-that-were-strict)).

### Fixed

- opencode's history copy with SQLite 3.54 (#26), a NUL byte in an ssh socket's path (#25), a build VM reusing its clone's ssh port, a login saved for an agent the VM lacks (#27), and `.git` protection locking a folder that only looks like a git folder (#28).

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
