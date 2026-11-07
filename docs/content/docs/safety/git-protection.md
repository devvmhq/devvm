---
title: .git protection
explainer: Keep code in the VM out of your .git
weight: 2
---

The project folder is read-write in the VM, so without protection anything in the VM could add a git hook, or set `core.fsmonitor`, `core.hooksPath` or `core.sshCommand` in `.git/config`, and your next git command on this machine would run it. devvm locks the git hooks and config with a flag that even root in the VM can't clear, so nothing the VM does runs through them on this machine. On Linux, devvm sets that flag through a small program of its own, devvm-immutable, which you install once with sudo, as [Linux hosts](#linux-hosts) describes.

Every new VM gets `.git` protection, on Linux once devvm-immutable is installed. `--no-git-protect` creates a VM without it, and [`devvm git-protect`](#turning-it-on-or-off) changes it later. [How it works › .git protection](../internals/git-protection.md) lists what devvm locks and when it checks the locks again.

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
- **A submodule whose git folder isn't one.** The VM can name any folder in the project as a submodule's git folder, such as your `src`, so devvm locks nothing in a folder that doesn't look like a git folder, and names the submodule instead.
- **A path that's a symlink or under one.** devvm never locks anything through a symlink, because the VM could point one at a file elsewhere on this machine, so it leaves that path unlocked.
- **A path that moved while devvm locked it.** devvm leaves it unlocked and takes the lock off wherever it landed, so check `.git`.
- **A path that devvm couldn't lock,** at every lock until it can. That's a config that isn't a regular file, or hooks that aren't a folder, and on Linux also a file with hard links, which a lock would lock under its other names too, a file you can't read, which Linux has to open to lock, and a hooks folder past devvm-immutable's bounds. devvm never rewrites such a file to lock it, so check it and replace it yourself, for example with a copy of it.

When the VM renames `.git` and makes another in its place, devvm names the new one as replaced. The old `.git` keeps its locks where it went, and `devvm git-protect off` or `devvm rm` takes them off when it's at the top of the project folder, such as `.git-old`. Elsewhere, unlock it yourself: `chflags -R nouchg PATH` on macOS, or `/usr/local/libexec/devvm/devvm-immutable clear PATH` on Linux.

[How it works › .git protection](../internals/git-protection.md#on-each-launch) lists every check.

## Not covered

The lock covers `.git`, and the VM can still put things elsewhere that git on this machine runs. devvm tells you about each of these, so read what it reports before you run git in the folder, and use [`--clone`](../using/clone.md) for a repo you don't trust.

- **Hooks in the working tree**, such as husky's `.husky/` with `core.hooksPath=.husky`, or an absolute path into the project folder. The VM can edit them, and they run on this machine. devvm warns about them when it creates the VM.
- **What the locked files hand over to the project folder**, which is an `include.path` or `includeIf.*.path` that leads into it, or a hook that's a symlink into it. The VM can edit those files, and git on this machine reads or runs them. devvm warns about them when it creates the VM, and on a launch after they change.
- **Submodules whose git folder is outside the project.** devvm can't lock that folder, because another project's VM may write it, and `git status` on this machine reads its config. devvm names these submodules when it creates the VM.
- **A replaced `.git`.** The VM can rename `.git` and put a new one in its place, or point git at another config. devvm reports this on each launch but doesn't undo it ([what a lock can't stop](../internals/git-protection.md#what-a-lock-cant-stop)).

## Linux hosts

On Linux, devvm locks `.git` with the immutable attribute, the flag that `chattr +i` sets. Nothing in the VM can clear it, not even root, because the VM reaches the project folder only through QEMU, which runs as you and can't clear the attribute either. Setting the attribute needs the `CAP_LINUX_IMMUTABLE` capability, which a program you run doesn't have, so devvm sets it through devvm-immutable, a small program that has that one capability. With devvm-immutable installed, `.git` protection on Linux works as it does on macOS, and every new VM gets it. Without it, devvm can't stop the VM from changing `.git`, so it warns at each launch and names the changes afterwards, as [without devvm-immutable](#without-devvm-immutable) describes.

### Installing devvm-immutable

devvm-immutable comes with devvm, but it needs sudo to install, because it has to be root's and to get its capability. On Linux, the [install script](../getting-started/install.md) offers to install it and asks first. `--yes` installs it without asking, and `--no-deps` skips it. `devvm doctor` says whether devvm-immutable works, and when it doesn't, it prints the two commands that install the copy that came with devvm, such as:

```sh
sudo install -D -o root -g root -m 0755 ~/.local/lib/devvm/0.2.0/devvm-immutable /usr/local/libexec/devvm/devvm-immutable
sudo setcap cap_linux_immutable+ep /usr/local/libexec/devvm/devvm-immutable
```

The first command copies it into `/usr/local/libexec/devvm`, owned by root, so that only root can replace it, and the second gives it its capability. `setcap` comes with libcap, which Debian and Ubuntu package as `libcap2-bin`. Homebrew installs devvm-immutable with devvm, in the formula's `libexec` folder, but Homebrew can't give it a capability, so run the commands that `devvm doctor` prints, which copy Homebrew's. When an upgrade brings a devvm-immutable that devvm needs, `devvm doctor` says it doesn't work and prints the commands again, and the install script offers to install it again.

On a machine that others share, you can let only your own group run it: give `install` `-g GROUP -m 0750` in place of `-g root -m 0755`, as `devvm doctor` shows with your group's name. Then other users' programs, a service's account included, can't run it to lock or unlock their own files ([what it can do](#what-devvm-immutable-can-do)).

devvm runs devvm-immutable only when root alone can change it: the file and every folder above it must be root's, and none of them writable by its group or others. Otherwise someone else could put a program of theirs in its place. Some Debian systems make `/usr/local` writable by the `staff` group (`root:staff 2775`), and there devvm refuses devvm-immutable, `devvm doctor` names the folder, and the install script doesn't install it. Members of `staff` can already change what runs from `/usr/local/bin`, so to use devvm-immutable there, take the group's write off the folders above it (`sudo chmod g-w /usr/local /usr/local/libexec`), if nothing on the machine needs it.

When devvm-immutable is installed but doesn't work, `devvm doctor` says why. It has no capability when `setcap` didn't run, when the file system that holds it is mounted with `nosuid`, or when devvm runs in a sandbox that sets `no_new_privs`, which drops file capabilities. A new VM that devvm would then make without `.git` protection isn't made until you agree to that: in a terminal, devvm asks first, and elsewhere it stops and names `--no-git-protect`, which makes the VM without protection.

### What devvm-immutable can do

devvm-immutable sets or clears the immutable attribute and changes nothing else. It never changes a file's contents, owner or mode, and it writes the file's other attributes back as they were, as `chattr` does. It acts only on regular files and folders that the user who runs it owns, and it never acts through a symlink, so it can't lock a file elsewhere for a symlink that the VM planted under `.git`. It reaches each path from `/` one folder at a time and refuses any path with a symlink in it. It doesn't lock a file that has other names, which are hard links, because one of them could be outside the project folder. Inside a folder, it skips symlinks and doesn't enter another file system, and it stops after 100,000 files and folders. It gives up every other capability as it starts, takes nothing from its environment, and runs no other program.

Any program on this machine can run devvm-immutable, as whichever user it runs as, to lock or unlock that user's own files, and nobody else's. So a program that runs as you can clear the attribute on a file of yours that you locked with sudo, such as `~/.ssh/authorized_keys`, which it couldn't do without sudo before. On a shared machine, each user, a service's account included, can clear the attribute on their own files where an administrator set it so that they can't change them, and can set it on their own files, which root then has to clear before it can delete them. So on a shared machine, devvm-immutable loosens that policy slightly.

### Removing devvm-immutable

Without devvm-immutable, devvm can't unlock `.git`, and git on this machine can't write a locked `.git/config`. So before you remove it, run `devvm git-protect off` in each VM's folder, or delete the VMs with `devvm rm`. Then delete it:

```sh
sudo rm /usr/local/libexec/devvm/devvm-immutable
sudo rmdir /usr/local/libexec/devvm
```

If a folder's `.git` is still locked after that, devvm names what stays locked, and `sudo chattr -R -i` on each path it names unlocks it.

### Without devvm-immutable

Without devvm-immutable, or with a project folder on a file system that has no immutable attribute, such as NFS or a FUSE file system, devvm can't lock `.git`. The VM can then change `.git` hooks and config, which run on the host. devvm refuses `devvm git-protect on` and `strict` there and says why, and the VMs it makes there get `off`. [`--clone`](../using/clone.md) keeps the VM out of `.git` instead, so use it for any repo where you don't trust what runs in the VM.

#### The warning at each launch

Each launch of a VM that mounts a folder with a `.git` warns that the VM can change its hooks and config, which run on this machine, and says why devvm can't lock them: devvm-immutable isn't installed, it doesn't work, or the folder's file system has no immutable attribute. That includes the launch that creates the VM, and launches of a VM that's already running. Once you've read the warning and decided to go on without protection, run `devvm git-protect off` in the folder. devvm records that you chose it and stops warning for that VM, but it still names the changes that the next section describes. A VM that you create with `--no-git-protect` doesn't warn either, and a `--clone` VM never warns, because it mounts no `.git`.

#### When devvm names a change

devvm can't stop the VM from changing `.git` without devvm-immutable, but it notices the change afterwards. When devvm creates a VM, it records a hash of each path that a lock would cover, which are the hooks in `.git/hooks`, `.git/config` and the rest of [what's locked](../internals/git-protection.md#whats-locked), submodules' included. It also hashes `.git/commondir`, whether it's there or not, and what the config hands over to the project folder: the files that an `include.path` leads to, and the hooks folder that `core.hooksPath` names, such as husky's `.husky`. Each launch that finds the VM stopped, and `devvm stop`, hash them again and name every path that changed since the last check, such as `.git/config` and `.git/hooks/pre-commit`. A warning names ten paths at most, and then how many more changed. The same checks report what a lock can't stop either, as they do on macOS: a `.git` that was replaced, a `commondir`, a repo or submodule that appeared, and includes, hook links and a `core.hooksPath` into the project folder ([not covered](#not-covered)). A launch of a VM that's already running checks nothing, so run `devvm stop` when you want the check. A VM that devvm has no hashes for yet, such as one that an older devvm made, records them at its first check and names nothing.

devvm names a change only after it happened, so git on this machine may already have run a changed hook or read a changed config. devvm can't tell who made a change, so it also names your own, such as a `git remote add` or `git push -u` that you ran on this machine. When it names a path that you didn't change, check it before you run git in the folder:

- `git config --list --show-origin` shows each setting of the repo and the file it comes from. Look for settings that run a program, such as `core.fsmonitor`, `core.hooksPath`, `core.sshCommand`, `core.pager`, `filter.*` and `include.path`.
- `ls -la .git/hooks` lists the hooks, and git runs a hook there by its name, such as `pre-commit`, so read every file whose name doesn't end in `.sample`.

If you find something that you didn't put there, remove it, and use [`--clone`](../using/clone.md) for that repo.

#### What the watch can't see

The watch isn't a security boundary, and it doesn't stop anything. It compares what's in `.git` when the VM is stopped with what was there at the last check, so it misses a change that the VM made and then undid before it stopped, even though git on this machine may have run the change in between. It also can't fully watch some paths, and it names them at every check until they're gone:

- **A symlink, or a path under one,** such as a hook that's a symlink, or a `.git` that's a symlink. devvm never follows a symlink out of the project folder, so it can't see changes to what such a link points to. For a link that stays inside the project folder, it hashes what the link leads to as well.
- **A file larger than 16 MiB,** which devvm records by its size, inode and change time, so it notices a change that keeps the size but can't hash the file.
- **A hooks folder with more than 1,000 entries,** which devvm records by its count and change time. It always hashes the hooks that git runs, at the folder's top, before the rest.
- **What's past 2,000 paths in all,** across every git folder and hooks folder of the repo.

A repo rarely has any of these, so when devvm names one that you didn't make, check what it holds, and use [`--clone`](../using/clone.md) for that repo.

### VMs made without it

A VM that devvm made without `.git` protection, and where you didn't choose that, keeps `off` once devvm can lock, so that devvm never locks a `.git` you didn't ask it to lock. Each launch of such a VM says that its `.git` protection is off and can be turned on, and devvm keeps naming the changes as before. Run `devvm git-protect on` in its folder to lock `.git` as it is then, after you've checked it as [when devvm names a change](#when-devvm-names-a-change) describes, or run `devvm git-protect off` to keep it unlocked and stop the warning.
