# Changelog

What's changed on `main` since the latest release is in [Unreleased](#unreleased), at the bottom of this page.

## 0.1.1-rc.3

A release candidate for 0.1.1, adding to 0.1.1-rc.2. Install it with `devvm upgrade --to 0.1.1-rc.3`, the install script's `--version 0.1.1-rc.3`, or `brew install devvmhq/tap/devvm-rc`. Homebrew's `devvm` stays on 0.1.0.

### Breaking

- **Your layers run after devvm's prelude:** devvm puts one line of its own in place of your layer's `#!` line, which gives the layer `$devvm_arch`, `$devvm_libc` and `devvm_sha`. A layer that sets its own `devvm_arch` or another `devvm_` name keeps its own. In a layer without a `#!` line, the line numbers in bash's errors are one more than the file's ([your own layers](https://next.devvm.dev/docs/using/customizing/#your-own-layers)).

### After you upgrade

- Base, every image built on it and every shared tool image are built again once, because base now has `git-lfs` and each build script starts with devvm's prelude on one line, which keeps the line numbers in a build's errors the script's own.
- Run `devvm update` in each existing VM's folder. Until you do, a VM made from an older image says it's outdated, and every existing VM says there are newer shared tools at the same versions. The first update or new VM builds each of its shared tools again, one at a time, which takes several minutes.
- An existing `--clone` VM gets `git-lfs` only once it's made again, with `devvm clone off`, then `devvm update`, then `devvm clone on`. Until then, its Git LFS files stay pointers.

### New

- **Cursor (#44, thanks to @barnesoir):** `--agents cursor` gives a VM Cursor's CLI, as `cursor-agent` or `agent`, and `devvm agent login cursor` logs it in from a browser on any machine, with the login kept on this machine ([Cursor CLI login](https://next.devvm.dev/docs/using/coding-agents/#cursor-cli-login)). `devvm cursor` opens the folder's VM in the Cursor editor, as `devvm code` does in VS Code. Recent Cursor runs its Agent in the VM with your Cursor sign-in in memory, so `devvm cursor` warns unless your Cursor settings send Agent's requests through the app on this machine, and you should open only VMs you trust with it ([Cursor's Agent](https://next.devvm.dev/docs/using/ssh/#cursors-agent)).
- **`devvm paste` (#38, thanks to @DustyPolk):** sends the image, PDF or file you copied on this machine into the running VM and puts its path in the VM on your clipboard, so you can paste it into a coding agent. A copied file goes only after you say yes, and `devvm paste rm` deletes what it sent ([pasting images](https://next.devvm.dev/docs/using/coding-agents/#pasting-images)).
- **`devvm agent import` can follow your dotfiles' links:** a link out of an agent's folder, such as `~/.claude/skills/x` into a dotfiles repo, is copied when you say yes in a terminal, when you pass `--follow-links`, or when it leads into a folder that `config.toml`'s `follow-links` names, such as `["~/dotfiles"]`. The store gets copies, never links, and devvm never follows a link to your home folder, its own folders, places that hold secrets, such as `~/.ssh`, or an agent's logins or history ([links out of an agent's folder](https://next.devvm.dev/docs/using/coding-agents/#links-out-of-an-agents-folder)).
- **`--clone` copies your repo as it is here:** the VM's clone gets every branch, tag and remote-tracking branch, a detached HEAD, what you've staged and what you haven't, a rebase, merge or other operation in progress, and your stash. It also gets your remotes without credentials, the repo's hooks and its own settings, your initialised submodules, each with a clone of its own, and the Git LFS objects your checked-out files point to, as every VM now has `git-lfs`. A partial clone is copied with the objects it has, and the VM's git fetches the rest from its remote. Untracked and ignored files, such as `.env`, and credential helpers stay on this machine, and devvm names anything it leaves out that you'd expect in the VM. Because `origin` is real in the VM, code there that may use your GitHub token can push to it, which a network rule can stop ([what's copied](https://next.devvm.dev/docs/using/clone/#whats-copied), [the settings](https://next.devvm.dev/docs/using/clone/#the-settings), [submodules](https://next.devvm.dev/docs/using/clone/#submodules), [a partial clone](https://next.devvm.dev/docs/using/clone/#a-partial-clone), [Git LFS](https://next.devvm.dev/docs/using/clone/#git-lfs)).
- **All of a `--clone` VM's work comes back:** `devvm clone fetch` also saves commits that only the VM's detached HEAD has as `saved/devvm/HEAD` and the VM's stash entries as `saved/devvm/stash-N`, and it brings back each submodule's branches and the LFS objects the VM's work points to, without `git-lfs` on this machine. `devvm rm` and `devvm clone off` check all of these and ask before any of them goes ([deleting the VM](https://next.devvm.dev/docs/using/clone/#deleting-the-vm)).
- **Pins in your own layers:** a layer of yours can pin a download for each architecture and fetch the right one with `devvm-fetch "$URL" "$(devvm_sha TOOL)" OUT`, as devvm's own scripts do ([the example](https://next.devvm.dev/docs/using/customizing/#example-mailpit)).
- **Quiet hosts:** Cursor CLI's metrics host, `api3.cursor.sh`, and Go's `telemetry.go.dev` stay blocked, but their blocks no longer show before the VM's prompt, in `devvm-why` or in `devvm network lock`. `devvm network log` still lists them, each line ending `, quiet` ([quiet hosts](https://next.devvm.dev/docs/safety/network/#quiet-hosts)).

### Changed

- **`--clone` in a folder of repos or a git worktree** copies each repo as above. A worktree's clone also gets the repo's default branch, remote-tracking branches and tags, and devvm says that the repo's other local branches stay on this machine.
- **`--clone` refuses a repo with a grafts file** before it makes the VM, and says what to do ([requirements](https://next.devvm.dev/docs/using/clone/#requirements)).
- **The proxy counts more addresses as internal:** a 6to4 address, in `2002::/16`, is judged by the IPv4 address in it, as a NAT64 address is, so `2002:a00:1::1` is as private as `10.0.0.1`. A deny for an IPv4 address also covers its 6to4 form, but an allow doesn't, because anyone may run a 6to4 relay. Teredo, IPv6's and IPv4's documentation ranges and other special ranges are refused unless a rule names the address, and `--from`'s registries are judged by the same list ([the proxy](https://next.devvm.dev/docs/internals/network/#the-proxy)).
- **Pins:** base's mise is 2026.11.8, Docker's buildx is 0.39.0 and compose 5.7.1, and Nix is 2.35.6.

### Fixed

- **`--clone` of a shallow or SHA-256 repo (#40):** a repo from `git clone --depth 1` or one that uses SHA-256 failed after devvm made the VM. Both work now, and a VM whose copy failed gets its clone the next time you run devvm in its folder. Thanks to @Josuesp1620 for the report and @dhruv1220 for finding the cause in #42 ([a copy that didn't finish](https://next.devvm.dev/docs/using/clone/#a-copy-that-didnt-finish)).
- **A stopped VM no longer keeps its listed ports from other VMs:** only a running VM holds a port. When the first VM starts again, devvm and `devvm ports` say that the other VM has the port, and the first VM takes it back within about 20 seconds of the other one stopping. Meanwhile, both VMs' apps share that port's site data and service workers in your browser ([two VMs and one port](https://next.devvm.dev/docs/using/ports-size/#two-vms-and-one-port)).
- **`devvm ports` says why a listed port isn't forwarded,** such as the forwarder's limit of 256 ports or Linux refusing a port below 1024, instead of always blaming a program on this machine ([a listed port isn't forwarded](https://next.devvm.dev/docs/using/troubleshooting/#a-listed-port-isnt-forwarded)). When a port's forward ends, so do its connections ([how it reaches the VM](https://next.devvm.dev/docs/internals/ports/#how-it-reaches-the-vm)).
- **A name that doesn't resolve says why:** a block now says whether this machine's DNS has no address for the name or didn't answer in time, and after a failure that may pass, the proxy asks once more before it blocks the name ([log files](https://next.devvm.dev/docs/internals/network/#log-files)).
- **dotnet on the allowlist tier** can reach DigiCert's OCSP responder, `ocsp.digicert.com` on port 80, which NuGet asks when it can't fetch DigiCert's revocation list ([dotnet](https://next.devvm.dev/docs/using/toolchains/#dotnet)).
- **`devvm history import FILE`** takes a `.tar.gz` made with macOS's `tar`, leaving out its AppleDouble `._` files ([importing history](https://next.devvm.dev/docs/using/coding-agents/#importing-history)).
- **opencode's links at `devvm agent import`** are copied or named as the other agents' are, where they were dropped without a word ([importing from this machine](https://next.devvm.dev/docs/using/coding-agents/#importing-from-this-machine)).

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

## Unreleased

These changes are on `main` and go out in the next release.

### Fixed

- **A removed link's connections end with it:** devvm link rm used to stop a link's new connections but leave its open ones running. A target that never closed its end kept each of those connections' ssh processes on this machine, and one of the 256 connections a client's links carry at once, until the client or the target stopped. Removing a link now ends its open connections within a few seconds, and when a target starts again, the connections still open through its earlier start end too ([removing a link](https://next.devvm.dev/docs/using/links/#removing-a-link)).
- **A closed connection's ssh process ends:** when a VM never closed its end of a forwarded port's or a link's connection, that connection's ssh process stayed on this machine after the program here, or in the client, had closed its side. So a hostile VM could take all 256 connections that a forwarder or a client's links carry at once. Such an ssh process now ends once its client has closed and the VM has sent nothing for 30 seconds. A program that closes its side for writing and then waits for an answer, as `nc -N` does, keeps its connection only while the VM sends something at least every 30 seconds, the first byte included, so a slow query that takes longer than that to start answering is cut ([how it reaches the VM](https://next.devvm.dev/docs/internals/ports/#how-it-reaches-the-vm)).
- **A forwarded port answers on `::1` too:** when this machine briefly couldn't listen on a port's IPv6 address, such as when it ran out of open files, the forwarder forwarded the port on `127.0.0.1` alone until the server in the VM restarted. A client that connects only over IPv6, such as `http://[::1]:PORT` or a tool that doesn't fall back to `127.0.0.1`, then didn't reach the VM, and another program on this machine could take `::1` on that port and get those connections instead. The forwarder now tries the port again at the VM's next report and forwards it on both addresses. A machine that denies the forwarder `::1` alone, such as through a security module, still gets its ports on `127.0.0.1`, and the forwarder's log says so ([when it can't forward](https://next.devvm.dev/docs/internals/ports/#when-it-cant-forward)).
