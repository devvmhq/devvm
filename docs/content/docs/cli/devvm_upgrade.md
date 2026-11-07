---
title: devvm upgrade
weight: 64
---

Install devvm's newest release

## Synopsis

Install devvm's newest release the way this devvm was installed, after asking. The install script's devvm gets the release once its signature and checksum check out, and Homebrew's runs brew upgrade devvm. --check only says whether a newer release is out.

```text
devvm upgrade [flags]
```

## Examples

```text
  devvm upgrade                  # install the newest release, asking first
  devvm upgrade --check          # say whether a newer release is out
  devvm upgrade --force          # install it without asking, as a script must
  devvm upgrade --to 0.1.0       # install that release, even an older one
  devvm upgrade --to 0.2.0-rc.1  # install a prerelease, which only --to installs
```

## Options

```text
      --check        say whether a newer release is out, and install nothing
  -f, --force        don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help         help for upgrade
      --json         print it as one JSON object, for scripts
      --to VERSION   install the release VERSION instead of the newest, even an older one or a prerelease
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm upgrade](../using/upgrading.md#upgrading) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
