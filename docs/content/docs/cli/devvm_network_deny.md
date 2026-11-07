---
title: devvm network deny
weight: 37
explainer: "Deny hosts for this folder's VM"
---

Deny hosts for this folder's VM

## Synopsis

Add RULEs as denies to this folder's VM's rules, or with -g every VM's, from the next connection on, and a deny beats any allow. RULE is a host, *.host for its subdomains, ** for any host, or an address or CIDR, and with a port, such as example.com:8443, it matches only that port instead of 443 and 80.

```text
devvm network deny RULE... [flags]
```

## Examples

```text
  devvm network deny example.com         # keep the VM from reaching example.com
  devvm network deny -g '*.example.com'  # keep every VM from its subdomains
```

## Options

```text
  -g, --global   change your rules for every VM instead
  -h, --help     help for deny
      --here     use this folder's own VM, never that of a folder it's inside
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm network deny](../safety/network.md#changing-rules) explains it in full.

## See also

- [devvm network](devvm_network.md): Show or change the network tier of this folder's VM
