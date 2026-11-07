---
title: devvm secret
weight: 47
explainer: "List the secrets kept on this machine, for VMs to use"
---

List the secrets kept on this machine, for VMs to use

## Synopsis

List the secrets devvm keeps on this machine, claude for the Claude Code token, github for a GitHub token and proxy for the password of the company proxy that config.toml names, with whether each is saved and the VMs using it. A VM gets a placeholder, and its proxy puts the secret into requests for the secret's hosts. No VM gets proxy, which devvm's proxies send only to the company proxy.

```text
devvm secret [flags]
```

## Examples

```text
  devvm secret             # list the secrets and the VMs using each
  devvm secret set github  # save a GitHub token, asking for it
  devvm secret use github  # let this folder's VM use it
```

## Options

```text
  -h, --help   help for secret
      --json   print it as one JSON object, for scripts
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm secret](../safety/network.md#saving-and-binding-secrets) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm secret rm](devvm_secret_rm.md): Delete a saved secret
- [devvm secret set](devvm_secret_set.md): Save a secret on this machine
- [devvm secret unuse](devvm_secret_unuse.md): Stop this folder's VM using a secret
- [devvm secret use](devvm_secret_use.md): Let this folder's VM use a secret
