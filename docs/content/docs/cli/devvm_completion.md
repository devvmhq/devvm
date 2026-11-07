---
title: devvm completion
weight: 10
explainer: "Print the shell completion script for bash, zsh or fish"
---

Print the shell completion script for bash, zsh or fish

## Synopsis

Print the script that completes devvm's commands, flags, image names and shared tools in SHELL, which is bash, zsh or fish. Load it from your shell's startup file.

```text
devvm completion SHELL [flags]
```

## Examples

```text
  source <(devvm completion bash)  # in ~/.bashrc, with the bash-completion package installed
  source <(devvm completion zsh)   # in ~/.zshrc, after compinit
  devvm completion fish | source   # in ~/.config/fish/config.fish
```

## Options

```text
  -h, --help   help for completion
```

## Options inherited from parent commands

```text
      --no-input   never ask, and fail where devvm would ask, naming the flag that answers it
  -v, --verbose    show the output of the commands devvm runs instead of a status line
```

## Docs

[The docs page for devvm completion](../using/command-line.md#shell-completion) explains it in full.

## See also

- [devvm](devvm.md): Open a shell in this folder's VM, creating it first
