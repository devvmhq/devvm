---
title: What devvm keeps on this machine
explainer: Each file devvm keeps, and how it's cleaned up
weight: 14
---

devvm, the tools it runs and Lima keep files on this machine outside your project folders. This page lists each of them: what it is, who makes it, where it is, whether devvm can make it again, and how it's cleaned up. Most of it is in devvm's data folder, `~/.local/share/devvm`, and devvm can make all of that again except your exports and what your commands saved, such as your agents' logins and your secrets. Lima's own caches hold more, and they can also hold your own downloads.

## devvm space

`devvm space` shows what devvm keeps on this machine and how much disk each kind of thing takes. It changes nothing and starts no VM, and it gives Lima 30 seconds to answer. `devvm space --json` prints the same as JSON, for scripts ([devvm space --json](#devvm-space---json)). The first table has a row for each kind of thing devvm has, followed by a total:

| Column | |
| --- | --- |
| `WHAT` | `VMs`, `images`, `tools gnu` or `tools musl` for the shared tools' builds for each C library, `history` for the exports of agents' history, `network logs` for the VMs' logs and the daily tools check's log, `run logs`, `mise`, `packages` for the copies of [base's packages](images.md#base), `Nix cache` for the [shared Nix cache](toolchains.md#nix-projects), `leftover state` for the whole folder of each VM that `devvm gc --orphans` would forget, and `state` for the rest of devvm's data folder, which includes the rest of the VMs' folders |
| `COUNT`, `SIZE` | how many there are and the disk they take, in powers of 1024 (`2.1G`) |
| `KEPT` | `remade` when devvm makes it again when it's needed, `only copy` for VMs and exports, which nothing else has, `log` for logs, `state` for what devvm knows of its VMs and images, and `leftover` |
| `GC` | what `devvm gc` frees of it now, which for the Nix cache is what it takes beyond its cap |
| `FREED BY` | what frees the rest, such as `devvm rm`, `images rm`, which deletes images that are current, or `gc --nix-cache` |
| `RETAIN` | the [retention setting](settings.md#retention) that applies to the row when it's on, such as `30d`, with what it would delete now in brackets, and `-` when none is on |

Sizes count each file's blocks once, as `du` does. On macOS a Lima VM's disk shares its image's blocks until the VM writes them, so those blocks count for both the VM and its image, and the total can be more than the disk they take. When Lima doesn't answer while one of your VMs uses it, devvm says so, and its VMs and images aren't counted.

### Lima's caches

The second table lists the caches that belong to Lima, with what each cache holds, how many entries it has, its size and where it is. That's Lima's download cache. devvm's builds fill it, but it can also hold your own downloads, so it isn't in the total. The [Lima](#lima) section below says what it holds.

devvm never prunes these caches on its own. `devvm gc --caches` frees devvm's share of them, which is what devvm's builds put there and nothing of devvm's needs. It lists the entries it would delete with their sizes and asks. Without a terminal it refuses unless you give `--force`, which frees them without asking.

### devvm space --json

`devvm space --json` prints both tables as one JSON object, as [output for scripts](command-line.md#output-for-scripts) describes, and devvm still says on stderr what it says without `--json`. `devvm` has a row of the first table each, with sizes in bytes:

| Field | |
| --- | --- |
| `what`, `count`, `size` | the row's `WHAT`, `COUNT` and `SIZE` |
| `backend` | the backend that has them, `lima`, for VMs, images and shared tools, left out for the other rows and for a shared tool's build when devvm can't tell which backend it's for |
| `sized` | whether devvm knows the size, which `SIZE` shows as `-` when it doesn't |
| `kept` | the row's `KEPT` |
| `gc` | what `devvm gc` frees of it now |
| `rest` | the row's `FREED BY` |
| `retain`, `retain_frees` | the retention setting that applies to the row and what it would delete now, both left out when none is on |

`total` and `gc` are the totals row's `SIZE` and `GC`. `backends` has the second table's rows, each with its `backend`, `name`, `path`, `count`, `size` and `sized`. `unknown` names the backends that one of your VMs uses that didn't answer, and `strays` has the [VMs devvm has no state for](#vms-devvm-has-no-state-for), each with its `backend`, `name` and the `data` folder its label names. `folder_gone` has the [VMs whose folder is gone](#vms-whose-folder-is-gone), each with its `backend`, `name` and `folder`, and `folder_not_connected` has those whose folder is on a disk that isn't connected, with the same fields. devvm leaves these out when they're empty.

```json
{
  "version": 1,
  "devvm": [
    {
      "what": "VMs",
      "backend": "lima",
      "count": 2,
      "size": 5368709120,
      "sized": true,
      "kept": "only copy",
      "gc": 0,
      "rest": "devvm rm"
    },
    {
      "what": "history",
      "count": 3,
      "size": 3145728,
      "sized": true,
      "kept": "only copy",
      "gc": 0,
      "rest": "gc --history",
      "retain": "30d",
      "retain_frees": 1048576
    }
  ],
  "total": 5371854848,
  "gc": 0,
  "backends": [
    {
      "backend": "lima",
      "name": "download cache",
      "path": "/Users/you/Library/Caches/lima",
      "count": 1,
      "size": 629145600,
      "sized": true
    }
  ]
}
```

## devvm's data folder

devvm keeps its state in `~/.local/share/devvm`, or in `$XDG_DATA_HOME/devvm` when `XDG_DATA_HOME` is set. Only you can read it, because each time devvm runs, it makes this folder and the settings folder, `~/.config/devvm`, yours alone (mode 700), whatever mode they had, so nothing inside either one is open to others. If devvm can't, for example because someone else owns a folder, it warns and goes on. Whenever devvm writes into the folders that hold history, logs, keys and secrets, which are `exports/`, `logs/`, `logs/runs/`, `run/`, `secrets/`, and the `run/`, `run/ca/` and `logs/` folders in each VM's folder, it makes them only yours again as well, whatever mode they had. Everything in it is devvm's own, and devvm makes most of it again when it's needed. The exceptions are the exports, which are the only copy of a deleted VM's agent history, and the defaults, agents' logins, keys and secrets that your commands saved.

| Path | What it is | How it's cleaned up |
| --- | --- | --- |
| `version`, `version.lock` | the folder's [layout version](troubleshooting.md#a-newer-devvm-changed-devvms-folders), and an empty file that each devvm checking it locks | they stay, and take no space |
| `settings.json` | the defaults that `devvm setup`, `devvm size -g`, `devvm ports -g`, `devvm network -g` and `devvm retention` save, below [`config.toml`](settings.md#configtoml)'s | it stays, and takes almost no space |
| `ssh_config` | the [ssh config](ssh.md#ssh-access-by-name) for running VMs | devvm takes a VM out when it stops or deletes the VM |
| `agent-keys.json` | the keys in [devvm's own SSH agent](ssh.md#devvms-own-agent) and their options | `devvm ssh-agent rm` takes a key out |
| `logins/` | your agents' logins in [devvm's agent store](coding-agents.md#devvms-agent-store), one folder for each agent, which `devvm agent import` and `devvm agent login` save and only you can read | they stay until you delete them |
| `secrets/` | on hosts without a Keychain, such as Linux hosts, your [secrets](../safety/network.md#saving-and-binding-secrets), one file each | `devvm secret rm` deletes one |
| `vms/<hash>/` | one folder for each VM, which holds everything devvm keeps for it in its data folder. `<hash>` is the hash of the project folder's path that the VM's name ends with. The folder holds the VM's [state](../internals/overview.md#a-vms-state) in `vm.json`, which records the VM's name, its ssh alias, its image, folder, backend and settings and the version of the devvm that made it, and the VM's own network rules in `policy`. The files of its proxy, port forwarder, link session and sandbox are in `run/`, with the VM's host keys for its current start in `run/known_hosts`, which devvm reads again each time the VM starts, and its logs are in `logs/` | `devvm rm` deletes the whole folder with the VM |
| `images/` | a folder for each image, named by its key, which holds the image's record, `record.json`; the [rebuild counters](images.md#rebuilding) in `rebuilds.json`, one for each backend, each image that base starts from and each image; the Ubuntu cloud image that base starts from on this machine, in `ubuntu.json`; and in `locks/` an empty file for each image that the devvm building it locks, one, `rebuilds`, that a devvm changing a rebuild counter locks, one, `ubuntu`, that a devvm pinning the cloud image locks, and one, `packages`, that a devvm changing `packages/` locks | `devvm gc` and `devvm images rm` delete an image's folder and its lock file with the image |
| `packages/` | a copy of each package that a first build of base installed, named by its SHA-256, and in `sets/` the list of packages each base recorded, named by its key, which building the same base again installs from these copies ([base](images.md#base)) | `devvm gc` deletes the lists of bases that are neither current nor built, then the copies that no list names. `devvm rebuild base` records a new list |
| `tools/` | the [shared tools](images.md#shared-tools), one read-only image per tool version, each with a record beside it, `<image>.json`, which holds its checksum, the backend that built it, the C library it's built for, when it was built and, once retention finds that no VM has it, since when | `devvm gc` deletes the versions that no VM has and that aren't the current ones, and `devvm gc --tool-images` also the current ones that no VM has, except while a VM is [being made](images.md#cleaning-up), each with its record |
| `mise.lock`, `mise.lock.toml`, `mise.lock.checked` | the tools lock, which holds the tools' versions and their checksums, the config it was resolved from, and when devvm last checked upstream for newer versions | devvm replaces them when it checks again |
| `cache/` | what devvm makes again whenever it needs it: its own mise, the shared Nix cache, and in `from-digests.json` the digest that each container image a `--from` base starts from had when devvm last looked it up | you can delete it at any time, and devvm makes each part again when it's needed. An image built from a container image keeps that digest in its own record, so it stays current without the cache |
| `cache/mise/<version>` | the mise that devvm runs on this machine to resolve the tools lock, one folder per mise version | `devvm gc` deletes the folders of older versions, which an upgrade of devvm leaves |
| `exports/` | the agents' [history](coding-agents.md#history) that `devvm history export` and `devvm rm` save, one `.tar.gz` per export, each with a `.folder` file beside it that names its VM's project folder | `devvm gc --history` deletes them, or with `--older-than 30d` those that old, and nothing else does |
| `logs/runs/` | the [run logs](troubleshooting.md#a-command-fails), one per devvm run that runs a command | devvm keeps the newest 20 |
| `logs/tools-check.log` | the output of the daily check for newer shared tools | each check replaces it |
| `vms/<hash>/logs/` | a VM's network log, `network.jsonl`, which holds its proxy's decisions, its proxy's own output, `proxy.log`, and its link session's output, `links.log`. Before the network log or the proxy's log would grow past 10 MiB, the proxy moves it to the same name with `.1` added, replacing the older one, so each of them takes at most about 20 MiB, and `devvm network log` reads both of the network log's files | `devvm rm` deletes them with the VM's folder, `.1` files included |
| `vms/<hash>/run/ca/` | the certificate authorities that the VM trusts for its proxy, its current one and the next, with their keys, which never leave this machine | devvm makes a new one when the current one has less than a week of its 30 days left, the VM's first start after `devvm network open` deletes them, and `devvm rm` deletes them with the VM's folder |
| `cache/nix-cache/` | the [shared Nix cache](toolchains.md#nix-projects): the packages that VMs with the `nix` layer downloaded from Nix's binary cache, once for all of them | the VMs' proxies and `devvm gc` delete its least recently used files while it takes more than its cap, `nix-cache.size`, which is 10 GiB when it's off, and `devvm gc --nix-cache` deletes all of it |
| `run/` | what running VMs need that isn't one VM's or whose path must be short, such as the QEMU wrapper on Linux hosts, the claims lock, `claims.lock`, which devvm holds while it chooses a new VM's ssh alias or a VM's host ports, the sockets of port forwarders and link sessions, which are named by hashes, in `build-ports/` the ssh port of each build VM that's starting, and in `builds/` a folder for each build VM, laid out as a VM's folder is | devvm deletes a VM's sockets when it deletes the VM, a build VM's port once the VM has started or failed to, or, if the devvm that started the VM was killed, the first time it chooses a port 15 minutes later, and a build VM's folder once its image is built, or else when it deletes the build VM |
| `agent.sock` | the socket of [devvm's own SSH agent](ssh.md#devvms-own-agent) | it takes no space |
| `oci-build.lock` | an empty file that the devvm building a base from a container image on Lima locks, so that one such build runs at a time | it stays, and takes no space |
| `tools.lock` | an empty file that the devvm building shared tools or updating the tools lock locks, so that one at a time writes to `tools/` | it stays, and takes no space |
| `groups.lock` | an empty file that each devvm joining or leaving a [group](links.md#groups), or changing a group's links, locks, so that they take turns | it stays, and takes no space |
| `making.lock` | an empty file that each devvm making a VM locks, so that `devvm gc --tool-images` can tell a VM is [being made](images.md#cleaning-up) | it stays, and takes no space |
| `retention.lock` | an empty file that the devvm applying the [retention settings](settings.md#retention) locks, so that one at a time applies them | it stays, and takes no space |
| `lima/` | devvm's own [Lima home](../backends/lima.md#devvms-lima-home), with its VMs and images, unless `DEVVM_LIMA_HOME` moves it | see [Lima](#lima) |

Which ports each VM forwards is in its state, and its port forwarder's files are in its own folder too, its log in `vms/<hash>/logs/` and the rest in `vms/<hash>/run/`. `devvm rm` deletes them with the VM.

The [retention settings](settings.md#retention), which are all off until you set them, make devvm delete some of these on its own: `logs.runs` keeps that many run logs instead of 20, `history.age` and `history.count` delete old exports of agents' history, `tools.unused-age` deletes current shared tools that no VM has had for that long, and `orphans.age` forgets VMs that their backend no longer has. `nix-cache.size` sets the shared Nix cache's cap, and each time retention runs, it deletes the cache's least recently used files while it takes more than that. Retention also writes `retention.run` in the data folder, when it last ran, and records in each current shared tool's record since when no VM has had it. It never deletes anything in `cache/` but the shared Nix cache's files.

A build also makes a temporary folder in the data folder, `tools-build.<random>` for the shared tools, and devvm deletes it when the build ends, even after a failure or Ctrl+C.

### Shared tools

Each tool version is built once for each kind of VM that uses it, so `tools/` can hold more than one build of the same version. A base on Alpine needs its tools built for its C library, musl, rather than glibc, so a node 24 that both an Ubuntu VM and an Alpine VM use is two builds, and `devvm images` lists both. Each Alpine container image gets builds of its own, so VMs on two different Alpine images have two musl builds of it. [Cleaning up](images.md#cleaning-up) says which ones `devvm gc` deletes.

### Exports

`devvm rm` saves the history of a VM whose image has an agent before it deletes the VM, so each export in `exports/` can be the only copy of that VM's transcripts and sessions. devvm deletes an export only when you ask it to. `devvm history` lists the exports, which are named `<vm>-<date>-<time>.tar.gz`, with their VM, its folder, their date and their size. `devvm gc --history` lists them by their VM and date, then deletes them, each with the `.folder` file beside it, after asking in a terminal, and `devvm gc --history --older-than 30d` deletes only those written 30 days ago or more. gc leaves anything else in the folder alone, such as an export that `devvm history export` is still writing.

### Leftovers of a VM deleted outside devvm

When you delete a VM with its backend's own command instead of `devvm rm`, such as `limactl delete`, devvm still keeps the VM's folder, with its state, its logs and its backend's files, the VM's sockets in `run/`, its ssh config entry and its host keys. `devvm gc` also keeps the shared tools that the VM's state names. `devvm rm` deletes these only together with the VM, so once the VM is gone it says the folder has no VM.

`devvm gc --orphans` deletes what devvm keeps for such a VM, as `devvm rm` does after it deletes a VM. That includes, for a `--clone` VM, the `devvm` remote in your repo. gc first asks each VM's backend whether it still has the VM, lists the VMs it would forget with their project folders, and in a terminal asks. It never deletes a VM, and it leaves a VM's state alone in these cases, saying why:

- **The backend doesn't answer**, for example because it isn't installed. When a backend that one of your VMs uses can't list its images, gc warns and leaves all of that backend's VMs' state alone, as it does the rest of that backend's ([cleaning up](images.md#cleaning-up)).
- **The VM was made elsewhere.** devvm records where each new VM's backend keeps it: the Lima home, which `DEVVM_LIMA_HOME` moves. When this run looks somewhere else, the VM may still exist there. A VM whose state doesn't record it counts as made in the backend's default place, so while `DEVVM_LIMA_HOME` is set, gc leaves such a VM alone.
- **Its state changed in the last hour,** because the VM may still be being made.

When another devvm starts to make the VM again in its folder after gc listed it, gc refuses to forget it and stops there, and so does retention's `orphans.age`.

Forgetting a VM also removes your other VMs' [links](links.md) to it, as `devvm rm` does. `devvm gc --orphans` also removes links to VMs whose state is already gone, naming each one, and deletes the sockets that a link session leaves on your machine when it's killed, which are the ones nothing listens on.

Once a VM's state is gone, `devvm gc` treats its shared tools like any others. `devvm space` counts the whole folder of each VM that `devvm gc --orphans` would forget as `leftover state`, and each socket a link session left counts there too. A VM that gc leaves alone for one of the reasons above isn't leftover state, so its folder counts as `state` and its logs as `network logs`.

### VMs devvm has no state for

A VM can also be the other way around: the backend has it, with devvm's label, but devvm has no state for it. Usually it belongs to another devvm, such as one run with another `XDG_DATA_HOME` for a test or a sandbox. `devvm space` names each one, with the data folder its label names, and devvm never deletes it, because the other devvm may still use it. To delete one yourself, run `devvm limactl delete NAME`.

### VMs whose folder is gone

A VM whose project folder you deleted or moved still takes its disk, and it isn't leftover state, because its backend still has it and it may hold that project's work. `devvm space` names each VM whose backend has it and whose folder is gone, with that folder, and counts it among the `VMs`. Neither `devvm gc` nor retention ever deletes one. `devvm rm VM` deletes it after asking, exporting its agents' history first ([a VM whose folder is gone](vms.md#a-vm-whose-folder-is-gone)).

`devvm space` also names each VM whose folder is on a disk that isn't connected, with that folder, and counts it among the `VMs` too. Connect the disk to use the VM again. `devvm rm VM` deletes one only after you confirm in a terminal, and `--force` never answers that question ([a folder on a disk that isn't connected](vms.md#a-folder-on-a-disk-that-isnt-connected)).

### What devvm doctor finds in `vms/`

`vms/` should hold only VMs' folders, each named by a hash of 14 hexadecimal digits and holding the VM's `vm.json`. `devvm doctor` warns about anything else it finds there, says what to do, and changes nothing:

- **An entry that isn't a VM's folder,** such as a file or a folder with another name, which devvm never uses. Move it out of `vms/` or delete it.
- **A VM's folder without `vm.json`,** which a devvm that stopped while making or deleting a VM leaves, for example one holding only the VM's `lock`. No VM uses the folder, so delete it.
- **A folder doctor can't read,** `vms/` itself or a VM's folder, so it can't check what's in it. Check that you own it and can read it, for example with `ls -ld`.

These warnings never make doctor fail, and `devvm doctor --json` lists them in `warnings`.

## Your settings

`~/.config/devvm` holds what you edit: `config.toml`, your network rules, your agents' settings, your own layers, and your company's CA certificates. devvm also writes the folder's [layout version](troubleshooting.md#a-newer-devvm-changed-devvms-folders) into `version` there when the file is missing or older. [Your settings](settings.md#your-settings) lists each file. devvm never deletes any of them.

## Lima

The Lima backend's VMs and images are in devvm's Lima home, `~/.local/share/devvm/lima`. Each image is a stopped VM named `devvm-img-<key>`, and each project VM is a clone of one. On macOS a clone's disk shares its image's blocks until the VM writes them, so `du` counts those blocks twice. On Linux hosts a VM's disk is an overlay of its image's. `devvm rm` deletes a VM, `devvm gc` deletes stale and unfinished images, and `devvm images rm` deletes any image you name.

Lima keeps a download cache of its own, `~/Library/Caches/lima` on macOS and `~/.cache/lima` on Linux, or `$XDG_CACHE_HOME/lima` when `XDG_CACHE_HOME` is set. When devvm builds base, Lima downloads Ubuntu's cloud image into that cache, which is 600 to 900 MB for each release, and on macOS it also keeps a copy converted to a raw disk, which takes about 2.4 GB more. The cache is Lima's and it isn't in devvm's Lima home, so your own Lima VMs' downloads share it. devvm deletes something in it only when you run `devvm gc --caches`. Lima keeps each download in a folder named by the SHA-256 of its address, and gc frees only the folders of Ubuntu's cloud images at the address devvm downloads base from, for its Ubuntu release, that no image record of devvm's names and that haven't changed in the last hour. A cloud image that one of your own Lima VMs downloaded from that same address can be among them, and Lima downloads it again the next time it needs it. No VM needs its entry once it's made, because Lima copies the disk out of the cache. `limactl prune` deletes the whole cache, your own downloads included, so devvm never runs it.

## Outside devvm's folders

devvm also changes a few things outside its own folders:

- **The Keychain:** on macOS, the [secrets](../safety/network.md#saving-and-binding-secrets) you save are Keychain items under the service `devvm`. `devvm secret rm` deletes one.
- **`~/.ssh/config`:** when you let it, devvm adds the line `Include ~/.local/share/devvm/ssh_config` so that ssh reaches VMs by name ([SSH and editors](ssh.md#ssh-access-by-name)).
- **Time Machine:** on macOS, devvm keeps each VM's `run/ca/` out of Time Machine's backups, because a copy of a key elsewhere could impersonate any site to that VM.
- **devvm itself:** the install script puts each version in `~/.local/lib/devvm/<version>` and links `~/.local/bin/devvm` to it. A newer install removes older versions that nothing runs ([upgrading](upgrading.md#upgrading)). Homebrew keeps devvm in its own folders.

[Uninstalling](upgrading.md#uninstalling) removes all of it.
