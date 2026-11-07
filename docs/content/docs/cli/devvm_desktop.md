---
title: devvm desktop
weight: 11
explainer: "Open this folder's VM's desktop in a VNC viewer"
---

Open this folder's VM's desktop in a VNC viewer

## Synopsis

Start the desktop of this folder's VM, which needs the desktop layer, and serve it on this machine's 127.0.0.1 with a password of the VM's own until you press Ctrl+C or the VM stops. On macOS, Screen Sharing opens it and asks for the password, which devvm shows.

```text
devvm desktop [flags]
```

## Examples

```text
  devvm desktop              # open the VM's desktop in Screen Sharing
  devvm desktop --clipboard  # the same, sharing the clipboard both ways
  devvm desktop --no-open    # show the address and password for a VNC viewer of your own
```

## Options

```text
      --clipboard   share the clipboard between this machine and the VM, both ways
  -h, --help        help for desktop
      --here        use this folder's own VM, never that of a folder it's inside
      --no-open     show the address and password without opening Screen Sharing
      --parent      use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm desktop](../using/desktop.md) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
