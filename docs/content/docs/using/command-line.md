---
title: The command line
explainer: Help, flags, questions, JSON output and completion
weight: 15
---

This page covers what works the same way in every command: the help, flags and their values, the questions devvm asks and what answers them beforehand, `--json` for scripts, and shell completion. [Everyday commands](../getting-started/commands.md) lists the commands, and the [CLI reference](../cli/_index.md) has every command with all its flags.

## Help

`devvm help COMMAND`, `devvm COMMAND --help` and `devvm COMMAND -h` print a command's help. It starts with a few sentences on what the command does, then its usage line and its examples, the most common first, each with a comment. Then come its commands, if it has any, and its flags, and the help ends with the address of the docs page that covers the command. `devvm --help` lists the commands in the groups that [Everyday commands](../getting-started/commands.md) has, which are Your VM, Change your VM, Images, Disk space and This machine, with `help` under Additional Commands, and it ends with devvm's issue tracker after its docs page.

devvm itself takes no word after it but a command's name, so every word there is a command, and a layer may have any name a command has. When the word after `devvm` isn't a command, devvm refuses it as a usage error. If the word names a toolchain, an agent or a layer, your own layers included, devvm points you to the flag that chooses it for a new VM, such as `devvm --tools node` for `devvm node`, and a word that joins several with `+` or a comma gets one line with each flag, such as `devvm --tools node --agents claude` for `devvm node+claude`. Any other word gets the closest command, such as `devvm stop` for `devvm stpo`. `devvm stpo --help` and `devvm help stpo` are refused the same way.

## Flags and values

