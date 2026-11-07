---
title: Upgrading and uninstalling
weight: 16
---

This page covers upgrading devvm, switching between Homebrew and the install script, and removing devvm with everything it keeps. Each depends on how you [installed devvm](../getting-started/install.md), and your images, VMs and settings stay through an upgrade or a switch, because they don't live with devvm. [Installing and updating](../internals/install.md) explains what the install script and devvm upgrade check.

## Upgrading

To upgrade devvm, run `devvm upgrade`, which installs the newest release the way you installed devvm:

```sh
devvm upgrade                 # install the newest release, asking first
devvm upgrade --check         # say whether a newer release is out, and install nothing
devvm upgrade --force         # install it without asking, as a script must
devvm upgrade --to 0.1.0      # install that release, even one older than the devvm you have
devvm upgrade --to 0.2.0-rc.1 # install a prerelease, which only --to installs
```

It says which release it will install in place of the devvm you have, and asks first, so without a terminal or with `--no-input` it needs `--force`.

You don't have to remember to check. When you run devvm in a terminal, it checks at most once a day for a newer release without making you wait, and when one is out it says so in one line that names devvm upgrade. It never upgrades on its own, and `release-check = false` in `config.toml` or `DEVVM_NO_RELEASE_CHECK=1` turns the check off ([the release check](settings.md#the-release-check)).

You can also upgrade devvm without devvm upgrade, with the command for the way you installed it:

{{< os-tabs >}}
{{< os-tab name="macOS" >}}

```sh
brew upgrade devvm
```

If you installed devvm with the install script instead, run the script again, as on Linux.

{{< /os-tab >}}
{{< os-tab name="Linux" >}}

```sh
curl -fsSL https://github.com/devvmhq/devvm/releases/latest/download/install.sh | bash
```

Running the install script again installs the newest release.

{{< /os-tab >}}
{{< /os-tabs >}}

Your images and VMs stay, because they live in `~/.local/share/devvm`, and your settings and layers stay in `~/.config/devvm`, apart from devvm itself.

Images whose inputs changed are built again the next time you use them. Existing VMs keep working, and `devvm tools update` moves a VM to the new shared tools. `devvm gc` deletes the copies of devvm's files that older versions wrote out, except those that a VM's proxy was started with.

### How devvm upgrade upgrades

devvm upgrade tells how devvm was installed from where the devvm you ran is, and it never runs anything to find out:

