---
title: Working on a clone
explainer: Give a VM its own clone of the repo
weight: 9
---

`devvm --clone` gives the VM its own clone of the repo instead of mounting your folder, so nothing in the VM can touch your working tree or `.git`. Use it when you don't trust what runs in the VM, such as an agent working unattended, and on Linux hosts, which have no [`.git` protection](../safety/git-protection.md#linux-hosts). [Files the VM writes](../safety/attack-vectors.md#files-the-vm-writes) lists what `--clone` keeps away from this machine.

```sh
devvm --clone              # create the VM, copy your branches into it and open a shell in its clone
git fetch devvm            # later, on this machine, fetch the VM's work as devvm/<branch>
git log -p main..devvm/main
```

```mermaid
sequenceDiagram
  participant R as Your repo
  participant V as The VM's clone
  R->>V: devvm --clone pushes<br>every local branch
  Note over V: you or an agent<br>commit in the VM
  V->>R: git fetch devvm gives<br>devvm/#lt;branch#gt;
  Note over R: review,<br>then merge
  R->>V: devvm rm looks for<br>commits not fetched
  V->>R: fetched into<br>saved/devvm/#lt;branch#gt;
  Note over R,V: the VM is deleted,<br>and its remote removed
```

## Requirements

- The folder is the top folder of a git repo that has at least one commit and a branch checked out, not a detached HEAD.
- The repo is the folder's own, or the folder is a worktree that `git worktree add` made. devvm refuses a `.git` that points into another repo, because the push would copy that repo's branches into the VM. That includes a `.git` file that isn't a worktree's, a worktree whose link back to this folder doesn't match, a `commondir` that leads to another repo, and objects or refs that are symlinks, or that sit in a folder that is one. devvm also refuses a repo that borrows objects from another repo through `objects/info/alternates`. To use such a repo, run `git repack -a -d` in it, then delete that file.
- git is installed on this machine. The VM's images already have it.
- You choose `--clone` when the VM is created, and once a VM is a `--clone` VM you don't need to pass `--clone` again. devvm refuses `--clone` for an existing VM that mounts its folder, and `devvm clone on` switches that VM instead ([switching a VM that exists](#switching-a-vm-that-exists)).

## What's copied

