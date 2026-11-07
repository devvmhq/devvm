---
title: devvm network allow
weight: 36
explainer: "Allow hosts for this folder's VM"
---

Allow hosts for this folder's VM

## Synopsis

Add RULEs to the allowlist of this folder's VM, or with -g of every VM, from the next connection on. RULE is a host, *.host for its subdomains, ** for any host, or an address or CIDR, and with a port, such as example.com:8443, it matches only that port instead of 443 and 80.

```text
devvm network allow RULE... [flags]
```

## Examples

```text
  devvm network allow example.com      # let the VM reach example.com
  devvm network allow '*.example.com'  # and any of its subdomains
  devvm network allow 10.0.0.5:5432    # one port of one address
  devvm network allow -g pypi.org      # let every VM reach pypi.org
```

## Options

```text
  -g, --global   change your rules for every VM instead
  -h, --help     help for allow
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm network allow](../safety/network.md#changing-rules) explains it in full.

## See also

- [devvm network](devvm_network.md): Show or change the network tier of this folder's VM
