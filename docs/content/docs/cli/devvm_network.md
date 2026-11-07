---
title: devvm network
weight: 35
explainer: "Show or change the network tier of this folder's VM"
---

Show or change the network tier of this folder's VM

## Synopsis

Show the network tier of this folder's VM, which is open, internet, allowlist, none or unenforced, and then its allowlist, policy by policy: devvm's built-in ones by name, then your files by path. With a tier, give the VM that tier: between the confined tiers at once, and into or out of open from the VM's next start, so in a terminal devvm offers to restart a running VM now. With -g, show or change the tier new VMs get, allowlist unless you change it.

```text
devvm network [open|internet|allowlist|none] [flags]
```

## Examples

```text
  devvm network                    # show the VM's network tier and allowlist
  devvm network allow example.com  # let the VM reach example.com
  devvm network log --blocked      # list what the VM's proxy blocked
  devvm network internet           # let the VM reach any public host
  devvm network -g none            # give every new VM the none tier
```

## Options

```text
  -g, --global   show or change new VMs' network tier instead
  -h, --help     help for network
      --here     use this folder's own VM, never that of a folder it's inside
      --json     print it as one JSON object, for scripts
      --parent   use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm network](../safety/network.md#choosing-a-tier) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm network allow](devvm_network_allow.md): Allow hosts for this folder's VM
- [devvm network deny](devvm_network_deny.md): Deny hosts for this folder's VM
- [devvm network log](devvm_network_log.md): Show what this folder's VM connected to and what its proxy blocked
- [devvm network rm](devvm_network_rm.md): Remove rules from this folder's VM
