---
title: devvm ports
weight: 40
explainer: "Show or change which ports this folder's VM forwards"
---

Show or change which ports this folder's VM forwards

## Synopsis

Show which ports this folder's VM forwards to the same port on this machine's localhost, or change them at once to all, none or a LIST of ports and ranges like 5173,8000-8010. With -g, show or change what new VMs forward instead.

```text
devvm ports [all|none|LIST] [flags]
```

## Examples

```text
  devvm ports                 # show what the VM forwards
  devvm ports 5173,8000-8010  # forward only these ports and ranges
  devvm ports none            # forward nothing
  devvm ports -g none         # make new VMs forward nothing
```

## Options

```text
  -g, --global   show or change what new VMs forward instead
  -h, --help     help for ports
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

[The docs page for devvm ports](../using/ports-size.md#ports) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
- [devvm ports add](devvm_ports_add.md): Forward more ports from this folder's VM
- [devvm ports rm](devvm_ports_rm.md): Stop forwarding some ports from this folder's VM