From this machine, the VM mounts only the shared tools and its [agents' config folders](coding-agents.md#mounts). Once the VM is running, devvm creates a repo in it at the same path as your folder and pushes every local branch into it. When the folder is a git worktree, devvm pushes only the worktree's branch and that branch's upstream, if it has one, and leaves the repo's other branches on this machine. The push goes over ssh to the VM itself and checks the host keys devvm pinned for it. No setting in your repo's config, such as a `url.*.insteadOf` that rewrites the VM's URL, can send the push anywhere else ([how the push connects](../internals/git-protection.md#what-a-lock-cant-stop)). devvm checks out your current branch in the clone, and your shell opens there.

Apart from a worktree's upstream, which can be a remote-tracking branch such as `origin/main`, devvm copies only your local branches. Tags, uncommitted changes, and untracked or ignored files such as `.env` and `node_modules` stay on this machine. devvm warns you about uncommitted changes, so you can commit them and send them later with `git push devvm BRANCH`.

If the copy fails, for example because the VM can't be reached, devvm tries again the next time you run it in the folder.

## The remote

devvm adds a git remote for the VM to your repo. It's named `devvm`, or `devvm-<folder>` if `devvm` is already taken, or the VM's full name if both are. The remote's URL is the VM's full name followed by the clone's path, so git connects through devvm's ssh config, and fetching needs two things:

- Your `~/.ssh/config` needs devvm's `Include` line ([SSH access by name](ssh.md#ssh-access-by-name)). devvm offers to add it when it creates the clone, and again when you run `devvm host` or `devvm code`. Until the line is there, or when devvm had no terminal to ask in, fetch with `GIT_SSH_COMMAND='ssh -F ~/.local/share/devvm/ssh_config' git fetch devvm`.
- The VM must be running. A stopped VM has no SSH entry, so `git fetch` fails because it can't resolve the name. Run `devvm` or `devvm host` in the folder first.

## Fetching the VM's work

Fetching runs nothing from the VM on this machine. git only receives objects and updates the `devvm/*` branches, or creates new `saved/devvm/*` branches when `devvm rm` does the fetch. The commits themselves are untrusted, though, so treat them as you would a contributor's branch and review them before you merge them, check them out or run anything they change.

## Subfolders

When you run devvm in a subfolder of the repo's folder, devvm uses the `--clone` VM as it uses any parent folder's VM ([a subfolder](vms.md#a-subfolder)). The clone is at the repo folder's path, so the subfolder's path in the VM is the same subfolder in the clone. When the clone has that subfolder, the shell opens there, and `devvm host` and `devvm code` give it as the folder to open. When the clone lacks it, for example because the subfolder is untracked or ignored and so never copied, they use the clone's top folder instead.

`devvm --clone` in a subfolder never uses a parent folder's VM. It makes a new `--clone` VM for the subfolder, as `devvm --here` does, and doesn't take `--parent`, so the subfolder must be the top folder of a repo of its own.

## Several agents in one VM

Several agents can work in one `--clone` VM, each on a branch of its own in a git worktree of its own, so each agent edits its own copy of the files. Put the worktrees inside the clone, such as in `.worktrees/NAME`, because the clone's parent folders in the VM belong to root, and so `git worktree add ../NAME` fails there. In the VM, in the clone:

```sh
echo .worktrees/ >> .git/info/exclude         # once, so git status in the clone leaves the worktrees out
git worktree add -b agent/api .worktrees/api  # a worktree on a new branch, agent/api
cd .worktrees/api                             # then start the agent here
```

Every worktree shares the clone's repo, so the agents' branches are all in it. One `git fetch devvm` on this machine brings every agent's branch back, such as `devvm/agent/api`, and you review each one as you would any of the VM's work ([fetching the VM's work](#fetching-the-vms-work)). Before `devvm rm` deletes the VM, it also checks each worktree inside the clone for uncommitted changes ([deleting the VM](#deleting-the-vm)).

Install each worktree's dependencies in that worktree, and never share one `node_modules` folder between worktrees, because each branch can need different packages, and two agents installing at once can break it. pnpm and bun keep one package store in the VM and hard-link packages from it into each worktree's `node_modules` when both are on the same disk, so another worktree costs little download or disk.

The agents also share the VM's ports, so two servers on the same port collide. Give each worktree's server a port of its own, such as with `PORT=3001`. A VM that forwards every port, as new VMs do by default, forwards each of them to this machine's `localhost` ([ports](ports-size.md#ports)).

The agents in one VM share its network tier and rules, its secrets, and its CPUs, memory and disk, and each agent can read and change every other agent's worktree. When the agents need different network tiers or rules, secrets or sizes, or when one agent mustn't reach another's work, give each agent a folder of its own on this machine, such as a separate clone of the repo, and run `devvm --clone` in each, so each gets a VM of its own.

## Deleting the VM

Before `devvm rm` deletes a `--clone` VM, it checks the clone for work you'd lose. It starts a stopped VM for the check as a launch does, so the VM's confinement is checked first:

1. If the clone has branches with commits this repo hasn't fetched, devvm names them and offers to fetch them into new local branches named `saved/devvm/<branch>`. It skips a branch whose name git on this machine refuses and names it, and that branch goes with the VM. To keep such a branch, rename it in the VM with `git branch -m` before you run `devvm rm`.
2. If the clone or a worktree inside its folder has uncommitted changes, devvm lists the first 20 of them in all, with each worktree's changes under its path, and asks before deleting them along with the VM. devvm skips a worktree whose folder is gone or that git would prune, and it doesn't check worktrees outside the clone's folder. When the clone has more than 100 worktrees, devvm checks the first 100 and asks before deleting the VM, even if none of them has changes.

If either check finds something and there's no terminal to ask in, or you give `--no-input`, devvm refuses and keeps the VM. These questions are asked only in a terminal, because only you can judge the work, so `--force` never answers them ([questions devvm asks](command-line.md#questions-devvm-asks)).

Once the checks pass, devvm deletes the VM. If the remote still points at the VM, devvm then removes it with `git remote remove`, which also deletes its `devvm/*` branches and their upstream settings. Before it does, devvm keeps every `devvm/*` branch that has commits no other branch has as a new local branch under `saved/devvm/`, whoever fetched it. devvm names each branch it creates, and they're yours to review, merge or delete, as with `git branch -D saved/devvm/main`.

The saved branches are named after the remote, as `saved/<remote>/<branch>`, and devvm never overwrites a branch. When a branch is in the way, devvm uses `saved/<remote>-2`, then `-3` and so on, and if it finds no free name, `devvm rm` refuses and keeps the VM ([saved branches](../internals/overview.md#saved-branches)).

`devvm rm --force` skips only the question whether to delete the VM at all, and both checks still run. `devvm rm --no-export` skips the export of the agents' history, and it also leaves both checks as they are.

`devvm rm --no-clone-check` skips both checks, so the VM goes with the commits this repo hasn't fetched and the uncommitted changes, and devvm warns you of that first. Use it when a check can't read the clone, such as when git in the VM is broken, and devvm refuses and keeps the VM. Use it too when the VM doesn't start for the check, such as when its tier isn't enforced, because devvm then says what kept it from starting and keeps the VM. devvm still asks whether to delete the VM unless you also give `--force`, and it still keeps the `devvm/*` branches you fetched as `saved/devvm/` branches.

## Switching a VM that exists

`devvm clone` shows whether this folder's VM has its own clone of the repo, which is `on`, or mounts the folder, which is `off`. `devvm clone on` and `devvm clone off` switch the VM by making it again from the image it has, as [`devvm layer add`](images.md#adding-layers-to-a-vm) does, and then open a shell in it:

```sh
devvm clone       # show on or off
devvm clone on    # make the VM again with its own clone of the repo, after asking
devvm clone off   # fetch the clone's work, then make the VM again with this folder mounted
```

The VM keeps its home folder and its settings, as [a VM made again](images.md#what-a-vm-made-again-keeps) does, and everything else on its disk is lost. So in a terminal devvm asks first, with no as the default, and without one it refuses unless you give `--force`. `--no-export` skips the export of the agents' history, and `--clean-home` gives the VM the image's home folder instead of its own, as for `devvm layer add`. When the VM is in the mode you ask for already, devvm says so and changes nothing.

`devvm clone on` needs a folder that `--clone` takes ([requirements](#requirements)), and devvm refuses before it changes anything when the folder isn't one. Your work is in the folder, so only the VM's disk is lost. The new VM mounts nothing of the folder, and as it starts, devvm copies your branches into its clone and adds [the remote](#the-remote), as for a new `--clone` VM ([what's copied](#whats-copied)). Once the new VM is ready and the old one is deleted, devvm takes the [`.git` locks](../safety/git-protection.md) off the folder, because no VM mounts it any more. First it names what the old VM changed that the locks can't stop, as a launch does ([when devvm reports .git](../safety/git-protection.md#when-devvm-reports-git)).

`devvm clone off` loses the clone, which is on the VM's disk, so first it checks the clone as `devvm rm` does ([deleting the VM](#deleting-the-vm)). It offers to fetch the branches this repo hasn't fetched into `saved/devvm/<branch>` branches, and it asks before the clone's uncommitted changes go. Only a terminal can answer these questions, and `--force` never does, so without a terminal, or with `--no-input`, devvm refuses and keeps the VM as it is. `devvm clone off --no-clone-check` skips the check after a warning, and the clone's unfetched commits and uncommitted changes are lost. The new VM mounts the folder again with `.git` protection as a new VM gets it, which is `on` on macOS and `off` on Linux hosts. Before the new VM first starts, and before devvm locks `.git`, it removes the remote and keeps each `devvm/*` branch that has commits no other branch has as a `saved/devvm/` branch, as `devvm rm` does. If the switch is then undone, devvm adds the remote back.

When a step fails, or you press Ctrl+C, before the new VM has its home folder, devvm puts the old VM back as it was, in its old mode, as it does for [any remake](images.md#adding-layers-to-a-vm).

A `--clone` VM keeps its clone on its own disk, so `devvm layer add`, `devvm layer rm` and `devvm update` refuse it. To change its layers or bring it up to date, run `devvm clone off`, then that command, then `devvm clone on`.

### devvm clone --json

`devvm clone --json` prints one JSON object with `clone`, which is `on` or `off`, as [output for scripts](command-line.md#output-for-scripts) describes. With `--json` the command only shows, so devvm refuses `on` or `off` with it, and the flags that go with them.

```json
{
  "version": 1,
  "clone": "on"
}
```
