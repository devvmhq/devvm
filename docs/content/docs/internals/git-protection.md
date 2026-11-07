---
title: .git protection
explainer: What devvm locks in .git, and when
weight: 6
---

This page explains what devvm locks in a project's git folder, when it locks and checks the locks again, and what the locks can't stop. [.git protection](../safety/git-protection.md) covers what still works, turning protection on or off, strict, and what isn't covered.

On macOS devvm locks with the user-immutable flag (`chflags uchg`), which even root in the VM can't clear. A read-only mount wouldn't hold, because root in the VM can unmount it.

## What's locked

devvm locks these paths:

- `.git/hooks` and `.git/config`. A repo without a `hooks` folder gets an empty one, locked, so the VM can't make one.
- The same paths for each submodule devvm finds, submodules of submodules included. Those are the git folders under `.git/modules`, and those in the working tree, which are `sub/.git` or the folder that a `sub/.git` file points to. devvm finds the submodules from the index, which it reads with `core.fsmonitor` off.
- `.git/config.worktree`, when `extensions.worktreeConfig` is on. devvm creates it empty if it's missing.
- What linked worktrees elsewhere on your machine read from here, which is `.git/worktrees/<name>/commondir` and `config.worktree`.
- The project folder's `.git` file, if the project folder is itself a linked worktree. The repo it points to is outside the VM.

### When devvm locks

devvm records a new VM's mode in its state before its backend makes the VM, so the mode is never unset: on with locks (macOS), off when you pass `--no-git-protect` or on hosts without locks, and `clone` for a `--clone` VM, which mounts no `.git`. devvm locks once the backend has made the VM, and again on each launch that finds the VM stopped and on `devvm stop`, so nothing in the VM can race the locks. A VM whose creation stopped before its first locks, or that `devvm adopt` moved to this folder, is locked on its next launch as a new VM is, without reports. A running VM keeps the locks it had, and `devvm stop` or the next launch that finds the VM stopped, such as `devvm restart`, sets new ones. `devvm git-protect` on a running VM saves the mode in the VM's state (the `git` section's `next`).

Each time it locks, devvm records in the VM's state what it locked and found: the identity of `.git` (its device and inode, and its birth time where the file system keeps one), every git folder it locked besides `.git` (those under `.git/modules` and the submodules' in the working tree, in one list), and a hash of what the locked files hand over to the project folder. The next check compares with those, so it reports what changed since. The same places first check `.git` under the mode the VM ran with, so a `.git` that the VM replaced meanwhile is still named (`git_replaced`), and then set the new mode, which locks `.git` as it is then.

A new VM that's interrupted before its first shell is deleted, and devvm takes its locks off. When `devvm rm` deletes a VM, devvm also takes its locks off. It unlocks what it would lock now and every git folder the VM's state records locking, so a submodule whose entry the VM dropped from the index since is unlocked too.

### Symlinks

devvm never locks or creates anything through a symlink, because the VM could plant one under `.git` to make devvm lock a file elsewhere. devvm reports a path that is a symlink, or is under one, instead of locking it (`git_not_locked`). devvm also checks each lock again once it's set. If the path moved in the meantime, devvm unlocks it where it landed and reports it (`git_lock_raced`).

## On each launch

Every launch of an existing VM, every export of its agents' history, and `devvm stop` check `.git` again. devvm reports each of these once:

- A `.git` that was replaced since devvm locked it (`git_replaced`). devvm then does nothing else, so check the new `.git`, and look for the old one, renamed.
- A repo that appeared in the folder after the VM was made (`git_appeared`), which devvm then locks.
- A submodule git folder that appeared, in the working tree or under `.git/modules` (`git_embedded_new`), which devvm then locks. devvm compares with the git folders it recorded last time, so `devvm git-protect on` after a VM was made without protection names none: it locks `.git` as it is then, as trusted.
- Hooks in a hooks folder that wasn't locked, because it wasn't there to lock or belongs to a new submodule (`git_hooks_unlocked`).

devvm also reports these, the first two on every launch and the last when they change:

