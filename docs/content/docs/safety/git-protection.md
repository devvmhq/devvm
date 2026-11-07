---
title: .git protection
explainer: Keep code in the VM out of your .git
weight: 2
---

The project folder is read-write in the VM, so without protection anything in the VM could add a git hook, or set `core.fsmonitor`, `core.hooksPath` or `core.sshCommand` in `.git/config`, and your next git command on this machine would run it. On macOS, devvm locks the git hooks and config with a flag that even root in the VM can't clear, so nothing the VM does runs through them on this machine.

Every new VM gets `.git` protection. `--no-git-protect` creates a VM without it, and [`devvm git-protect`](#turning-it-on-or-off) changes it later. [How it works › .git protection](../internals/git-protection.md) lists what devvm locks and when it checks the locks again.

## What still works

git in the VM can commit, branch, check out, stash, fetch and pull, and `git worktree add` works on this machine. Commands that write `.git/config` fail with `Operation not permitted`, in the VM and on this machine. Those are `git config`, `git remote add`, `push -u` to a new branch, and hook installers such as husky or pre-commit. `git worktree remove` and `prune` also fail for the worktrees that were there when devvm locked `.git`.

To run them, stop the VM, unlock `.git`, run the commands on this machine, and lock it again:

```sh
devvm stop
devvm git-protect off
git config …
devvm git-protect on
```

## Turning it on or off

`devvm git-protect` shows the VM's protection, which is `on`, `strict`, `off`, or `clone` for a [`--clone`](../using/clone.md) VM, which mounts no `.git`. `devvm git-protect on`, `strict` or `off` changes it at once when the VM is stopped. A running VM could race the locks, so for a running VM devvm saves your choice and asks in a terminal whether to restart the VM now, which stops anything running in it. If you answer no, or there's no terminal, or you pass `--no-input`, devvm says it saved the choice. It applies the choice the next time it finds the VM stopped: at the restart you agreed to, at `devvm stop`, at `devvm restart`, or, when the VM was stopped some other way, at the next launch, before the VM starts. Until then, `devvm git-protect` shows the protection the VM has, and a later choice replaces the saved one. Launches lock only a stopped VM for the same reason.

- `on` locks `.git` as it is now and takes it as yours, so a `.git` that was replaced since the last lock isn't reported, and the next launches compare with this one. It reports what [a lock can't stop](../internals/git-protection.md#what-a-lock-cant-stop) and what's [not covered](#not-covered), as devvm did when it made the VM. If the folder has no `.git` yet, devvm locks it on the first launch after one appears.
- `strict` is `on` plus [strict](#strict).
- `off` takes the locks off. The VM can then change `.git` hooks and config, which run on your machine.

On an existing VM, devvm ignores `--no-git-protect` and warns you. A `--clone` VM's protection can't be changed. `devvm clone on` takes the locks off once it has made the VM again with its own clone, and `devvm clone off` makes it mount the folder again with protection as a new VM gets it ([switching a VM that exists](../using/clone.md#switching-a-vm-that-exists)).

### devvm git-protect --json

`devvm git-protect --json` prints the VM's protection as one JSON object with `mode`, which is `on`, `strict`, `off` or `clone`, as [output for scripts](../using/command-line.md#output-for-scripts) describes. With `--json` the command only shows, so devvm refuses `on`, `strict` or `off` with it.

```json
{
  "version": 1,
  "mode": "on"
}
```

## Strict

The VM can add a submodule at any time, and `git status` and `git diff` on this machine read the submodule's config. So the submodule's `core.fsmonitor` would run on this machine before the VM's next launch locks it. `devvm git-protect strict` closes that gap by setting `diff.ignoreSubmodules = all` in the locked `.git/config`. `git status` and `git diff` on this machine then never look inside submodules, and they don't show the submodules' changes. `git submodule …` and `--recurse-submodules` still enter them, so for a repo with submodules you don't trust, use [`--clone`](../using/clone.md).

`devvm git-protect on` and `off` take `diff.ignoreSubmodules` back out.

## When devvm reports .git

Every launch of an existing VM, and `devvm stop`, check `.git` again, and devvm reports what it finds there that it didn't lock. When it names one of these, check it before you run git in the folder, because git on this machine would run what the VM put there:

- **A repo that appeared** in the folder after its VM was made. devvm locks it from then on, but if you didn't make it, check its hooks and config.
- **Hooks in a folder devvm hadn't locked,** because the folder wasn't there to lock or belongs to a new submodule.
- **A path that's a symlink or under one.** devvm never locks anything through a symlink, because the VM could point one at a file elsewhere on this machine, so it leaves that path unlocked.
- **A path that moved while devvm locked it.** devvm leaves it unlocked and takes the lock off wherever it landed, so check `.git`.

[How it works › .git protection](../internals/git-protection.md#on-each-launch) lists every check.

## Not covered

The lock covers `.git`, and the VM can still put things elsewhere that git on this machine runs. devvm tells you about each of these, so read what it reports before you run git in the folder, and use [`--clone`](../using/clone.md) for a repo you don't trust.

- **Hooks in the working tree**, such as husky's `.husky/` with `core.hooksPath=.husky`, or an absolute path into the project folder. The VM can edit them, and they run on this machine. devvm warns about them when it creates the VM.
- **What the locked files hand over to the project folder**, which is an `include.path` or `includeIf.*.path` that leads into it, or a hook that's a symlink into it. The VM can edit those files, and git on this machine reads or runs them. devvm warns about them when it creates the VM, and on a launch after they change.
- **Submodules whose git folder is outside the project.** devvm can't lock that folder, because another project's VM may write it, and `git status` on this machine reads its config. devvm names these submodules when it creates the VM.
- **A replaced `.git`.** The VM can rename `.git` and put a new one in its place, or point git at another config. devvm reports this on each launch but doesn't undo it ([what a lock can't stop](../internals/git-protection.md#what-a-lock-cant-stop)).

## Linux hosts

Linux has no immutable flag that a user can set, so VMs on Linux hosts have no `.git` protection, and the VM can change `.git` hooks and config, which run on the host. devvm says so when it creates a VM for a folder with a `.git`, and it refuses `devvm git-protect on` and `strict`. [`--clone`](../using/clone.md) keeps the VM out of `.git` instead.