Each flag belongs to the commands that list it in their help, and only `-h` or `--help`, `-v` or `--verbose` and `--no-input` work with every command. `-v` shows the output of the commands devvm runs as they run ([details](troubleshooting.md#a-command-fails)), and `--no-input` makes devvm ask nothing ([questions devvm asks](#questions-devvm-asks)). A flag can come before or after the command's arguments, and options that take a value also work with an equals sign, such as `--cpus=N`.

The options for a new VM, such as `--tools` and `--cpus`, are taken only by `devvm`. Another command given one of them, such as `devvm ls --cpus 4`, fails as it does for any flag it doesn't know ([what a new VM gets](images.md#what-a-new-vm-gets)).

When a flag or a setting can mean none or off, you write that as a word: `devvm setup --features none`, `devvm --agents none`, `devvm ports none`, `devvm retention history.age=off` and `devvm ssh-agent add KEY_FILE --lifetime forever`. devvm refuses an empty value, such as `--features ''`, and an empty argument, so a blank never stands for none or for a default.

Every duration is a whole number followed by a unit, which is `m` for minutes, `h` for hours, `d` for days or `w` for weeks, as in `30m`, `12h`, `30d` or `2w`. That covers `devvm gc --older-than`, the retention settings' ages and `devvm ssh-agent add --lifetime`, and only `--lifetime` also takes `forever`, for a key that devvm's agent keeps until it ends.

## Questions devvm asks

devvm asks a question only in a terminal, and each question has a flag or a command that answers it beforehand, so a script never needs to answer one. Without a terminal, with `--no-input`, or when the input ends before an answer, devvm can't ask, and every question is handled the same way in all three cases. A question that only adds something, such as an agent's folders or the `Include` line, gets its usual answer or a warning that says what to do, and a command that deletes something refuses unless you give it `--force`.

| Question | Answered by |
| --- | --- |
| the CPUs and memory new VMs get, on the first run | `--cpus` and `--memory`, or `devvm size -g`, or nothing: where devvm can't ask, new VMs get the size they would anyway |
| the coding agents new VMs get, on the first run and in `devvm setup` | `devvm setup --agents LIST` or `devvm setup --agents none` |
| base's features, on the first run before the new VM's summary, or on the first build | `devvm setup --features LIST` or `devvm setup --features none` |
| whether to customize a new VM, after its summary | `--tools`, `--agents`, `--layers`, `--network`, `--ports`, `--cpus`, `--memory` and `--disk`, or nothing: where devvm can't ask, the VM gets what the summary shows |
| a new VM's toolchains, coding agents and layers, when you customize it | `--tools`, `--agents` and `--layers` |
| a new VM's network tier, chosen from a list when you customize it | `--network TIER`, or `devvm network -g` with a tier for every new VM, or nothing: where devvm can't show the list, the VM gets the tier the summary shows |
| the ports a new VM forwards, chosen from a list when you customize it: every port, none, or the ports you type | `--ports`, or `devvm ports -g` with the ports every new VM forwards, or nothing: where devvm can't ask, the VM forwards the ports the summary shows |
| a new VM's CPUs, memory and disk, when you customize it | `--cpus`, `--memory` and `--disk`, or nothing: where devvm can't ask, the VM gets the size the summary shows |
| the layers new VMs get, in `devvm setup` | `devvm setup --layers LIST` or `devvm setup --layers none` |
| a VM's toolchains and agents, in `devvm tools` | `devvm tools add` and `devvm tools rm`, or `--json` to show them |
| new VMs' network tier, in `devvm setup` | `devvm network -g` |
| whether to load an SSH key into your agent | `devvm ssh-agent add KEY_FILE` beforehand |
| whether to restart a running VM to apply a change from `devvm size`, `devvm network`, `devvm ssh-agent` or `devvm git-protect`, or, as devvm opens it, the changes that wait for its next start, such as an agent's new folders | `devvm restart` afterwards, or nothing: without a terminal or with `--no-input`, the change waits for the VM's next start |
| whether to add the `Include` line to `~/.ssh/config` | `--ssh-config` on `devvm`, `devvm host` and `devvm code` |
| a Claude Code token or a GitHub token | `devvm secret set NAME --stdin` |
| an agent's API key, in `devvm agent login --api-key` | `--stdin` |
| whether to delete, in `devvm rm`, `devvm gc`, `devvm gc --caches`, `devvm images rm` and `devvm secret rm` | `--force` |
| whether to replace the images built on one, in `devvm rebuild` | `--force` |
| whether to replace what devvm's agent store has, in `devvm agent import` | `--force` |
| whether to install a release, in `devvm upgrade` | `--force` |
| whether to use the VM made for an earlier folder here, in `devvm adopt`, or to move a VM to this folder, in `devvm adopt VM` | `--force` |
| whether to move the VM made for this folder at the path it had before, in any command on this folder's VM | `devvm adopt --force` |

`--force` only skips a confirmation, and it never deletes more than the command does without it. A command that would delete something asks first in a terminal, and where devvm can't ask, it refuses unless you give `--force`.

A few questions protect work that only you can judge, so no flag answers them, and devvm asks them only in a terminal. They are `devvm rm` deleting the VM of a folder this one is inside or the VM made for an earlier folder at this path, a `--clone` VM's commits that this repo hasn't fetched and its uncommitted changes, a command giving the VM of a folder this one is inside more access, and `devvm agent save` saving a VM's settings for every VM. Where devvm can't ask, each of them refuses and says what to run instead.

With `--no-input`, devvm asks nothing even in a terminal, and it never reads an answer from a pipe or a file either. A question that has no usual answer fails instead, after devvm shows the question, and the error names the flag or command that answers it. A question fails the same way when the input ends before an answer, as it does when you press Ctrl+D at the prompt.

## Output for scripts

Every command that shows or lists something takes `--json`, which prints what the command shows as one JSON object instead of a table or lines, so a script can read it. The object's first field is `"version": 1`, and the page for each command lists its other fields with an example. A shape only ever gains fields. devvm adds new fields to version 1, and any other change to a shape, such as a field renamed, removed or given another type, comes with a new version.

With `--json`, standard output gets only the JSON. devvm's messages, such as warnings and notes, still go to standard error, as they do without it, and the JSON is never colored. A list with nothing in it is `[]`, never `null`. Sizes are in bytes, unless a field's name gives the unit, such as `memory_gib`. A field that doesn't apply, such as a VM's `from` when its base is Ubuntu's cloud image, is left out.

A command that shows a setting when you run it bare and changes the setting when you give it a value, such as `devvm network` or `devvm size`, only shows with `--json`. devvm refuses a value given with `--json`, such as `devvm network open` with `--json`, and changes nothing.

| Command | Its JSON |
| --- | --- |
| `devvm ls --json` | [the project VMs](vms.md#devvm-ls---json) |
| `devvm images --json` | [the images and the shared tool images](images.md#devvm-images---json) |
| `devvm tools --json` | [this folder's VM's shared tools](toolchains.md#devvm-tools---json) |
| `devvm size --json`, `devvm ports --json` | [this folder's VM's size and ports](ports-size.md#as-json), or with `-g`, new VMs' |
| `devvm network --json` | [this folder's VM's network tier and allowlist](../safety/network.md#devvm-network---json), or with `-g`, new VMs' choice |
| `devvm network log --json` | [this folder's VM's network log, or with `--blocked` its blocks](../safety/network.md#devvm-network-log---json) |
| `devvm secret --json` | [the secrets, whether each is saved and the VMs using it](../safety/network.md#devvm-secret---json) |
| `devvm ssh-agent --json` | [this folder's VM's SSH agent forwarding and devvm's own agent's keys](ssh.md#devvm-ssh-agent---json) |
| `devvm link --json` | [this folder's VM's links](links.md#devvm-link---json) |
| `devvm link publish --json` | [the ports this folder's VM publishes to links](links.md#devvm-link-publish---json) |
| `devvm git-protect --json` | [this folder's VM's `.git` protection](../safety/git-protection.md#devvm-git-protect---json) |
| `devvm history --json` | [the saved exports of agents' history](coding-agents.md#devvm-history---json) |
| `devvm space --json` | [what devvm keeps on this machine](space.md#devvm-space---json) |
| `devvm retention --json` | [the retention settings](settings.md#devvm-retention---json) |
| `devvm doctor --json` | [what devvm needs on this machine and what's missing](../internals/install.md#devvm-doctor---json), and it prints nothing else |
| `devvm upgrade --json` | [the newest release and how this devvm was installed](upgrading.md#devvm-upgrade---json), and it installs nothing |

## Shell completion

`devvm completion SHELL` prints the shell completion script for `bash`, `zsh` or `fish`. The script completes commands, flags and the image names that `devvm build` and `devvm rebuild` take, including layers joined with `+` after the ones you've already typed. It also completes shared tools for `devvm tools add` and `devvm tools rm`, the lists that `--tools`, `--agents` and `--layers` take, after the names you've already typed and their commas, and `all` and `none` for `--ports` and `devvm ports`. Load it from your shell's startup file:

| Shell | Line | In |
| --- | --- | --- |
| bash | `source <(devvm completion bash)` | `~/.bashrc`, with the bash-completion package installed |
| zsh | `source <(devvm completion zsh)` | `~/.zshrc`, after `compinit` |
| fish | `devvm completion fish \| source` | `~/.config/fish/config.fish` |
