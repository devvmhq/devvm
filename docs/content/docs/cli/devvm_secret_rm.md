---
title: devvm secret rm
weight: 48
explainer: "Delete a saved secret"
---

Delete a saved secret

## Synopsis

Delete a saved secret, after asking in a terminal without --no-input. Proxies that are running stop putting it in at once, but what a VM did with it stays done.

```text
devvm secret rm NAME [flags]
```

## Examples

```text
  devvm secret rm github          # delete the GitHub token, after asking
  devvm secret rm github --force  # delete it without asking, as a script must
```

## Options

```text
  -f, --force   don't ask to confirm, which devvm needs without a terminal or with --no-input
  -h, --help    help for rm
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm secret rm](../safety/network.md#saving-and-binding-secrets) explains it in full.

## See also

- [devvm secret](devvm_secret.md): List the secrets kept on this machine, for VMs to use
