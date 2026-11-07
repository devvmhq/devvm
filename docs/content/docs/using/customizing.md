---
title: Customizing images
explainer: Add your own layers and choose base's features
weight: 7
---

You customize images with your own layers, which add what a project needs, and by choosing base's features, which are devvm's optional extras that every VM gets. Your layers live in `~/.config/devvm` (`$XDG_CONFIG_HOME/devvm`), not next to devvm's binary, so upgrading devvm keeps them. To give every new VM something of your own, write it as a layer and make it one of the layers new VMs get ([extras of your own](#extras-of-your-own)).

## Your own layers

```sh
devvm layer new mailpit    # writes ~/.config/devvm/layers/mailpit.sh and mailpit.policy
$EDITOR ~/.config/devvm/layers/mailpit.sh
devvm --layers mailpit     # a new VM with your layer; or devvm build mailpit
```

When you customize a new VM, its list also shows your layers under Layers, so you can choose `mailpit` there instead of giving `--layers` ([what a new VM gets](images.md#what-a-new-vm-gets)). To give every new VM your layer, choose it with `devvm setup --layers mailpit`, or add it to the `layers` key in `config.toml` ([layers for new VMs](settings.md#layers-for-new-vms)). When you delete a layer's script that you chose there, new VMs are made without it, and devvm warns you.

A [layer](images.md#layers) is a script, `~/.config/devvm/layers/<name>.sh`, that bash runs once as the VM's user, who has sudo, in an image on top of its parent. `devvm layer new NAME` writes a starter script, and also `NAME.policy` next to it unless that file exists. It refuses to overwrite a script that exists.

- **Names** are made of lowercase letters, digits and dashes, and start with a letter or digit. A name can't be `base`, a [built-in layer's](images.md#built-in-layers) name (`desktop`, `docker` or `nix`) or a shared tool's name (`node`, `claude` and so on). A command's name, such as `log`, is fine, because devvm takes layers only through `--layers`, the list and image names.
- Built-in layers go in the same image names as yours, bottom first, as in `devvm --layers docker,mailpit`, which makes the image `docker+mailpit`.
- Put interactive shell setup in `~/.bashrc.d/<name>.sh`, which every VM's `~/.bashrc` reads, and PATH changes in `~/.profile`.
- Download files with `devvm-fetch URL SHA256 OUT`, which `base` installs. It fails when the file's checksum doesn't match.
- `NAME.policy` lists the hosts that VMs with the layer can reach on the [allowlist tier](../safety/network.md#allowlists), one rule per line.
- Editing the script rebuilds its image, and the ones built on it, the next time they're used ([rebuilding](images.md#rebuilding)).
- A layer's script changes the VM's disk, so `devvm layer add mailpit` gives a VM you have the layer by making the VM again, keeping its home folder and settings ([adding layers to a VM](images.md#adding-layers-to-a-vm)), and `devvm layer rm mailpit` takes it out again in the same way ([taking layers out of a VM](images.md#taking-layers-out-of-a-vm)).

To edit layers with an agent in a VM, keep them in a repo of their own and copy them in, because devvm refuses a VM for a folder inside `~/.config/devvm` ([details](../internals/security.md#only-the-project-folder)).

### Example: mailpit

This layer installs [Mailpit](https://mailpit.axllent.org/), which catches the emails your app sends while you develop it, and starts it with the VM. Point your app's SMTP settings at `localhost:1025`, and read the emails at `localhost:8025` in your browser, because devvm [forwards](ports-size.md#ports) both ports to your machine. Put these lines in `~/.config/devvm/layers/mailpit.sh`, below the starter's `set -euo pipefail`:

```bash
mailpit_version=1.32.0
mailpit_sha256_amd64=4b5993c978e8604c70d43927e2ad103b311bba3b1822fa0cf1d4ee4ec890a93c
mailpit_sha256_arm64=e1e45d1dc72bd497fb262a1e4100178eee057c42c5fc36d143eb8621769e1137

case "$(uname -m)" in
  x86_64) arch=amd64 sha256=$mailpit_sha256_amd64 ;;
  aarch64) arch=arm64 sha256=$mailpit_sha256_arm64 ;;
  *) echo "mailpit: no build for $(uname -m)" >&2; exit 1 ;;
esac
devvm-fetch "https://github.com/axllent/mailpit/releases/download/v$mailpit_version/mailpit-linux-$arch.tar.gz" \
  "$sha256" /tmp/mailpit.tar.gz
sudo tar -xzf /tmp/mailpit.tar.gz -C /usr/local/bin mailpit
rm /tmp/mailpit.tar.gz

printf '%s\n' '[Unit]' 'Description=Mailpit' '[Service]' 'ExecStart=/usr/local/bin/mailpit' \
  'DynamicUser=yes' '[Install]' 'WantedBy=multi-user.target' |
  sudo tee /etc/systemd/system/mailpit.service >/dev/null
sudo systemctl enable --now mailpit
```

The version and each download's SHA-256 sit at the top, so `devvm-fetch` refuses a download that doesn't match. To update Mailpit, change all three and rebuild the image. The downloads come from GitHub's releases, which `base`'s allowlist has, so `mailpit.policy` can stay empty. On a base made `--from` an Alpine image, start it with OpenRC (`rc-update`, `rc-service`) instead of systemd.

## Base image features

Features are optional extras that devvm builds into [`base`](images.md#base) after base's own setup, so every VM has them.

| Feature | What it adds |
| --- | --- |
| `omb` | [oh-my-bash](https://ohmybash.github.io/): a themed prompt, completions and aliases, with auto-update off |
| `fzf` | fuzzy Ctrl-R history and Ctrl-T file search |
| `cli-tools` | bat, fd, eza, zoxide (`z`), btop, tree, tldr |

The desktop that [`devvm desktop`](desktop.md) opens isn't a feature but a [built-in layer](images.md#built-in-layers), `desktop`, so only the VMs you choose it for get it.

### Choosing features

On its first run, devvm asks which features `base` gets, after the size and coding agents that new VMs get and before it shows what the folder's new VM gets. devvm build and devvm rebuild ask too, until the features are chosen. The list starts with every feature chosen, and you choose in it as in [any of devvm's lists](images.md#choosing-in-a-list). Without a terminal, devvm chooses every feature. devvm prints the choice and saves it, in the order the features run. When `features` in [`config.toml`](settings.md#configtoml) chooses them, devvm doesn't ask.

To change the choice:

```sh
devvm setup                     # asks again, starting from the current choice
devvm setup --features fzf,omb  # without asking
devvm setup --features none     # without asking, and base gets no features
```

In a terminal, `devvm setup` asks for the features among its other questions, which [devvm setup](settings.md#devvm-setup) lists in order. `--features` asks for none of the others. Without a terminal, `devvm setup` keeps the choice. devvm refuses a name that isn't a feature, and an empty `--features ''`, and then nothing changes. You can also choose them by hand with `features` in `config.toml`, such as `features = ["fzf", "omb"]`, which wins over what `devvm setup` saved, and then `devvm setup --features` refuses to change them. `features = []` chooses none, and devvm skips a name without a script, with a warning.

A new choice makes `base` and the images built on it rebuild the next time they're used, and `devvm setup` says so. The old images are then stale, and [`devvm gc`](images.md#cleaning-up) deletes them.

### Extras of your own

Base's features are the ones in the table above, and devvm ships all of them. To give every new VM an extra of your own, such as your shell setup or a tool you always use, write it as a [layer of your own](#your-own-layers) and choose it as one of the layers new VMs get:

```sh
devvm layer new dotfiles           # writes ~/.config/devvm/layers/dotfiles.sh
$EDITOR ~/.config/devvm/layers/dotfiles.sh
devvm setup --layers dotfiles      # every new VM gets it
```

A new VM's summary then shows `dotfiles`, and when you customize the VM, its list shows `dotfiles` under Layers, already chosen because devvm setup chose it, so you can leave it out of that VM ([layers for new VMs](settings.md#layers-for-new-vms)). A layer is an image built on top of base, so the images that have it are named with it, such as `dotfiles` or `docker+dotfiles`, and editing its script rebuilds only those images, never base.
