---
title: Coding agents
explainer: Run Claude Code and the other coding agents in a VM
weight: 1
description: "devvm runs coding agents such as Claude Code in VMs, with each agent's settings and logins from devvm's own store on this machine but never your history. This page covers which agents devvm supports, devvm's agent store and what each VM gets from it, the agents' logins and their history."
---

## Running an agent

An agent runs where you start it, so you start it in the VM's shell, not on this machine. On this machine, devvm opens the VM's shell, and the VM's prompt starts with `(devvm)`:

```console
~/some-project $ devvm
(devvm) ~/some-project $ claude
```

In the VM the agent sees only your project folder, and your agents' settings from [devvm's agent store](#devvms-agent-store), which it gets read-only. Whatever it runs, such as a script a package installs, runs in the VM too. You don't install any agent on this machine, because devvm installs each one in the VMs that have it.

### The guide in every VM

Every VM has a guide to devvm for the agents in it, at `/usr/share/doc/devvm/README.md`, and `devvm-help` in the VM prints it. It tells an agent what a devvm VM is, how its network tiers and secrets work, where its installs go, and which commands to ask you to run on this machine, such as `devvm network allow` for a blocked host or `devvm agent login` for a login. The guide is the same in every VM, so it says nothing about the VM's own tier, ports or layers. Each launch installs it again whenever it differs from devvm's, and it belongs to root and is read-only, so nothing that the VM's user runs without `sudo` can change what the agents read, and a change made with `sudo` lasts only until the next launch. Ask your agent to read it, or to run `devvm-help`, when it doesn't know where it's running. devvm adds nothing about it to the agents' instruction files.

## Pasting images

An agent in the VM can't see this machine's clipboard, so an image you copied, such as a screenshot, gets to it through devvm paste, which sends the image into the VM and copies its path there for you to paste:

1. Copy an image on this machine, such as a screenshot, copy pages in a PDF viewer such as Preview, or copy a file in Finder or your file manager.
2. In a terminal on this machine, run `devvm paste` in the project folder. devvm says where the image is in the VM, such as `/tmp/devvm-paste/paste-20261109-153012-7f3a9c01.png`, and puts that path on your clipboard. If putting it there fails, devvm warns you and still says where the image is, so you can paste that path yourself.
3. Paste the path into the agent with your terminal's paste shortcut, which is Cmd+V on macOS and usually Ctrl+Shift+V on Linux. Ctrl+V on its own asks the agent to read the VM's clipboard, and the VM has none.

Claude Code, Codex and opencode attach an image whose path you paste, and pi reads it with its `read` tool. The VM must be running, because devvm paste is for an agent that runs in it, and devvm refuses a stopped one.

devvm reads only an image, a PDF or a copied file's reference from the clipboard, never text, so a password you copied never reaches it. It checks that an image's or a PDF's bytes are the type the clipboard claims. It takes PNG, JPEG, GIF and WebP images, and on macOS it also takes a TIFF, which many Mac apps copy, and sends it as a PNG. An image, a PDF or a file over 64 MiB is refused.

The app you copied from hands over what you copied when devvm asks for it, and devvm waits for it at most 30 seconds. If the app doesn't answer in that time, devvm stops waiting, sends nothing and says so. Copy it again, or wait until the app responds, then run devvm paste again. On macOS, devvm reads the clipboard with `osascript`, which reaches the clipboard of the session you're logged in to, so run devvm paste in a terminal on that Mac, not over ssh.

What devvm gets depends on the app you copied from. A browser's Copy Image hands over a PNG, never the original JPEG, GIF or WebP, and Safari hands over a TIFF, which devvm makes a PNG, so an animated GIF arrives as one still frame. To send the original, save it and copy the saved file instead. Text is never sent, so paste text, such as an error message, into the agent's terminal yourself.

A PDF on the clipboard, such as pages you copied in Preview, is sent as it is, as a `.pdf` file, because most models read PDFs. A PDF viewer puts a picture of what you copied beside the PDF, and devvm sends the PDF, never the picture. When you copy a page's thumbnail in Preview, Preview also puts a temporary file of its own on the clipboard, and devvm sends the PDF without asking about that file.

A copied file is sent the same way, under a name of devvm's that keeps only the file's extension, when it's letters and digits alone and at most 16 of them. So `my shot (1).png` becomes `paste-20261109-153012-7f3a9c01.png`, and `Makefile` becomes `paste-20261109-153012-7f3a9c01`. Nothing else of the file's name reaches the clipboard, so the path you paste into a terminal is devvm's own and can't run anything, even when the VM named the file in the project folder.

devvm sends one file at a time. It refuses several files, a folder, and a symbolic link anywhere in the file's path, your own included. A file you copy may be in the project folder, which the VM can write, and a link planted there could point at a file of yours. The only links devvm follows are the system's own, which root owns, such as macOS's `/tmp`, so copy the file from the folder it's really in.

On macOS, Finder puts the copied file's icon on the clipboard as an image beside the file, and devvm sends the file, not the icon. If the file is gone by the time you run devvm paste, devvm says so and sends nothing, rather than the icon. Text is never taken for a file, even when it's a path, so copy the file itself in Finder.

Before devvm sends a copied file, it shows the file's path on this machine and asks, [y/N], because the clipboard can name any file of yours, such as a key in `~/.ssh` that you didn't mean to copy. Answer no and nothing is sent. An image or a PDF is sent without a question, because it's only what you copied. A photo you copy in Photos comes with a file of the Photos library beside it, so devvm asks about that file, as it does about any copied file.

`devvm paste --send-file` sends a file in the project folder without asking, as a script must, because the VM can read that folder already. A file anywhere else is still asked about with `--send-file`, so only you can send it, at a terminal. So is a file whose path goes through a symbolic link, even one of the system's that devvm follows, because the file may not be where its path says. Where devvm can't ask, such as without a terminal, it sends nothing. It names `--send-file` for a file in the folder, the file and the folder for one outside it, and the file for one reached through a link. So don't bind `devvm paste --send-file` to a hotkey to send files from outside the project, because a hotkey has no terminal to ask in, and devvm refuses those files. A clone VM, made with `--clone`, doesn't mount the folder, so `--send-file` answers for none of its files.

`devvm paste --json` does the same and prints what it sent as one object, with `"version": 1` first. It has whether anything was `sent`, which is `false` when you answered no about a file, the VM, and the file's `path` in it (`""` when nothing was sent). Then its `kind` (`image`, `pdf` or `file`), an image's or a PDF's `type` (`image/png`, `image/jpeg`, `image/gif`, `image/webp` or `application/pdf`, and `""` for a file), and a copied file's `source`, its path on this machine (`""` otherwise). Last come its size in `bytes`, and whether the path was `copied` to the clipboard:

```json
{"version": 1, "sent": true, "vm": "devvm-website-3f9a1c2b4d5e6f", "path": "/tmp/devvm-paste/paste-20261109-153012-7f3a9c01.png", "kind": "image", "type": "image/png", "source": "", "bytes": 48213, "copied": true}
```

### The paste folder and your agent's settings

The files go into the VM's paste folder, `/tmp/devvm-paste` unless `config.toml`'s `paste-dir` names another ([the paste folder](settings.md#the-paste-folder)). Each launch makes the folder in the VM, mode 700 and yours, and leaves an existing one as it is. The folder is a fixed path so that you can allow it once in an agent's settings. Claude Code refuses to read a path outside the project folder when its `permissions.blockReadsOutsideWorkingDirectories` setting is on, until you add the folder with `/add-dir /tmp/devvm-paste` or list it in `permissions.additionalDirectories` in your settings, which [devvm's agent store](#devvms-agent-store) keeps for every VM. The files keep their mode, 600, so only the VM's user reads them.

Anything in the VM can read what you send, as it can read any file in the project folder, so paste nothing you wouldn't put in the folder.

### Deleting pasted files

devvm never deletes a pasted file on its own, because Codex reads the image when you submit your message and pi when the model calls `read`, which may be some time after the paste. `devvm paste rm` deletes the files devvm paste left in the folder, after asking in a terminal, and `devvm paste rm --force` deletes them without asking, as a script must. It deletes only the files devvm paste named, and nothing else in the folder, so a `paste-dir` of your own, such as `/home/USER/pastes` where `USER` is your user in the VM, can hold other files.

The VM's `/tmp` is emptied when the VM boots, so with the default folder the files are gone after a restart anyway. With a `paste-dir` of your own, they stay. A command that starts a stopped VM and opens its shell offers to delete them, [y/N], once the VM is set up. Those commands are `devvm`, `devvm restart`, `devvm tools`, `devvm tools add`, `devvm tools rm` and `devvm tools update`, `devvm layer add` and `devvm layer rm`, `devvm update`, and `devvm clone on` and `devvm clone off`. Without a terminal, devvm keeps the files and says nothing, and if deleting them fails, it warns you and opens the shell anyway. Commands that start a VM without opening its shell, such as `devvm code` and `devvm agent save`, don't ask, so run `devvm paste rm` when you want the files gone.

### On Linux

devvm reads and writes the clipboard with `wl-paste` and `wl-copy` in a Wayland session, which `WAYLAND_DISPLAY` names, and with `xclip` in an X11 session, which `DISPLAY` names. It looks for them only in the system's folders, `/usr/bin`, `/bin`, `/usr/local/bin` and NixOS's `/run/current-system/sw/bin`, and never elsewhere on your `PATH`, where a folder the VM can write, such as a project's `node_modules/.bin`, might come first. It tells you which one is missing, and installs nothing itself, so install `wl-clipboard` or `xclip` with your distribution's package manager. Run devvm paste from a terminal in your desktop session, because without either variable there's no clipboard to read. If `WAYLAND_DISPLAY` or `DISPLAY` names a session that's over, as in a tmux window left from an earlier login, devvm tells you that `wl-paste` or `xclip` failed and what it said, rather than that the clipboard is empty. Copying the path leaves `wl-copy` or `xclip` running until you copy something else, because a Linux clipboard's contents live in the program that copied them.

## Supported agents

A new VM gets an agent when you choose it as you customize the VM, or when you name it with `--agents`. A VM that exists gets one from `devvm tools`, which shows the same list with the VM's agents already chosen, or from `devvm tools add`:

```sh
devvm                          # a new VM: customize it, then choose its agents in the list
devvm --agents claude,codex    # a new VM with Claude Code and Codex, without the list
devvm tools                    # the VM that exists: choose its agents in the list
devvm tools add opencode       # the VM that exists: add opencode without the list
```

In the VM, you run each agent by its name in the table, such as `claude`.

| Name | Agent | What devvm adds | Allowlist, on the allowlist and none tiers |
| --- | --- | --- | --- |
| `claude` | Claude Code | the newest release, with its auto-update off, since the shared copy is read-only, and its telemetry off | Anthropic's API, Claude's login and Claude Code's downloads: `api.anthropic.com`, `claude.ai`, `downloads.claude.ai`, `platform.claude.com` and `mcp-proxy.anthropic.com` |
| `codex` | Codex | the newest release, with its analytics and metrics off unless your config sets them | the OpenAI API and the ChatGPT login: `api.openai.com`, `chatgpt.com` and `auth.openai.com` |
| `opencode` | opencode | the newest 2.x release, because opencode marks its 1.x releases as its latest on GitHub | opencode's own service and its model catalog: `opencode.ai` and `models.opencode.ai` |
| `pi` | [pi](https://pi.dev) | the newest release, with its check for a newer pi and its install report off, and `fd`, which pi finds files with, in base | pi's model catalog: `pi.dev` |

Claude Code's telemetry hosts aren't on its allowlist, because blocking them costs nothing. opencode and pi talk to the model provider you choose, so on the allowlist tier you allow that provider's API host yourself, such as `devvm network allow api.anthropic.com` ([changing rules](../safety/network.md#changing-rules)), unless devvm keeps the agent's login or API key for that provider, because the VM's proxy then allows its host. Every VM sets `OPENCODE_DISABLE_AUTOUPDATE=1`, as it sets `DISABLE_AUTOUPDATER=1` for Claude Code, because the agents are read-only shared tools that can't update themselves, and devvm keeps their versions. For the same reason every VM sets `PI_SKIP_VERSION_CHECK=1`, so pi doesn't ask you to run `pi update`, and `PI_TELEMETRY=0`, which turns off pi's report of its install and the attribution headers it adds to some providers' requests. Claude Code's telemetry and error reports are off with `DISABLE_TELEMETRY=1` and `DISABLE_ERROR_REPORTING=1`. devvm writes every agent's variables into each VM at every launch, whichever agents the VM has, and every shell in the VM gets them, login or not, so an agent that an editor's terminal or `ssh` runs gets them too. pi would download `fd` and `rg` into `~/.pi/agent/bin` whenever they're missing, so base has both, from Ubuntu's or the container image's own packages, and pi finds Ubuntu's and Debian's `fd` by its name there, `fdfind`.

pi's releases are built for glibc alone, so a VM made `--from` an Alpine-based container image can't have pi, and devvm refuses it there ([shared tools and glibc](from.md#shared-tools-and-glibc)). Every other agent runs on Alpine too.

Agents are [shared tools](images.md#shared-tools), like the toolchains, so you can combine them with any layers and with each other, as in `devvm --tools node --agents claude,codex`. devvm follows each agent's newest release, because agents release several times a week, and `devvm update` or `devvm tools update` moves a VM to newer versions ([adding and removing tools](toolchains.md#adding-and-removing-tools)).

### Setup's default agents

`devvm setup` chooses the agents that the list checks for every new VM, and that a new VM gets without a terminal. devvm asks for them on its first run in a terminal and again in `devvm setup`'s questions, and `devvm setup --agents claude,codex` or `devvm setup --agents none` sets them without asking. They're a setting for new VMs only, so a VM that exists keeps its agents ([devvm setup](settings.md#devvm-setup)).

## devvm's agent store

devvm keeps the agents' settings and logins on this machine in a store of its own, and every VM with an agent gets that agent's settings and logins from there. devvm never reads the agents' own files in your home folder for a VM, such as `~/.claude`, `~/.claude.json`, `~/.codex`, `~/.config/opencode`, `~/.local/share/opencode` or `~/.pi/agent`, so a VM gets the same settings and logins whether or not this machine has the agent. The one command that reads those files is [`devvm agent import`](#importing-from-this-machine), which copies them into the store when you run it, and [`devvm agent save`](#saving-a-vms-settings) copies the settings you changed in a VM into the store.

The store has two folders for each agent, and each one holds the agent's files at the same paths below it as the agent keeps them in a home folder:

- **Settings** go in `~/.config/devvm/agents/<agent>/`, such as `~/.config/devvm/agents/claude/.claude/settings.json` and `~/.config/devvm/agents/claude/.claude/skills`. You can edit these files and make or delete these folders yourself, and a VM gets the change on its next launch, as [mounts](#mounts) and [copies](#copies) describe.
- **Logins** go in `~/.local/share/devvm/logins/<agent>/`, such as `~/.local/share/devvm/logins/codex/.codex/auth.json` and `~/.local/share/devvm/logins/claude/.claude.json`, and so does a login that you make inside a VM, such as Codex's ([Codex and opencode logins](#codex-and-opencode-logins)) or Claude Code's in `~/.local/share/devvm/logins/claude/.claude/.credentials.json` ([Claude Code login](#claude-code-login)). Only you can read them, because devvm makes each agent's logins folder yours alone (mode 700) and each login file mode 600. On macOS, devvm keeps each agent's logins folder out of Time Machine's backups.

Each VM gets its agents' settings and logins from the store, but never your history. Transcripts, sessions and databases aren't in the store, and each VM's own stay in that VM. Before you leave an agent running unattended, read [staying safe](../safety/_index.md#the-habits) and [rogue agents](../safety/attack-vectors.md#rogue-agents).

```mermaid
flowchart TD
  home["the agents' files<br>in your home folder"]
  subgraph store["devvm's agent store"]
    folders["settings folders:<br>skills, plugins…"]
    files["settings files<br>and logins"]
  end
  subgraph guest["The VM"]
    links["linked into ~"]
    copies["copies in ~"]
    own["its own history"]
  end
  home -- "devvm agent import,<br>never your history" --> store
  folders -- "mounted<br>read-only" --> links
  files -- "copied on every<br>launch, without<br>your secrets" --> copies
  copies -- "devvm agent save,<br>after a diff and a yes" --> store
  own -- "devvm history export,<br>devvm rm" --> export["a .tar.gz<br>on this machine"]
  export -. "devvm history import,<br>or yes when a new VM<br>offers it" .-> own
```

Each row lists the agent's paths in the VM's home, and the store keeps each one at the same path below the agent's settings or logins folder:

| | Mounted read-only, linked into `~` | Copied in on every launch |
| --- | --- | --- |
| claude | `~/.claude/{skills,agents,commands,output-styles,plugins}` | `~/.claude/settings.json`, `~/.claude/CLAUDE.md`, and from the logins `~/.claude.json` ([merged](#copies), only its settings, account state and MCP servers) |
| codex | `~/.codex/{prompts,skills}` | `~/.codex/{config.toml,AGENTS.md}`, and from the logins `~/.codex/auth.json` |
| opencode | the subfolders of `~/.config/opencode` (not `node_modules`) | the files in `~/.config/opencode` (not lock files), and from the logins `~/.local/share/opencode/auth.json` |
| pi | `~/.pi/agent/{extensions,skills,prompts,themes}` | `~/.pi/agent/{settings.json,keybindings.json,models.json,mcp.json,AGENTS.md,SYSTEM.md,APPEND_SYSTEM.md}`, and from the logins `~/.pi/agent/auth.json` |

### Importing from this machine

When you already use an agent on this machine, `devvm agent import` copies its settings and logins from your home folder into the store, so your VMs get them. With no argument it imports every agent that your home folder has files of, and `devvm agent import claude codex` imports only the agents you name:

```sh
devvm agent import              # copy every agent's settings and logins into the store
devvm agent import codex        # copy only Codex's
devvm agent import --force      # replace what the store has without asking
```

The import copies only the paths in the table above, from `~/.claude`, `~/.claude.json`, `~/.codex`, `~/.config/opencode`, `~/.local/share/opencode` and `~/.pi/agent`, so it never copies your history, such as `~/.claude/projects`, `~/.codex`'s sessions or `~/.pi/agent/sessions`, and never `~/.claude/.credentials.json`. From `~/.claude.json` it keeps only the settings, account state and MCP servers, so your API key and your per-project entries stay out of the store. Claude Code's plugin records name the plugins' folders by their paths, so the store's copies of the JSON files in `~/.claude/plugins` name the plugins' folders in the store, where the VMs mount them. devvm says which agents it imported and where, and an agent you named that your home folder has nothing of gets a warning.

devvm reads each agent's settings inside the agent's own folder, such as `~/.claude`, and it never follows a link out of it. A link to a file inside the folder becomes a copy of that file, and a link that leads out of the folder, by a relative or an absolute path, or to a folder, is left out with a warning that names it. A login file that's a link is left out with a warning too. So when your settings live elsewhere, such as in a dotfiles repo with links into `~/.claude`, copy them into the store yourself.

When the store already has settings or a login of an agent you import, devvm asks in a terminal whether to replace them, with no as the default. Without a terminal, or with `--no-input`, devvm refuses and names `--force`, which replaces them without asking. A replacement writes each file again and takes out what your home folder no longer has, but it keeps the store's folders, so a running VM's mounts still show them. An agent whose settings aren't in your home folder keeps the store's settings, and one without a login there keeps the store's login.

On macOS, Claude Code keeps its own login in the Keychain, and devvm neither reads it nor copies it. You log Claude Code in from a VM instead, with `devvm agent login claude`, and every VM with Claude Code gets that login ([Claude Code login](#claude-code-login)).

opencode 2 keeps its logins in its own database rather than in `~/.local/share/opencode/auth.json`, so when this machine's opencode is version 2 or later, devvm reads its logins with `opencode auth export` and saves them in the store in the format of `auth.json`, the file that devvm copies for opencode 1. It saves the API key of each provider, and your OpenAI login when you made it with one of opencode's Codex logins, the browser or the headless one. Any other login is left out with a warning that names it, such as opencode's Sign in with ChatGPT, whose tokens a VM can't use, or another provider's OAuth login, and you log in from a VM instead, with `devvm agent login opencode` ([logging in with devvm agent login](#logging-in-with-devvm-agent-login)). When `opencode auth export` fails, the import fails too, and the store keeps the login it had.

### Saving a VM's settings

When you change an agent's settings in a VM, for example by installing a plugin or writing a skill there, `devvm agent save` copies them into the store, so every VM with that agent gets them. With no argument it saves every agent that the folder's VM has, and `devvm agent save claude` saves only the agents you name. devvm starts a stopped VM for it.

```sh
devvm agent save          # show what changed in the VM's agents' settings, then save them
devvm agent save codex    # save only Codex's
```

The save reads only the settings in the table above from the VM's home, and never the logins, so it never reads your history, `~/.claude/.credentials.json`, `~/.codex/auth.json`, opencode's `auth.json` or pi's. From the VM's `~/.claude.json` it keeps only the settings, account state and MCP servers, as the import does, and the VM leaves out its per-project entries before it sends anything. devvm puts that part in the store's logins folder, where the import keeps it too. A folder that the store already has is mounted read-only in the VM, so the agent there can't change it, and the save leaves it as it is. A folder that the store doesn't have yet is the VM's own until you save it, and once the VM next starts with the store's new folder mounted, its setup links that folder in place of its own ([mounts](#mounts)).

Before it saves anything, devvm shows what would change. For each folder it lists the files it adds (A), changes (M) and removes (D), and then it shows a diff of each file it adds or changes against the store's copy, with a line when a file's mode changes. The diff shows the settings files and `~/.claude.json` as a VM gets them, written out again with `devvm-redacted` in place of the values that look like secrets, so it never prints your secrets, and it says so when only those values or the file's layout changed. Every character that a terminal wouldn't show, such as a control character, a zero-width character or a character that reverses the text, appears in the diff as its code, such as `\u{200b}`, so a VM can't hide a line from you. The diff names a binary file without showing it, and it shows a file of more than 1 MiB or 10,000 lines, and every line past the first 20,000 of the whole diff, only as a count. Then devvm says that every VM with these agents runs the hooks, the MCP servers' commands and the plugins you save, and it asks whether to save them, with no as the default. Read the diff before you answer, because a hook or a plugin that a [rogue agent](../safety/attack-vectors.md#rogue-agents) added in one VM runs in every VM once you save it. devvm agent save always asks, and no flag answers for you, so without a terminal, or with `--no-input`, it shows what would change and then refuses.

devvm treats everything the VM sends as untrusted, because the agent in the VM could have changed any of it. It reads at most 128 MiB from the VM, in at most 20,000 files, folders and links, none of the files larger than 16 MiB, and when the VM sends more, or sends a path outside the agents' settings or a name with a character that a terminal wouldn't show, devvm saves nothing. Each file it saves is yours alone (mode 600), or mode 700 when it was executable in the VM, so it never keeps a set-user-ID or set-group-ID bit. A link in a settings folder that leads to a file in the same folder becomes a copy of that file. devvm leaves out any other link, any other kind of file, such as a socket, and a settings file that it can't parse whole, such as a `settings.json` that isn't JSON, and it warns you about each. When it leaves out a file in a folder, the store keeps its own copy of that file. A settings file that the VM doesn't have stays in the store. A folder that the VM sends replaces the store's folder whole, so devvm writes each of its files again and takes out the files that the VM's folder doesn't have, but it keeps the store's folder itself, so a running VM's mounts still show it.

The VM's [copies](#copies) of the settings files and of `~/.claude.json` hold placeholders, such as `devvm-redacted`, where the store has your secrets. devvm puts the store's secret back in place of each placeholder, as long as the VM's copy had that placeholder in that place and nothing around it changed, such as the URL of the MCP server that a header is for. When something around a placeholder changed, devvm leaves the whole file out and warns you, so a VM can't send a secret it never saw to another host, and you can make that change in the store's file yourself. devvm also leaves out any other file that holds a placeholder, such as a skill. No VM gets opencode's files in `~/.config/opencode` that are neither settings nor Markdown, such as a `.env`, so devvm leaves out the VM's own file of that name and keeps the store's.

## Mounts

devvm mounts the store's settings folders that exist when it creates the VM read-only, at the same path as in the store on this machine, and links them from the VM's home. Absolute paths in them, such as Claude Code's plugin paths, still work. This machine enforces read-only, so even root in the VM can't write to them. devvm mounts each folder whole, whatever the VM's network tier, so the VM reads every file in it, a `.env` or a key file in a plugin's folder included, and the git config of a private plugin marketplace whose URL holds a password. devvm takes no secrets out of these folders, because they hold plugins, skills and their scripts that the agent runs as they are. So keep secrets out of these folders, as you keep them out of the project folder ([why devvm mounts them whole](../internals/network.md#what-stays-in-the-vms-copies)). The VM follows a symlink in these folders among its own files, so a link to a file outside the folder, by a relative or an absolute path, doesn't reach that file on this machine, on macOS's virtiofs mounts and on Linux's 9p mounts alike. devvm doesn't mount a folder whose path has a comma, on any backend, and it warns you. Lima can't mount such a path ([Lima's limits](../backends/lima.md#limits)).

When you make or delete one of these folders in the store later, by hand or with `devvm agent import`, devvm notices on the VM's next launch and names the folder. A VM's mounts change only when it starts, so a stopped VM gets the new mounts as the launch starts it. For a running VM, devvm asks in a terminal whether to restart it now, which stops anything running in it, in the one question that covers every change waiting for the VM's next start, and because the mounts are among those changes, the default answer is yes ([restarting](vms.md#restarting-and-stopping)). If you answer no, or there's no terminal, or you pass `--no-input`, the mounts change when the VM next starts, for example with `devvm restart`.

## Copies

devvm copies the store's files into the VM's home on every launch, over the VM's own. On every [network tier](../safety/network.md#network-tiers), the copies of the logins and settings files have your secrets taken out, and the VM's proxy puts them into requests instead ([agents' logins and settings](../internals/network.md#agents-logins-and-settings)).

devvm merges the settings and MCP servers from the store's `.claude.json` into the VM's own `~/.claude.json`, so servers added in the VM stay ([MCP servers](../internals/network.md#mcp-servers)). Your API key, your answers about API keys and your per-project entries never reach the VM. devvm also replaces the secrets in the agents' settings files with `devvm-redacted`, and names what it replaced. Those are the values under keys that look like secrets, every env value and header, and the secrets in URLs and in arguments. In other text, such as a hook's command, devvm replaces the secrets in its URLs, the keys that start like a known key, such as `sk-` or `ghp_`, and the credential after `Bearer` or `Basic`, and keeps the words around them ([keys in settings files](../internals/network.md#keys-in-settings-files)). pi's settings files that can hold a key are `settings.json`, `models.json` and `mcp.json`, so a provider's `apiKey` in `models.json` reaches the VM as `devvm-redacted`, and pi's `keybindings.json` and Markdown files reach it as they are. From the `env` of Claude Code's `settings.json`, the VM gets only Claude Code's own settings, such as `ANTHROPIC_MODEL`, the `CLAUDE_CODE_*`, `DISABLE_*` and `MAX_*` variables and those ending in `_TIMEOUT_MS`, but none whose name marks it as a credential, such as `ANTHROPIC_AUTH_TOKEN`. It also gets the values that only name where a secret is, base URLs without their passwords, `true`, `false` and short numbers. Every other env value, and every env value of an MCP server, becomes `devvm-redacted`. When devvm can't parse a settings file to take its secrets out, the VM doesn't get a copy of it, and neither does it get opencode's files in `~/.config/opencode` that are neither settings nor Markdown, such as a `.env`. devvm names each file it doesn't copy, and when it can't make the copy of a login, it says why.

### Claude Code login

You log Claude Code in once, from inside any VM that has it, and devvm keeps the login on this machine for every VM with Claude Code, so this machine never needs Claude Code:

```sh
devvm agent login claude                # log Claude Code in to your Claude subscription
devvm agent login claude --setup-token  # get a long-lived Claude Code token with claude setup-token instead
```

devvm starts the folder's VM first if it's stopped, as `devvm host` does, and then runs Claude Code's own login in the VM, on your terminal: `claude auth login --claudeai`, or `claude setup-token` with `--setup-token`. Claude Code shows an address, which you open in a browser on any machine. When you approve the login there, the browser shows a code, which you paste back at Claude Code's prompt, so devvm forwards no port. The VM's proxy saves the login as it reaches Claude's token endpoint, and Claude Code in the VM gets only placeholders. devvm then sets the VM up again, so it uses the login at once, and every other VM with Claude Code gets it from its next launch. When the login ends without a new login in the store, devvm says so and fails.

devvm keeps one Claude Code login, either your subscription's login or a Claude Code token, and the newest replaces the other, so every VM uses the account you logged in to last:

- **Your subscription's login**, from `devvm agent login claude` or a `claude /login` that you run in a VM yourself, goes in devvm's agent store, in `~/.local/share/devvm/logins/claude/.claude/.credentials.json`, only yours to read. The VM's `~/.claude/.credentials.json` holds placeholders, with no refresh token and an expiry in 2099, so Claude Code in the VM never refreshes the login itself. The VM's proxy puts the login into the VM's requests to `api.anthropic.com`, and refreshes it on this machine when it's due, through the company proxy when `config.toml` names one. devvm keeps only a login that can use Claude's models and can't make API keys, so a login to an Anthropic Console account, which Claude Code uses to make an API key, fails in a VM, and no API key reaches it. Only a VM that has Claude Code can log it in.
- **A Claude Code token**, from `devvm agent login claude --setup-token` or a `claude setup-token` that you run in a VM yourself, goes with the other [secrets](../safety/network.md#saving-and-binding-secrets). You can also save one with `devvm secret set claude` and paste the token that `claude setup-token` printed on another machine (`sk-ant-oat01-…`), not the code the browser shows. devvm neither saves nor sends a Claude Code token that isn't a setup token. Every VM with Claude Code gets the token in its environment as `CLAUDE_CODE_OAUTH_TOKEN`, holding a placeholder of its own, and its proxy puts the token into its requests. When you set up a token in a VM, `claude setup-token` prints that VM's placeholder, not the token, because devvm already saved it.

A VM gets either `~/.claude/.credentials.json` or `CLAUDE_CODE_OAUTH_TOKEN` from devvm, never both, because Claude Code would use the token in its environment over the login in its file. A VM that started with the login devvm replaced keeps working, because its proxy puts the login devvm keeps now in place of either placeholder. A `CLAUDE_CODE_OAUTH_TOKEN` in devvm's own environment on this machine wins over both, as it does in Claude Code, so VMs get that token, and `devvm agent login claude` warns you when it's set. The VM gets placeholders whatever its network tier.

Anthropic doesn't document the requests that Claude Code's login sends, so devvm follows the ones Claude Code sends today, and a new Claude Code release can change them. When a login in a VM stops working, run `devvm agent login claude` again, or save a Claude Code token instead.

### Codex and opencode logins

On every network tier, the VM's `auth.json` holds placeholders, and its proxy holds the real API keys and logins. The proxy reads them from the login files in devvm's store as it needs them and refreshes ChatGPT logins there, so when the agent in the VM stops getting answers, the proxy's own log, `proxy.log` in `~/.local/share/devvm/vms/<hash>/logs/`, may say that refreshing a login failed or that you're no longer logged in that way. To log in again, run [`devvm agent login codex` or `devvm agent login opencode`](#logging-in-with-devvm-agent-login) in the VM's folder, which also gives that VM the new login's placeholders, and the other VMs get them on their next launch. The proxy puts in opencode's API keys for the providers that [API keys the proxy puts in](#api-keys-the-proxy-puts-in) lists, and its OpenAI and GitHub Copilot logins. The VM's entry for such an API key holds only its type and the placeholder, so the entry's other fields, such as its `metadata`, stay on this machine.

The VM's copy of Codex's `auth.json` holds only what Codex needs for its API key and its ChatGPT login: the placeholders, how it logs in, when it last refreshed, and the IDs of the account and the login. devvm keeps every other field on this machine, because the VM's proxy can't put it in, and it names those fields at every launch. That includes Codex's other ways to log in, such as a personal access token or an Amazon Bedrock key, so Codex in a VM can't use them. To use Codex in a VM, log in with ChatGPT or an OpenAI API key ([logging in with devvm agent login](#logging-in-with-devvm-agent-login)). devvm treats Claude Code's account state in `~/.claude.json` the same way, so the VM gets only the account fields that Claude Code writes there today, and devvm names any other field at every launch.

devvm keeps every other entry of opencode's `auth.json` on this machine too, because devvm doesn't know which hosts it's for, so the VM gets that entry without its key or its tokens, and devvm warns you about each at every launch. That's an API key for any other provider, such as Azure's or a local model server's, a well-known login, which an organization's own opencode config sets up, and any other OAuth login, such as Copilot Enterprise's. The VM keeps only the fields of a withheld entry that name the account or how it logs in, such as its type, its `enterpriseUrl` and its expiry, so opencode in the VM doesn't use that provider. To use an OAuth login's provider in the VM, log in to it with an API key on this machine and import the login again.

To give the VM one provider's API key as it is, such as a local model server's that the VM's proxy can't put in, list the provider under `opencode` in `config.toml`'s `[pass-keys]` table ([config.toml](settings.md#configtoml)), as you would for [pi](#pi-logins). The provider is its entry's name in opencode's `auth.json`, such as `ollama`. Every VM with opencode then gets that entry whole, its `metadata` included, and opencode 2 in the VM imports it into its database too, and devvm says so at each launch. It applies only to entries with an API key, because a VM never gets a login's tokens, and only to providers whose keys the proxy doesn't put in, because a key the proxy puts in is always its placeholder:

```toml
[pass-keys]
opencode = ["ollama"]
```

So when opencode in a VM can't reach a provider whose key works on this machine, such as a local model server's, and devvm's warning at launch names that provider, add it to `[pass-keys]` and run devvm in the folder again.

#### Logging in with devvm agent login

`devvm agent login` logs Codex or opencode in from inside this folder's VM, so this machine never needs either agent, and devvm's agent store keeps the login for every VM with that agent. Claude Code's login is `devvm agent login claude` ([Claude Code login](#claude-code-login)), and pi's is an API key alone ([pi logins](#pi-logins)):

```sh
devvm agent login codex                                    # log Codex in to ChatGPT in a browser on this machine
devvm agent login opencode --device-code                   # log opencode in with a code, in a browser on any machine
devvm agent login codex --api-key                          # save Codex's OpenAI API key, asking for it
devvm agent login opencode --api-key --provider anthropic  # save opencode's Anthropic API key
```

devvm starts the folder's VM first if it's stopped, as `devvm host` does, and the VM must have the agent, which `devvm tools add codex` gives it. Then devvm runs the agent's own ChatGPT login in the VM, on your terminal: `codex login` for Codex, and `opencode auth login openai --method chatgpt-browser`, opencode's Codex browser login, for opencode. The agent shows an address, which you open in a browser on this machine. When you approve the login there, the browser comes back to `localhost:1455`, where the agent's own server in the VM waits, so devvm forwards this machine's `localhost:1455`, and `localhost:1457`, which Codex uses when 1455 is taken in the VM, to the same ports in the VM. The forward lasts only while the login runs, and it ends when the login ends, fails or you press Ctrl+C, which ends the agent's login in the VM too. When another program on this machine has port 1455, devvm refuses and names `--device-code`.

With `--device-code`, the agent shows a code instead, which you type into a browser on any machine, and devvm forwards nothing. Codex's device code login is `codex login --device-auth`, which your ChatGPT account's security settings must allow, and opencode's is `opencode auth login openai --method chatgpt-headless`, its Codex device code login.

When the agent logs in, the VM's proxy saves the login in the store, and the agent in the VM gets placeholders, as [the next section](#logins-made-in-a-vm) describes. devvm then says what the store saved and the account's email, and the VM uses the login at once. When the login ends without a new login in the store, for example because you chose another way to log in, devvm says so and fails, and `devvm network log` shows each login the proxy saved. Codex in a VM keeps its login in `~/.codex/auth.json`, never in a keyring, because devvm sets `cli_auth_credentials_store = "file"` in the VM's `~/.codex/config.toml` on every launch.

With `--api-key`, devvm asks for the API key without showing it, or reads it from a line of input with `--stdin`, and saves it in the store itself, so the key never goes into the VM. Codex's key is an OpenAI key, and it replaces Codex's ChatGPT login in the store, as `codex login --with-api-key` does. opencode's key is for the provider that `--provider` names, by one of the IDs in [API keys the proxy puts in](#api-keys-the-proxy-puts-in), and the store keeps opencode's other providers beside it. devvm then gives the running VM its copy of the login, with a placeholder in place of the key, and the VM's proxy puts the key in at once. opencode keeps its logins in its own database in the VM, and it reads its `auth.json` only when it first makes that database, so when opencode has already run in the VM, devvm also gives it the key's placeholder with `opencode auth import`. devvm reads at most 1 MiB of what that import prints in the VM, and when it prints more, devvm stops it and fails, so running devvm in the VM's folder tries again. Every other VM with opencode gets the key's placeholder at its next launch, the same way, and any credentials that you added to opencode in a VM yourself stay in its database.

opencode's Anthropic login is an API key, because opencode itself has no other way to log in to Anthropic. opencode's Claude Agent provider uses Claude Code's own login instead, so it works in a VM that has Claude Code too, with the login that `devvm agent login claude` saves.

#### Logins made in a VM

When Codex or opencode in a VM logs in to OpenAI, whether `devvm agent login` runs it or you run it yourself, the VM's proxy saves the login in devvm's store, in the agent's own `auth.json`, and the agent in the VM gets only placeholders, so on every tier but open the VM never holds the login's tokens. The VM you logged in from uses the login at once, and every VM with that agent gets it from its next launch. opencode reads its `auth.json` only when it first makes its database, so in a VM where opencode has already run, devvm adds the store's logins to opencode's database at each launch when they changed since the VM last got them, and makes them the ones opencode uses. Credentials that you added to opencode in the VM yourself stay in its database, labeled with your names rather than devvm, and `opencode auth switch` changes back to one of them. A login that shows a code to type into a browser on any machine works in a VM as it is, such as `codex login --device-auth` and opencode's Codex device code login. opencode's Sign in with ChatGPT doesn't work in a VM, because it gets its tokens from another endpoint, whose replies the VM's proxy refuses so that they never reach the VM, so choose one of opencode's Codex logins instead, as `devvm agent login opencode` does. A new login replaces the one in the store, together with Codex's API key, so every VM with the agent uses the account you logged in to last. A VM saves a login only for an agent that it has, so a VM without Codex can't replace the store's Codex login, and a VM without opencode can't replace opencode's. When a VM sends a login for an agent it doesn't have, such as one that a program installed in the VM itself, its proxy refuses the login, devvm saves nothing, and the proxy's own log says so. In a VM with both Codex and opencode, devvm tells their logins apart by the program that sends them. `devvm network log` shows a `login` line for each login it saved. On the open tier, a program that goes around the VM's proxy gets the login's tokens itself, and devvm saves nothing ([logins made in a VM](../internals/network.md#logins-made-in-a-vm)). Claude Code's logins in a VM are saved in the same way ([Claude Code login](#claude-code-login)).

### pi logins

pi in a VM uses API keys that devvm keeps on this machine, for the providers whose keys the VM's proxy puts in, which [API keys the proxy puts in](#api-keys-the-proxy-puts-in) lists. You save one with `devvm agent login pi --api-key --provider PROVIDER`, because pi's login always takes both flags, or you import the API keys in your own `~/.pi/agent/auth.json` with `devvm agent import pi`:

```sh
devvm agent login pi --api-key --provider anthropic   # save pi's Anthropic API key, asking for it
devvm agent import pi                                  # copy pi's settings and auth.json from this machine
```

The login saves the key in the store's `~/.local/share/devvm/logins/pi/.pi/agent/auth.json`, in pi's own format, beside pi's other providers, as opencode's `--api-key` does ([logging in with devvm agent login](#logging-in-with-devvm-agent-login)). It needs a VM with pi in this folder, which it gives its copy of the login at once, and every other VM with pi gets it at its next launch. The VM's `~/.pi/agent/auth.json` holds a placeholder in place of each of those keys, and the VM's proxy puts the key into pi's requests to that provider's hosts, on every network tier, and allows those hosts, as the table in [API keys the proxy puts in](#api-keys-the-proxy-puts-in) describes. The VM's entry for such a provider holds only its type and the placeholder, so it gets none of the entry's other fields, such as its `env`, whose values pi uses before the environment's, and the store keeps them on this machine.

devvm keeps every other entry of pi's `auth.json` on this machine too, and the VM gets that entry without its key, its `env` or its tokens, and devvm warns you about each. That's an API key for any other provider, such as a local model server's, a key that's a command for pi to run, such as `!security find-generic-password`, and a key that pi fills in from a variable, such as `$KEY` or `${KEY}`, even for a provider whose key the proxy puts in, because devvm can't tell what pi would send. The VM keeps only the fields of a withheld entry that name the account or how it logs in, such as its type, its `accountId` and its expiry.

To give the VM one provider's API key as it is, such as a local model server's that the VM's proxy can't put in, list the provider under `pi` in `config.toml`'s `[pass-keys]` table ([config.toml](settings.md#configtoml)). Every VM with pi then gets that entry whole, its `env` included, and devvm says so at each launch. It applies only to entries with an API key, because a VM never gets a login's tokens, and only to providers whose keys the proxy doesn't put in, because a key the proxy puts in is always its placeholder:

```toml
[pass-keys]
pi = ["ollama"]
```

pi's own logins with an account, which `/login` and `pi auth login` make for a subscription, such as Claude's, ChatGPT's or GitHub Copilot's, aren't among the logins devvm keeps, so save the provider's API key instead. Don't log pi in with an account inside a VM either. pi's Claude and ChatGPT logins use the same token endpoints as Claude Code's, Codex's and opencode's logins, whose replies the VM's proxy captures for those agents, and devvm never keeps such a login as pi's. In a VM that also has the matching agent, Claude Code for a Claude login and Codex or opencode for a ChatGPT login, the proxy saves pi's login as that agent's own, in place of the one devvm keeps for every VM with that agent. In a VM with none of them, the proxy refuses pi's login and devvm saves nothing ([logins made in a VM](#logins-made-in-a-vm)).

#### pi's settings

The VM's copy of pi's `settings.json` leaves out three keys, because each would undo what devvm sets in the VM, and devvm names the ones it left out. `httpProxy` would send pi around the VM's proxy, `sessionDir` would keep pi's sessions outside `~/.pi/agent/sessions`, where history exports look for them, and `selfUpdateCommand` would run a command of the file's on `pi update`. When `devvm agent save pi` saves the VM's `settings.json`, its diff shows those keys as taken out, so save it only when you mean to drop them from the store's copy too.

When the store's `settings.json` sets `defaultProjectTrust` to `always`, devvm warns you at each launch. That setting makes pi load a project folder's own `.pi` extensions, settings and MCP servers without asking, and they run in the VM with pi, so set it to `ask` in devvm's agent store unless you trust every project you open.

### API keys the proxy puts in

The VM's proxy puts in opencode's and pi's API keys for the providers in this table. The ID is the provider's entry in the agent's `auth.json` and the value that `devvm agent login --provider` takes. The VM's copy of the login holds a placeholder in place of each of these keys, and on every network tier the proxy swaps it for the key in the headers that the table names, in requests to the provider's hosts alone, so any other host gets only the placeholder. Where the table names a path, the proxy swaps it only in requests for paths that start with it, so the rest of that host gets the placeholder too. The proxy also allows those hosts on port 443, as it allows the hosts of every secret it puts in, so you don't allow them yourself:

| ID | Provider | Hosts | Paths | Headers |
| --- | --- | --- | --- | --- |
| `anthropic` | Anthropic | `api.anthropic.com` | any | `x-api-key` |
| `deepseek` | DeepSeek | `api.deepseek.com` | any | `Authorization` |
| `google` | Google Gemini | `generativelanguage.googleapis.com` | any | `x-goog-api-key` |
| `groq` | Groq | `api.groq.com` | any | `Authorization` |
| `mistral` | Mistral | `api.mistral.ai` | any | `Authorization` |
| `openai` | OpenAI | `api.openai.com` | any | `Authorization` |
| `openrouter` | OpenRouter | `openrouter.ai` | `/api/` | `Authorization`, `x-api-key` |
| `xai` | xAI | `api.x.ai` | any | `Authorization` |

OpenRouter's key goes in either of its two headers, because pi sends it in `x-api-key` for OpenRouter's Anthropic models, as Anthropic's own API takes a key, and in `Authorization` for every other model. The proxy swaps the placeholder only when the header holds the placeholder and nothing else, or for `Authorization`, `Bearer`, one space and the placeholder, which is how opencode and pi send each of these keys. A header that holds anything else beside the placeholder, or a second header of the same name, keeps the placeholder, so code in the VM can't surround your key with text of its own, which a provider that quotes part of a key it refuses could show back to it.

The placeholder for an OpenAI key starts with `sk-`, as an OpenAI API key does, because pi takes an OpenAI key that starts any other way for a Sign in with ChatGPT login and then leaves settings such as the longest reply it wants out of its requests. The Google Gemini key is the Gemini API's, for Google AI Studio. Vertex AI logs in with a Google Cloud account or a service account instead, so devvm withholds its logins, as it does any other provider's. These providers' keys never need `[pass-keys]`, and when it lists one of them all the same, the VM still gets the key's placeholder, not the key, and devvm notes at each launch that you can take the provider out.

The placeholder keeps a key out of the VM only from the launch that first gives the VM the placeholder. A VM that was given one of these keys as it was before then, such as through `[pass-keys]` when the proxy didn't put that provider's key in, may still hold copies of it. They can be in opencode's database, its write-ahead log and its freed pages, in the disk's free space and anywhere that the VM's own code copied the key. While opencode in the VM fails to import the placeholder, devvm's record of opencode's last import, `~/.local/state/devvm/opencode-import.json`, holds the key too. Those copies still work wherever they are, so when you don't trust that VM, make a new key at the provider, save it with `devvm agent login AGENT --api-key --provider PROVIDER` and revoke the old one.

With some of these keys, code in the VM can make a credential of its own that works without the proxy. Google Gemini's key gets an ephemeral token for Gemini's Live API, which lasts up to about 20 hours and allows as many uses as the VM asks for. xAI's key gets a realtime client secret that lasts as long as the VM chooses, and OpenAI's key gets one that lasts up to 2 hours. opencode's experimental voice mode asks for these itself, so the proxy allows those requests ([attack vectors](../safety/attack-vectors.md#secrets-and-keys-the-vm-may-use)). Some of these providers' own tools also fetch the URLs that a request names, such as Google Gemini's URL context and Search grounding, xAI's live search, Groq's compound models and Mistral's agents with web search, so code in the VM can reach hosts that the allowlist doesn't have through them, as it can through Anthropic's and OpenAI's.

## History

Each VM's transcripts and sessions stay in that VM. `devvm history export [FILE]` saves them, and `devvm rm` saves them before it deletes a VM that has an agent. The export is a `.tar.gz` of `~/.claude`, `~/.claude.json`, `~/.codex`, `~/.local/share/opencode`, `~/.local/state/opencode` and pi's sessions, `~/.pi/agent/sessions`, in the VM, written to FILE or to `~/.local/share/devvm/exports/<vm>-<date>-<time>.tar.gz`. An export in that folder gets a `<vm>-<date>-<time>.folder` file beside it, which names the VM's project folder and the VM's [network tier](../safety/network.md#network-tiers) when devvm wrote the export.

`devvm history` lists the exports in `~/.local/share/devvm/exports`, oldest first, in a table with a header. It shows each export's file, the VM it's of, that VM's folder, the date and time devvm wrote it, and its size, with `-` for a folder devvm has no record of. Exports you wrote to a FILE of your own aren't listed, and `devvm history` changes nothing and starts no VM.

### devvm history --json

`devvm history --json` prints the same list as one JSON object, as [output for scripts](command-line.md#output-for-scripts) describes. Each of the `exports` has its `file`, its `vm`, the VM's `folder`, which is `""` when devvm has no record of it, the `date` devvm wrote it in RFC 3339 with this machine's time zone, and its `size` in bytes.

```json
{
  "version": 1,
  "exports": [
    {
      "file": "/Users/you/.local/share/devvm/exports/devvm-website-71a99b1076d92c-20261001-120000.tar.gz",
      "vm": "devvm-website-71a99b1076d92c",
      "folder": "/Users/you/code/website",
      "date": "2026-10-01T12:00:00+02:00",
      "size": 1048576
    }
  ]
}
```

After `devvm rm`, an export is the only copy of that VM's history, so devvm keeps the exports until you ask it to delete them. `devvm gc --history` deletes the exports in that folder, and `devvm gc --history --older-than 30d` deletes only those written 30 days ago or more. An export's age counts from the date and time in its name, which is this machine's local time when devvm wrote it, so across a change to or from daylight saving time an export reaches an age such as `30d` an hour earlier or later, for `--older-than` and for the `history.age` [retention setting](settings.md#retention) alike. gc lists each one by its VM and date first, and in a terminal it asks. Exports you wrote to a FILE of your own stay where they are.

The agents' logins aren't in the export, which leaves out `~/.claude/.credentials.json`, `~/.codex/auth.json`, and opencode's `auth.json`, `account.json`, `mcp-auth.json` and `bin`, and holds nothing of `~/.pi/agent` but its sessions, so pi's `auth.json` stays out too. opencode keeps its sessions and its logins together in one database, `~/.local/share/opencode/opencode.db`, so the export holds a copy of that database without its logins. The VM makes the copy with `sqlite3`, which every base image has, and the copy has no logins, API keys or opencode account in any part of the file, not even a key you typed into opencode in the VM, while the VM's own database keeps them ([how](../internals/design.md#opencodes-database-in-history-exports)). The export never holds the database's `-wal`, `-shm` or `-journal` files, which can hold logins that opencode saved last. When the VM has no `sqlite3`, such as a VM made from an image built without it, the export leaves the database out, so opencode's sessions aren't in it, and the run log says so. devvm starts a stopped VM for the export as a launch does, so it locks the folder's `.git` again and checks the VM's confinement first, and the VM stays running. devvm writes the file only once the export is complete, so a failed export leaves no file and leaves an existing FILE as it was. An export holds at most 2 GiB, because a VM could otherwise write until this machine's disk is full. When the VM sends more, devvm stops the export and keeps nothing of it, so `devvm history export` fails, while `devvm rm` and the commands that make a VM again warn that they kept no export and go on without one. Only you can read the file, and devvm makes the exports folder yours alone each time it writes one there, whatever its mode was before. When FILE is in a folder of your own, that folder keeps its mode. If the export fails, `devvm rm` keeps the VM, and `devvm rm --no-export` deletes it without exporting. `devvm rm VM`, for a VM whose folder is gone, exports in the same way, but only from a running VM or a `--clone` VM, because starting any other would make its folder again ([a VM whose folder is gone](vms.md#a-vm-whose-folder-is-gone)).

### Importing history

`devvm history import [FILE]` puts an export back into this folder's VM, so the agents' transcripts and sessions carry over when you delete a VM and make it again. Without FILE, it imports the newest export in `~/.local/share/devvm/exports` that devvm saved for this folder, going by the `.folder` file beside each one, and it never picks an export made for another folder. With FILE, it imports that `.tar.gz`, such as one you saved with `devvm history export FILE`. devvm starts a stopped VM for the import, as it does for an export. The import works on every VM, whether its base is Ubuntu's image or a Debian-, Ubuntu- or Alpine-based container image ([starting from a container image](from.md)), because it unpacks the export with GNU `tar`, which Debian and Ubuntu always have and which devvm installs in every base made `--from alpine`.

When devvm makes a new VM that has an agent, and it saved an export for the same folder before, it offers that export. In a terminal it asks whether to import it into the new VM, with no as the default. Without a terminal, or with `--no-input`, it doesn't ask, and it says which export it found and that `devvm history import` in the folder puts it into the VM. devvm imports the export once the VM has started and before its setup, and if the import fails, devvm warns and makes the VM anyway, so you can run `devvm history import` later.

An export holds what a VM wrote, so devvm treats it as untrusted. Its transcripts can hold what the old VM's agents read, and a VM on a less strict tier reaches more, such as your local network on the open tier. So when the export comes from a VM on a less strict tier than the VM you import it into, going from open to internet, allowlist and none, devvm warns you before it imports. devvm knows an export's tier only from the `.folder` file beside it in `~/.local/share/devvm/exports`, so it warns about an export with no tier recorded, or any FILE elsewhere, such as in the project folder that a VM can write, as if it came from an open VM. devvm never unpacks an export on this machine. It streams the file into the VM as it is, and the VM's own `tar` unpacks it there as the VM's user, so it can only write what that user can write. Before unpacking anything, the VM lists the paths in the export, and when one of them is outside `~/.claude`, `~/.claude.json`, `~/.codex`, `~/.local/share/opencode`, `~/.local/state/opencode` and `~/.pi/agent/sessions`, starts with `/` or has a `..` part, devvm imports nothing and says so. The import keeps no owner or mode from the export, so the VM user's umask applies and no file is set-user-ID. It also takes out every symbolic link that points outside the folder it's in, which is any link to an absolute path or one with a `..` part, so an agent writing to a session file can't follow a link out of its history. An export that devvm didn't write, such as a `.tar.gz` you made yourself, can hold opencode's database with its logins, so the import takes out the database's `-wal`, `-shm` and `-journal` files and puts in a copy of the database without its logins, as the export makes it, or leaves the database out when the VM has no `sqlite3`. opencode in the new VM then reads the logins that devvm gave the VM, as it does when it first makes its database. When the VM already has an opencode database, it keeps its own, and opencode's sessions from the export aren't imported.

The import leaves out what devvm puts in the VM itself. That's the agents' logins, `~/.claude/.credentials.json`, `~/.codex/auth.json`, and opencode's `auth.json`, `account.json`, `mcp-auth.json` and `bin`. It's also `~/.claude.json`, whose copy in the old VM held that VM's own placeholders, and every agent's [mounted folders and copied files](#devvms-agent-store), which the new VM's setup links and copies from devvm's agent store. A file or folder the VM already has stays as it is, so importing into a VM that has history of its own adds the export's files without replacing any of its own.

## The VM's setup

Besides the agents' config, every launch sets the VM up from this machine's settings:

- its hostname, which is the folder's part of the VM's name;
- the timezone (`TZ`, else this machine's) and, when it's UTF-8, the locale (`LC_ALL`, else `LANG`);
- git's `user.name` and `user.email`, as git has them in the project folder;
- telemetry off where an environment variable can't turn it off, which is Go's, and Codex's analytics and metrics;
- every agent's environment variables, which turn off its self-update and its telemetry ([supported agents](#supported-agents));
- every toolchain's environment variables, such as the ones that turn off .NET's telemetry and send `npm i -g` into your home, and the folders where the toolchains' global installs go, on the `PATH` ([toolchains](toolchains.md));
- `(devvm)` at the start of the prompt.

On every network tier, the launch also installs the VM's own CA and the next one, points apt, sudo, ssh, the [docker layer](images.md#docker)'s Docker and the [nix layer](images.md#nix)'s Nix at the proxy, and adds `devvm-why`, which shows what the proxy blocked lately. It also tells the agents, in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, why a request may fail ([inside the VM](../safety/network.md#inside-the-vm)). The VM's secrets go in `~/.config/devvm/env` in the VM, which login shells read, and that file holds their placeholders.
