---
title: devvm link publish
weight: 31
explainer: "Show or add the ports this folder's VM offers to links"
---

Show or add the ports this folder's VM offers to links

## Synopsis

Show the ports this folder's VM publishes, or offer PORTS, ports and ranges like 5432,8000-8010, to links from any of your other VMs. Each link is still made from the other VM's folder, with devvm link add.

```text
devvm link publish [PORTS] [flags]
```

## Examples

```text
  devvm link publish            # show what the VM publishes
  devvm link publish 5432       # offer port 5432 to links
  devvm link publish 8000-8010  # offer a range
```

## Options

```text
  -h, --help     help for publish
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

[The docs page for devvm link publish](../using/links.md#publishing-a-port) explains it in full.

## See also

- [devvm link](devvm_link.md): List this folder's VM's links to your other VMs
