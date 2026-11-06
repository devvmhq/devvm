# devvm: a guide for the agents in this VM

You're running in a devvm VM. devvm gives each project folder on the user's machine its own Linux VM, and the user runs devvm on that machine, the host, never in here. This guide is the same in every VM, so it says how devvm VMs work and not how this one is set up. `devvm-help` prints it, and the full docs are at <https://devvm.dev/docs/>.

## This VM

- The project folder is mounted read-write at the same path as on the host, so the user's editor sees your changes at once. It's the only folder of the host's that the VM can write to: the agent store's folders and the shared tools are mounted read-only.
- Your home folder is on the VM's own disk, not the host's. The coding agents' settings, skills and logins come from devvm's agent store, and the folders among them are mounted read-only.
- devvm update, devvm layer add and devvm layer rm make the VM again from a newer or another image. The project folder and your home folder survive, and everything else on the disk is lost, such as packages installed with apt, programs installed outside your home and Docker's images. Shell settings of your own go in `~/.bashrc.d`, because the image owns `~/.bashrc`.
- In the project folder, git can commit, branch, fetch and pull, but `.git`'s hooks and config are locked, so `git config`, `git remote add`, `push -u` to a new branch and hook installers fail with `Operation not permitted`. Ask the user to change them on the host. A VM made with `--clone` has its own clone of the repo instead, and the user fetches its branches with `git fetch devvm`.

## Network and secrets

Every request goes through a proxy that devvm runs on the host, and the VM's network tier decides what the proxy lets through:

| Tier | What the VM reaches |
| --- | --- |
| `open` | whatever the host reaches, its `localhost` and LAN included |
| `internet` | every public host, but no host or LAN address |
| `allowlist` | only the hosts on the VM's allowlist (the default) |
| `none` | only its agents' hosts, its secrets' hosts and its links |

The VM doesn't record its tier, and the user sees it with devvm network on the host. A request that the tier refuses fails with a 403 from the proxy, which names the host and the rule that would allow it, and on every tier but open a direct connection or a DNS lookup that skips the proxy fails at once.

- Run `devvm-why` to see what the proxy blocked lately. A note before each shell prompt says when something new was blocked.
- The proxy settings are in the environment (`HTTP_PROXY`, `HTTPS_PROXY` and their lowercase forms). A tool that ignores them, such as Java, needs the proxy's address in its own settings. A DNS or connection error for a host the tier allows means the tool skipped the proxy, and devvm-why doesn't show it.
- No secret is in the VM. `GH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` and the agents' logins hold placeholders, and the proxy puts the real value into each request for that secret's hosts. Use them as they are, and don't copy them anywhere else, because they work nowhere but through this VM's proxy.
- git over ssh goes through the proxy too, with `devvm-ssh-proxy` as its ProxyCommand, so the host must be one the tier allows.
- Browsers ask for a proxy's password, so they use `devvm-local-proxy`, which the desktop starts on the VM's loopback.

## Tools and installs

- The toolchains, such as node ([the list](https://devvm.dev/docs/using/toolchains/)), and the coding agents are shared tools, mounted read-only from images that every VM shares. They can't update themselves, and global installs go into your home: `npm i -g` installs into `~/.npm-global`, Python's packages go into a virtual environment or `~/.local/bin`, and `dotnet tool install -g` installs into `~/.dotnet/tools`.
- mise runs the shared tools. A project that asks for another version, in `.tool-versions`, `mise.toml` or its own toolchain file, gets it from mise, which installs it in your home on first use.
- `sudo` needs no password, and `apt install` (`apk add` on Alpine) goes through the proxy, whose allowlist for every VM has Ubuntu's, Debian's and Alpine's package hosts. What it installs is lost when the VM is made again.
- `devvm-fetch URL SHA256 OUT` downloads a file and fails when its checksum doesn't match.

## What to ask the user for

These commands run on the host, in the project folder, so ask the user to run them:

| You need | The user runs |
| --- | --- |
| a host the proxy blocks | `devvm network allow RULE`, such as `devvm network allow example.com` |
| a dev server's port on the host's `localhost` | `devvm ports add LIST`, such as `devvm ports add 5173` |
| another toolchain or agent | `devvm tools add TOOL`, such as `devvm tools add python` |
| a layer, such as docker, desktop or nix | `devvm layer add LAYER`, which makes the VM again |
| an agent's changed settings, skills or plugins kept for every VM | `devvm agent save` |
| the GitHub token in this VM | `devvm secret set github`, then `devvm secret use github` |
| an agent logged in | `devvm agent login AGENT`, such as `devvm agent login claude` |
| `.git`'s hooks or config changed | devvm git-protect off, with the VM stopped, then devvm git-protect on |
