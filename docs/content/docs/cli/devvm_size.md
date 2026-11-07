---
title: devvm size
weight: 53
explainer: "Show or change this folder's VM's size"
---

Show or change this folder's VM's size

## Synopsis

Show this folder's VM's CPUs, memory and disk, or change them with --cpus, --memory and --disk. The VM changes when it next starts, so for a running VM devvm offers in a terminal to restart it now. With -g, show or change the size new VMs get instead.

```text
devvm size [flags]
```

## Examples

```text
  devvm size                       # show the VM's size
  devvm size --cpus 6 --memory 16  # give it 6 CPUs and 16 GiB of memory
  devvm size --disk 200            # grow its disk to 200 GiB
  devvm size -g --memory 16        # give new VMs 16 GiB of memory
```

## Options

```text
      --cpus N       the N CPUs the VM gets
      --disk GiB     the VM's disk in GiB, at least what it has
  -g, --global       show or change the size new VMs get instead
  -h, --help         help for size
      --here         use this folder's own VM, never that of a folder it's inside
      --json         print it as one JSON object, for scripts
      --memory GiB   the VM's memory in GiB, such as 16 or 16G
      --parent       use the VM of the nearest folder this one is inside, even across a git repo of its own
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm size](../using/ports-size.md#size) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