- A `commondir` (`git_commondir`).
- Linked worktrees whose `commondir` leads elsewhere (`git_worktree_stray`).
- Includes and hook links into the project folder (`git_include_inside`, `git_hook_link_inside`), which [not covered](../safety/git-protection.md#not-covered) describes.

## What a lock can't stop

The VM can rename `.git` and put a new one in its place, add `.git/commondir` to point git at another config, or do the same with a linked worktree's folder in `.git/worktrees`. When devvm creates the VM, and [on each launch](#on-each-launch), it reports a `commondir` that's there, linked worktrees whose `commondir` leads somewhere else, and paths it couldn't lock because they're symlinks. devvm doesn't repair anything, so check what it names before you run git in the folder. When you don't trust what runs in the VM, use [`--clone`](../using/clone.md), which mounts no `.git` at all.

The VM writes `.git/index`, because it has to in order to commit, so it can also add a submodule with its own config, which `git status` on the host reads. A submodule's `core.fsmonitor` therefore also runs on the host. devvm names a new submodule on the next launch, but git on the host reads it before that, unless the VM is [strict](../safety/git-protection.md#strict).

devvm's own git commands on the host run no program that a repo's config or `.git` names, a submodule's included, whatever the VM wrote there. devvm runs each one with these settings, given as `GIT_CONFIG_COUNT` pairs, which git takes over any config file:

- `core.fsmonitor=false`, `core.hooksPath=/dev/null` and `core.alternateRefsCommand=true`, so git runs no fsmonitor, no hook from a hooks folder and no command for an alternate object store's refs.
- `hook.EVENT.enabled=false` for every event githooks(5) lists, and `hook.NAME.enabled=false` for each hook the repo's config names, so git 2.54 and newer run no hook that a config defines.
- `filter.NAME.clean`, `smudge` and `process` emptied, and `filter.NAME.required=false`, for each filter driver the repo's config names, so git hashes files without running a filter and doesn't fail on a required one.
- `protocol.allow=never` and `protocol.ssh.allow=always`, so git reaches nothing but ssh, not a path, `file://`, `ext::`, `git://` or `http`. A repo's config could still allow one protocol by its own key, such as `protocol.ext.allow=always`, which git takes over `protocol.allow`, so devvm also sets `GIT_ALLOW_PROTOCOL=ssh`, which git takes over every `protocol` setting.
- `maintenance.auto=false` and `gc.auto=0`, so a fetch starts no maintenance or gc.

Its ssh is `false`, which reaches nothing, in place of a repo's `core.sshCommand` (`GIT_SSH_COMMAND=false`, with `GIT_SSH_VARIANT=ssh` over a repo's `ssh.variant`), unless the command is a `--clone` VM's push or fetch, and it fetches nothing lazily (`GIT_NO_LAZY_FETCH=1`), so a repo that says it's a partial clone can't make `git rev-list` or `git status` connect anywhere for an object it lacks.

It runs without any `GIT_*` variable of devvm's own environment, such as `GIT_DIR`, `GIT_WORK_TREE` or `GIT_CONFIG_GLOBAL`, so a hook or a rebase you run devvm from can't point it at another repo or config, and it reads your global config from its usual place. It keeps only the absolute folders of `PATH`, because git looks programs up, ssh among them, from the repo's folder, and it runs with `GIT_TERMINAL_PROMPT=0`. Its `git status` adds `--no-optional-locks`, so it writes no index, and `--ignore-submodules=all`, so it never enters a submodule, whose own config could name filters of its own.

A `--clone` VM's push adds `--no-verify`, `--no-recurse-submodules` and `--no-signed`, so no signing program runs, and its fetch adds `--no-recurse-submodules` and an empty `--refmap=`, so it updates only the branches its own refspecs name, never those of a remote that the repo's config names after the clone's address. Both run git with devvm's own `GIT_SSH_COMMAND`, which ignores the host, port and options git passes to ssh. It always connects to the VM's own address and port, the host's loopback port that the VM's backend gives, as the VM's user with the VM's own key, and it reads no ssh config (`-F none`). It checks the host keys devvm pinned for the VM's current start, with the VM's own known hosts file and no global one, `-o HostKeyAlias=` the VM's name and `-o StrictHostKeyChecking=yes`, and it runs with `-o BatchMode=yes`, `-o ForwardAgent=no` and `-o ClearAllForwardings=yes`. In the VM it runs only `git-receive-pack` or `git-upload-pack`, whichever git asks for, on the clone's path, and it refuses any other command. So no `url.*.insteadOf`, `pushInsteadOf` or remote URL in the repo's config can send the push or the fetch to another host or to another path, and they forward neither your agent nor any port. A repo's `core.sshCommand` doesn't apply, because devvm sets `GIT_SSH_COMMAND`, and its `diff.external`, textconv drivers and pager never run, because none of devvm's commands shows a diff or writes to a terminal.

Before a `--clone` VM is made, and again before its push, devvm checks that what git reads in the folder is the repo's own, so the push can't copy another repo into the VM. The folder's `.git` is a folder with no `commondir`, or it's the `.git` file of a linked worktree whose link git made both ways. That `.git` file names `<repo>/.git/worktrees/<name>`, that entry's `gitdir` file names the folder's `.git` back, and its `commondir` leads to that `<repo>/.git`, which has no `commondir` of its own. Neither the git dir nor the common dir has `objects/info/alternates`, or a `packed-refs` or `reftable` that's a symlink, or anything in `objects` or `refs` that's a symlink, such as a pack folder or a loose object's folder that leads to another repo's objects. Then `git rev-parse --absolute-git-dir --git-common-dir` must name the same two folders. A worktree's push sends only its branch and that branch's upstream, when the upstream is one ref, so the repo's other branches stay on this machine. A `branch.<name>.merge` that's a pattern, such as `refs/heads/*`, would make the upstream push every branch, so devvm then pushes only the branch.

devvm finds the hook and filter names to turn off with one `git config` before each command, so a VM that keeps swapping the repo's config could show that `git config` one config and the command another. Every hook event is turned off by its name as well, which covers config hooks on git that has the event setting, so the gap is a filter that the swap hides, which `git status` could then run on a file whose stat changed. It needs a VM that writes the repo's `.git`, so on macOS, where `.git/config` is locked, it needs a repo devvm doesn't lock, such as one nested in a parent folder's mount.

Two things a repo's config can still do, and neither runs code. An `include.path` can make devvm's `git config --get user.name` read your name and email from another file in git's config format, which then go into the VM as its git identity. A config file or included file that is a FIFO makes git, and so devvm, wait until you press Ctrl+C.
