---
title: devvm secret use
weight: 51
explainer: "Let this folder's VM use a secret"
---

Let this folder's VM use a secret

## Synopsis

Let this folder's VM use a secret. Its proxy puts the secret in at once, and the VM gets its placeholder from its next launch.

```text
devvm secret use NAME [flags]
```

## Examples

```text
  devvm secret use github  # let the VM use your GitHub token
```

## Options

```text
  -h, --help     help for use
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm secret use](../safety/network.md#saving-and-binding-secrets) explains it in full.

## See also

- [devvm secret](devvm_secret.md): List the secrets kept on this machine, for VMs to use