- **With the install script**, devvm is `~/.local/lib/devvm/<version>/devvm`, and `~/.local/bin/devvm` links to it, or the same folders under `DEVVM_PREFIX`. devvm upgrade downloads the new release and checks it, as [What devvm upgrade checks](#what-devvm-upgrade-checks) describes. It unpacks the release into a folder of its own beside the devvm you have, runs the new devvm to see that it works, and then switches `~/.local/bin/devvm` to it. When anything fails before the new devvm runs through `~/.local/bin/devvm`, the link goes back, so the devvm you had stays the one that runs. The version you upgraded from stays in `~/.local/lib/devvm`, and so does any older version that a running VM still uses, until a later upgrade finds nothing using it.
- **With Homebrew**, devvm is in Homebrew's Cellar, and devvm upgrade runs `brew upgrade devvm` after asking. Homebrew may upgrade Lima at the same time, because the formula needs it. Homebrew installs the release its formula names, which can come out a little after the release itself, so until then `brew upgrade devvm` finds nothing to upgrade.
- **Any other way**, such as `go install`, a build from devvm's source, or a copy of devvm somewhere else, devvm upgrade can't upgrade. It says how that devvm was installed and how to upgrade it, such as with `go install github.com/devvmhq/devvm/cmd/devvm@latest`, and fails.

A devvm built without devvm's release key can't check a release's signature, so devvm upgrade refuses to install a release with it and says to run the install script again.

### What devvm upgrade checks

devvm upgrade downloads over HTTPS only, and checks each file before it uses the next:

1. It downloads the release's `SHA256SUMS` and its signature, `SHA256SUMS.minisig`, and checks that devvm's release key signed it for that release's version. The key is built into the devvm you have, so a replaced release can't bring its own.
2. It downloads the archive for this machine and checks it against its checksum in `SHA256SUMS`.
3. It unpacks the archive, which must hold only devvm's files in one folder.
4. It runs the new devvm with `--version`, which must say the release's version.

When a check fails, devvm upgrade installs nothing, and the devvm you have stays the one that runs. It checks the signature itself, so it doesn't need minisign on this machine, as the install script does. It never runs a script that it downloaded, only the new devvm's `--version`.

### An older release

When the newest release is older than the devvm you have, devvm upgrade refuses to install it, even with `--force`. A server that offers an old release in place of the newest could bring back problems that a newer release fixed, so devvm upgrade never goes back a version unless you name it. `devvm upgrade --to VERSION` installs the release you name, newer or older, and it asks first, saying when the release is older than the devvm you have. When the devvm you have is a prerelease of a release that isn't out yet, the newest release is older too, and devvm upgrade says so and installs nothing, without failing ([trying a prerelease](#trying-a-prerelease)).

Homebrew installs only its formula's release, so with Homebrew devvm upgrade doesn't take `--to`.

### Trying a prerelease

A prerelease is a version of devvm that you can try before its release is out, such as a release candidate, `0.2.0-rc.1`, or a beta, `0.2.0-beta.1`. devvm upgrade and the install script never pick a prerelease as the newest release, and the daily release check never announces one, so you get a prerelease only when you name it.

The docs at [devvm.dev](https://devvm.dev/) are for the newest release. The docs for the next release, which a prerelease comes before, are at [next.devvm.dev](https://next.devvm.dev/), and they change as that release is written, so they can describe more than a prerelease has.

To install a prerelease with the install script, run the copy of the script that comes with the prerelease, which installs that prerelease, or give its version to the newest release's script with `--version`:

```sh
curl -fsSL https://github.com/devvmhq/devvm/releases/download/v0.2.0-rc.1/install.sh | bash
curl -fsSL https://github.com/devvmhq/devvm/releases/latest/download/install.sh | bash -s -- --version 0.2.0-rc.1
```

When the install script installed the devvm you have, `devvm upgrade --to 0.2.0-rc.1` installs the prerelease too. Both check a prerelease's signature and checksum as they check a release's. Homebrew's formula always names the newest release, so to try a prerelease on a Homebrew install, switch to the install script first, as [one devvm](#one-devvm) describes.

Versions go in this order: a prerelease comes before its release, a beta before a release candidate, and the numbers in order, so `0.2.0-beta.3` comes before `0.2.0-rc.1`, then `0.2.0-rc.2`, then `0.2.0`. While you run a prerelease, devvm upgrade offers the release it comes before once that release is out, and the daily check says so. It never offers a newer prerelease, such as `0.2.0-rc.2`, so install one with `devvm upgrade --to 0.2.0-rc.2`.

To go back to the newest release before the prerelease's own release is out, name the newest release's version, such as `devvm upgrade --to 0.1.0`. devvm upgrade asks first, because that release is older than the prerelease you have.

### devvm upgrade --json

`devvm upgrade --json` prints what `devvm upgrade --check` says as one JSON object, for scripts, as [output for scripts](command-line.md#output-for-scripts) describes, and installs nothing:

- `current` is the version of the devvm you ran.
- `latest` is the newest release's version, which is never a prerelease's.
- `newer` says whether that's newer than `current`.
- `install` is how devvm was installed, `install.sh`, `homebrew` or `other`.
- `path` is where the devvm you ran is, its links followed.

```json
{
  "version": 1,
  "current": "0.1.0",
  "latest": "0.2.0",
  "newer": true,
  "install": "install.sh",
  "path": "/Users/you/.local/lib/devvm/0.1.0/devvm"
}
```

## One devvm

Install devvm one way, with Homebrew or with the install script, but not both. With two copies, the one that comes first on your `PATH` is the one that runs, and `brew upgrade devvm` may update the one you don't run. So when Homebrew installed devvm, the install script stops before it downloads anything and says to run `brew upgrade devvm` instead, or `brew uninstall devvm` first if you want to use the script. It stops whether or not it runs in a terminal, and its `--force` installs a second copy anyway.

To switch from one way to the other, remove devvm with the command for the way you installed it, as step 5 of [Uninstalling](#uninstalling) shows, and then install it the other way. Your images, VMs and settings stay, because they don't live with devvm.

After it installs devvm, the script warns when another devvm comes before `~/.local/bin` on your `PATH`. The warning names that copy's path and says to remove it, or to put `~/.local/bin` before its folder in `PATH`. `devvm doctor` warns the same way when the devvm first on your `PATH` isn't the one you ran, naming both, and the warning doesn't make it fail.

## Uninstalling

1. Delete your VMs: `devvm ls`, then `devvm rm` in each folder. For a `--clone` VM, this also removes the `devvm` git remote from the repo.
2. Delete the images: `devvm gc`, then `devvm images rm` with every name `devvm images` lists. Images built `--from` a container image need the same `--from`: `devvm images rm --from node:24 base`.
3. Free Lima's cache. In a terminal, `devvm gc --caches` frees devvm's share of it, after asking. If you don't use Lima yourself, you can also empty its cache: Lima keeps Ubuntu's cloud images in `~/Library/Caches/lima` on macOS and `~/.cache/lima` on Linux, and `limactl prune` deletes that whole cache.
4. Delete your saved secrets: `devvm secret rm NAME` for each one `devvm secret` lists. On macOS they're in the Keychain under the service `devvm`, and on Linux in `~/.local/share/devvm/secrets/`.
5. Remove devvm itself, with the command for the way you installed it:

   {{< os-tabs >}}
   {{< os-tab name="macOS" >}}

   ```sh
   brew uninstall devvm
   ```

   If you installed devvm with the install script instead, remove it as on Linux.

   {{< /os-tab >}}
   {{< os-tab name="Linux" >}}

   ```sh
   rm -rf ~/.local/lib/devvm ~/.local/bin/devvm
   ```

   {{< /os-tab >}}
   {{< /os-tabs >}}

6. Remove its state: `rm -rf ~/.local/share/devvm ~/.config/devvm`. That includes your layers, in `~/.config/devvm/layers`, so copy them somewhere first to keep them. It also includes your exports, the agents' history that `devvm rm` saved, and devvm's agent store, the agents' settings in `~/.config/devvm/agents` and their logins in `~/.local/share/devvm/logins`. On macOS, the Time Machine exclusions that devvm set on its CA folders and logins folders go with the folders.
7. If you let devvm add it, remove the `Include ~/.local/share/devvm/ssh_config` line from `~/.ssh/config`.
