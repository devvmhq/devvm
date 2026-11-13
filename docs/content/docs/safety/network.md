---
title: Network tiers
explainer: Choose what each VM can reach on the network
weight: 1
---

Every VM goes through a proxy that devvm runs for it on this machine, and the VM's network tier decides what the proxy lets it reach. New VMs get the allowlist tier by default, so they reach only the hosts on their allowlist, which are devvm's built-in hosts, your secrets' hosts and the hosts you add. No VM holds the secrets that devvm saved, whatever its tier, because its proxy puts them into its requests, but code in the VM can still use each secret the VM may use, on that secret's hosts. A secret is a credential devvm keeps on this machine for VMs to use: your GitHub token, Claude Code's token, an agent's login or an API key. An open VM, the one tier that isn't confined to its proxy, reaches anything this machine can, including this machine's own services on `localhost` and your LAN.

```mermaid
sequenceDiagram
  participant P as The VM
  participant X as Its proxy,<br>on this machine
  participant H as api.github.com
  P->>X: GET /user, with<br>a placeholder
  Note over X: on the allowlist
  X->>H: GET /user, with<br>your GitHub token
  H-->>X: the response
  X-->>P: the response
  P->>X: CONNECT<br>example.com:443
  X-->>P: 403: not on<br>the allowlist
```

This page covers what you do with a VM's tier and what you see. [How it works › Network tiers](../internals/network.md) explains how devvm confines a VM to its proxy and how the proxy handles each connection.

## Network tiers

A VM is on one of four tiers, from the lowest to the highest:

| Tier | What the VM reaches | Confined to its proxy |
| --- | --- | --- |
| `open` | whatever this machine reaches, its `localhost` and the LAN included; the proxy applies no rules | no |
| `internet` | every public host, on any port, apart from your denies; never this machine's addresses or its networks' | yes |
| `allowlist` | only the hosts its [allowlists](#allowlists) allow, apart from your denies | yes |
| `none` | only its agents' hosts, the hosts of its secrets and logins, and its [links](#links-between-vms), apart from your denies | yes |

```sh
devvm --network internet   # make this folder's new VM on the internet tier
devvm network none         # give this folder's VM the none tier
devvm network              # show its tier and its allowlist
```

`--network TIER` chooses a new VM's tier, and `devvm network` with a tier changes an existing VM's ([choosing a tier](#choosing-a-tier)). A higher tier is stricter, and a VM on any tier but open is confined: it reaches nothing but its proxy, and a direct connection from it fails. A VM can also start on the internet tier while it [learns the hosts it needs](#learning-the-hosts-a-vm-needs), and then go to the allowlist tier with the ones you choose.

An open VM runs on its backend's own network, so it reaches this machine and your LAN directly, as a VM without a proxy would. It still gets only a placeholder for each secret it may use, so a request that goes around its proxy carries no secret, and every launch checks that its proxy is set in the VM and answers it ([an open VM](../internals/network.md#starting-a-vm-on-its-tier)).

- **Rules by tier:** on the internet and none tiers, devvm reads only the denies of your policy files, because the internet tier allows every public host itself and the none tier allows only what the VM's agents, secrets and links need. `devvm network` marks each such file `(denies only)`, and when you add an allow to a VM on either tier, devvm says that the allow changes nothing until the VM is on the allowlist tier. On the open tier no rule applies at all.
- **When a change applies:** a change from one confined tier to another applies at once, because the VM's proxy starts again with the new tier's rules and the VM keeps running. A change into or out of the open tier applies from the VM's next start, as [choosing a tier](#choosing-a-tier) describes.
- **What you see:** `devvm ls` and `devvm network` show a VM's tier, or `unenforced` when a confined tier isn't in force ([when it isn't enforced](#when-it-isnt-enforced)).

## Choosing a tier

A new VM gets the allowlist tier unless you choose another, and the summary devvm shows before it makes the VM names its tier. When you customize the VM in a terminal, devvm lists the four tiers, each with what it reaches, with `learn` after `internet`, and the tier the summary showed is chosen ([what a new VM gets](../using/images.md#what-a-new-vm-gets)):

```text
Network tier for devvm-some-project-71a99b1076d92c (up/down to choose, enter to confirm):
  ( ) open       reaches anything this machine can
  ( ) internet   reaches any public host
  ( ) learn      reaches any public host while it learns which it needs, until devvm network lock
> (*) allowlist  reaches only the hosts you allow (recommended)
  ( ) none       reaches only its agents' and secrets' hosts and its links
```

Up and down move the choice, and enter gives the VM the chosen tier, so pressing enter at once keeps the tier the summary showed. `learn` gives the VM the internet tier while it [learns the hosts it needs](#learning-the-hosts-a-vm-needs). `--network TIER` answers the question up front, such as `devvm --network internet` or `devvm --network learn`, and when there's no terminal to ask in, or you give `--no-input`, the VM gets the tier the summary showed. A VM keeps its tier. On an existing VM devvm ignores `--network` and warns you, naming the `devvm network` command that changes the tier.

To give every new VM another tier, save it with `devvm network -g`, or choose it in [devvm setup](../using/settings.md#devvm-setup):

```sh
devvm network -g none   # every new VM gets the none tier
devvm network -g        # show what new VMs get, and where it came from
```

- Neither takes `learn`, because each VM learns the hosts it needs on its own, so `devvm network -g` refuses it and names `devvm --network learn` and `devvm network learn`.
- Each new VM's summary then shows that tier, and customizing the VM still asks, with Enter keeping the saved tier.
- `devvm network -g` shows what new VMs get, `allowlist` until you save another tier, followed by where it came from after a `#`, and `devvm network -g --json` prints it as `policy`, with `source`.
- devvm reads the saved tier only when it creates a VM, so the VMs you already have keep their own tier, and `--network` still chooses for one new VM.
- You can also set it with `network` in [`config.toml`](../using/settings.md#configtoml), which wins over the saved tier, and then `devvm network -g` refuses to change it and names the file ([where a default comes from](../using/settings.md#where-a-default-comes-from)).

Lima confines every VM to its proxy, so every VM can be on any tier, the docker layer's included. devvm never starts a VM on a confined tier with an open network, so if a VM's backend couldn't confine it, the VM could only be open: devvm would refuse to make it on any other tier unless you give `--network open`, and it would refuse to give it any other tier with `devvm network` ([when a backend lacks something](../backends/_index.md#when-a-backend-lacks-something)).

| | |
| --- | --- |
| `devvm network` | print this folder's VM's tier as `devvm ls` shows it, `open`, `internet`, `internet (learning)`, `allowlist`, `none` or `unenforced`, and then its [allowlist](#changing-rules) |
| `devvm network open\|internet\|allowlist\|none` | change it: between confined tiers at once, and into or out of open from the VM's next start, which `devvm restart` gives you, and for a running VM, ask in a terminal whether to restart it now |
| `devvm network learn` | give it the internet tier while it [learns the hosts it needs](#learning-the-hosts-a-vm-needs), from now on |
| `devvm network lock` | choose which of those hosts it keeps, and give it the allowlist tier ([locking it](#locking-it)) |
| `devvm network -g [open\|internet\|allowlist\|none]` | show or change what new VMs get |

When a change into or out of open finds the VM running, devvm asks in a terminal whether to restart it now, which stops anything running in it:

- If you answer yes, devvm restarts the VM as `devvm restart` does, so the change applies at once.
- If you answer no, or there's no terminal, or you pass `--no-input`, the change waits until the VM starts again.
- If you give a running open VM a confined tier, the VM isn't confined yet, so `devvm ls` shows it as `unenforced`, and its next launch offers to restart it confined, as it does for a VM started outside devvm.
- If you make a VM open while it runs confined, its launches keep it confined to its proxy, which now applies an open VM's rules, until it next starts, and they warn you that the VM leaves its proxy's confinement when it next starts. In a terminal, each of those launches first asks whether to restart the VM now, with no as the default unless an agent's new folders wait too ([restarting](../using/vms.md#restarting-and-stopping)).

### When it isn't enforced

On every launch devvm checks on this machine that the VM still runs on the process devvm started confined, and that a direct connection from the VM to the internet fails. A VM that fails either check shows as `unenforced`, and devvm offers to restart it confined or stops it, as [the self-check](../internals/network.md#the-self-check) describes.

## Learning the hosts a VM needs

Finding every host a project needs one block at a time takes a while, so a VM can learn them instead:

1. Make the VM learn. For a new VM, choose `learn` in its tier list when you customize it, or give `--network learn`, such as `devvm --network learn`. For an existing VM, run `devvm network learn`, which gives it the internet tier at once, as `devvm network internet` does, and starts its learning from now.
2. Run the project's installs, builds and tests. The VM has the internet tier's access meanwhile, and its proxy logs each host it reaches, as it does on every tier.
3. Run `devvm network lock`, which lets you choose which of those hosts the VM keeps, and puts it on the allowlist tier with them ([locking it](#locking-it)).

Learning is for one VM at a time, so no default makes new VMs learn: `devvm network -g`, `devvm setup` and `config.toml` don't take `learn` ([choosing a tier](#choosing-a-tier)).

> [!WARNING]
> Learning isn't a tier of its own. While it lasts, the VM is on the internet tier, so it reaches every public host and only your denies apply, and links and groups treat it as an internet VM. That's internet access for as long as the VM learns, and that includes the dependency installs you run to teach it, when packages' install scripts run in the VM. So a compromised dependency that you install meanwhile can reach a host of its attacker's choosing, and that host then shows up among the learned hosts. That's why `devvm network lock` asks, starts with few hosts ticked, and leaves you to read the list before you confirm it. Your secrets stay on this machine as they do on every tier, and the VM still gets only placeholders.

Each launch of a learning VM says that it's learning, since when, and how many hosts it has reached beyond its allowlist, and it names `devvm network lock`. Once the VM's log holds more than 2 MiB, the launch leaves out the count, so it never reads the whole log each time. `devvm ls` and `devvm network` show its tier as `internet (learning)`, and their `--json` adds `learning_since`, the time it started learning.

`devvm network internet`, `none` or `open` end the learning and give the VM that tier. `devvm network allowlist` refuses while the VM learns, because `devvm network lock` is how a learning VM goes to the allowlist tier, and `devvm network lock --none` does it without adding a host. A VM in a [group](#links-between-vms) can't start learning or be locked, because a group's VMs are all on one tier, so run `devvm group leave` first, and for the same reason a learning VM can't join a group until you lock it.

### Locking it

`devvm network lock` reads the hosts the VM reached since it started learning from its network log, and it leaves out these:

- what the VM's allowlist tier would allow already, judged by the proxy's own rules: devvm's built-in rules, its shared tools' and layers', your own rules for every VM and for this VM, and its secrets' and logins' hosts
- what your rules deny, which no allow would reach
- blocked requests
- internal and LAN addresses, which the internet tier never reaches

In a terminal it lists the rest, one row for each host and port, with how many connections each had and the first word of the `User-Agent` that the requests sent most, in three groups:

1. The hosts that the VM's tools reached through its proxy come first.
2. The hosts that only tools without proxy settings reached, which [transparent mode](#tools-that-ignore-the-proxy) sent to the proxy, come next, under a heading of their own, because background tools' telemetry and update checks reach the proxy that way, and you may not want to keep them.
3. The risky hosts come last, under a heading of their own, so neither a long name nor a `User-Agent` can push off the row what makes them risky.

That `User-Agent` is the VM's own claim, which any program in it can set to anything, so it's a hint, not proof of which tool made the connection, and devvm shows it only when it's at most 32 letters, digits and `._/+-`. Which way a connection came is the VM's own doing, as its `User-Agent` is, so the second group's heading is a hint about the tool, not proof of what it is. A host name that doesn't fit is cut from the left, so its domain shows, and a list longer than the terminal scrolls, with a line above and below that says how many more rows there are. In a terminal too short to show a few rows, devvm refuses rather than draw the list wrong:

```text
Since 2026-11-08 09:03 the VM could reach any public host, so this list can hold a host that a compromised dependency reached. Only a host on port 443 of a package registry or vendor devvm knows starts ticked, so read the others before you tick them.
Choose the hosts devvm-some-project-71a99b1076d92c keeps. Use up and down to move, space to toggle, a to toggle them all and enter to confirm.
> [x] files.pythonhosted.org:443 2 connections, User-Agent: uv/0.9.2
  [x] static.crates.io:443       1 connection, User-Agent: cargo/1.91.0
  [ ] example.com:8443           1 connection
  [ ] uploads.github.com:443     1 connection, User-Agent: git/2.51.0
Reached only by tools without proxy settings, which the VM's names sent to its proxy:
  [ ] api.open-meteo.com:443     3 connections
Risky, each an easy way to send data out:
  [ ] 203.0.113.5:443            1 connection
  [ ] docs.google.com:443        1 connection, User-Agent: python-requests/2.32
  [ ] pastebin.com:443           1 connection, User-Agent: curl/8.5.0
```

A host starts ticked only when it's on port 443 and belongs to a package registry or vendor on a short list that devvm keeps, each name checked so that nobody but its vendor can make a host or post a page under it:

- npm's `registry.npmjs.org`, PyPI's `files.pythonhosted.org` and `crates.io`
- Go's `proxy.golang.org` and `sum.golang.org`, and NuGet's `api.nuget.org`
- the toolchains' own release hosts, such as `nodejs.org` and `static.rust-lang.org`
- Docker Hub's and GitHub's container registries
- the agents' APIs, `api.anthropic.com` and `api.openai.com`

A registry's packages are its users', but they only get there through the registry's own upload, with a token. Nothing else makes a host start ticked: not a domain where anyone can get a subdomain or a page, such as `github.io`, `googleapis.com`, `amazonaws.com` or `pypi.org`, which takes uploads, not a host beside one your rules or your secrets name, and not one beside a host an earlier lock added. So `files.pythonhosted.org:443` starts ticked, while `files.pythonhosted.org:8080`, `docs.google.com` and `evil.github.io` don't. Every other host starts unticked, and so does every host when the list has more than 30, so read the list, or `devvm network lock --json`, before you tick them.

A host that's an easy way to send data out is risky, and it always starts unticked. devvm's own rules leave all of them out too, so tick one only when the project needs it. These are risky:

- an address rather than a host name
- a name with a punycode (`xn--`) label, which can look like another name
- a paste or file drop site such as pastebin.com, paste.ubuntu.com or gist.github.com, or a log that takes entries from anyone, such as rekor.sigstore.dev
- file storage or file sharing such as transfer.sh, Dropbox, Google Docs, SharePoint or an S3 bucket
- a mail server such as Gmail's
- a tunnel such as ngrok or trycloudflare.com
- a service that records what it's sent, such as webhook.site or an OAST domain
- free hosting such as vercel.app, workers.dev or cloudfront.net
- a chat service's endpoint for bots, such as Discord's, Slack's hooks or Telegram's API

In the list, these keys choose the hosts:

- Space ticks or unticks the row under the cursor.
- `a` ticks every row, the risky ones and those on other ports too, and again unticks them all.
- Enter adds the ticked hosts to the VM's own policy file. Each is an exact host and port, such as `registry.example.com:443` or `example.com:8443`, with the port written even for 443, so it doesn't allow port 80 too, and never a `*.` wildcard. Then devvm gives the VM the allowlist tier at once, as it does between confined tiers, so a running VM keeps running, and its learning ends.

When the VM's tier changed while the list was open, such as with `devvm network internet` in another terminal, `devvm network lock` changes nothing and says so. When the terminal doesn't let devvm read single keys, or goes away while the list is open, or you press Ctrl+C, `devvm network lock` changes nothing, rather than lock with the hosts as they were ticked then. Later, `devvm network allow`, `deny` and `rm` change those rules as they change any other ([changing rules](#changing-rules)).

| | |
| --- | --- |
| `devvm network lock` | list the hosts the VM learned, so you choose which it keeps, then give it the allowlist tier |
| `--all` | keep every host it learned, the unticked ones too, without asking |
| `--none` | keep none of them, without asking |
| `--json` | list the hosts it learned, and change nothing |

Without a terminal, or with `--no-input`, `devvm network lock` refuses and names `--all` and `--none`. When the VM reached no host beyond its allowlist, it locks at once without asking. A VM that isn't learning has nothing to lock, so `devvm network lock` refuses and names `devvm network learn`. While the VM links to a target on a lower tier than allowlist, `devvm network lock` refuses as `devvm network allowlist` would ([links between VMs](#links-between-vms)).

The proxy keeps each VM's log to its last 10 to 20 MiB ([logs](#logs)), so a VM that learns for a long time with a lot of traffic can lose the first hosts it reached. When the log no longer goes back to the time the VM started learning, `devvm network lock` warns you, and its `--json` sets `partial`. A host that's missing then is blocked once the VM is locked, and `devvm network log --blocked` names the `devvm network allow` command that allows it.

#### devvm network lock --json

`devvm network lock --json` prints what the VM learned as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes, and changes nothing, so devvm refuses it with `--all` or `--none`. The hosts come in the list's order: the tickable ones first, then the others, then those that only tools without proxy settings reached, then the risky ones.

| Field | What it holds |
| --- | --- |
| `learning_since` | when the VM started learning |
| `partial` | `true` when the log no longer goes back that far |
| `hosts` | each host the VM learned, with the fields below |
| `rule` | the rule that `devvm network lock` adds for it |
| `host`, `port` | its host and port |
| `count` | the number of its connections |
| `last` | the last time the VM reached it |
| `agent` | the `User-Agent`'s first word that reached it most, when devvm shows one |
| `tickable` | `true` for a host on port 443 of a registry or vendor on devvm's list, which starts ticked unless it's risky |
| `risk` | for a risky host, `address`, `lookalike`, `paste`, `storage`, `mail`, `tunnel`, `webhook`, `hosting` or `chat` |
| `via` | `dns` for a host that only tools without proxy settings reached |

```json
{
  "version": 1,
  "learning_since": "2026-11-07T10:00:00Z",
  "hosts": [
    {
      "rule": "files.pythonhosted.org:443",
      "host": "files.pythonhosted.org",
      "port": 443,
      "count": 48,
      "last": "2026-11-07T10:19:00Z",
      "agent": "uv/0.9.2",
      "tickable": true
    },
    {
      "rule": "registry.example.com:443",
      "host": "registry.example.com",
      "port": 443,
      "count": 12,
      "last": "2026-11-07T10:20:00Z",
      "agent": "npm/11.6.2"
    },
    {
      "rule": "pastebin.com:443",
      "host": "pastebin.com",
      "port": 443,
      "count": 3,
      "last": "2026-11-07T10:21:00Z",
      "agent": "curl/8.5.0",
      "risk": "paste"
    }
  ]
}
```

## Allowlists

The VM's proxy reads devvm's built-in policies first and then your policy files, in order, as one list:

- devvm's own rules for every VM, which allow OS packages, mise's version lists, the Sigstore and GitHub trust roots that mise checks a download's attestations against, Node's downloads for a project's own node, and GitHub;
- devvm's rules for each shared tool in the VM's image (`node`, `claude` and the others), which allow its registries, downloads and APIs;
- devvm's rules for each [built-in layer](../using/images.md#built-in-layers) in the VM's image, so the `docker` layer allows Docker's packages and the container registries Docker Hub and ghcr.io, and the `nix` layer allows Nix's binary cache, channels and flake registry;
- `~/.config/devvm/layers/<name>.policy`, for each of your own layers in the VM's image;
- `~/.config/devvm/policy`, your own rules for every VM, which `devvm network allow`, `deny` and `rm` change with `-g`;
- `~/.local/share/devvm/vms/<hash>/policy`, the rules for that VM only, in the VM's own folder, which is named by the hash that the VM's name ends with, which [`devvm network allow`, `deny` and `rm`](#changing-rules) change;
- the hosts of the VM's secrets and logins, and the host its agents log in at, such as Codex's and opencode's `auth.openai.com` and Claude Code's `platform.claude.com` and `console.anthropic.com`, on port 443, which devvm adds.

The first three are built in: they come with devvm, inside its binary, so you can't edit them, and an upgrade of devvm is what changes them. To change what a VM reaches, add your own rules, and to keep a VM from a host that devvm's rules allow, add a deny, which beats any allow. devvm leaves storage and paste sites out of its defaults on purpose, because they're easy ways to send data out. Rules never come from the project folder, because the VM can write it. The proxy reads each of your files again when it changes, so an edit applies at once, from the next connection. [How it works](../internals/network.md#policy-files) lists where each policy comes from.

### Rules

A policy file holds one rule per line. `#` starts a comment, and matching a host ignores case.

| Rule | Matches |
| --- | --- |
| `example.com` | that host only |
| `*.example.com` | any subdomain, at any depth, not `example.com` itself |
| `**` | any host, and any address, though an internal address still needs its own rule |
| `10.0.0.5`, `10.0.0.0/8`, `fd00::/8` | addresses, IPv4 or IPv6 (also what lets a connection past the internal-address refusal) |
| `RULE:22`, `[fd00::1]:22`, `10.0.0.0/8:8080` | only that port; without a port, 443 and 80 |
| `RULE:*`, `**:*` | any port |
| `!RULE` | deny: a deny beats any allow |
| `RULE METHODS PATH`, such as `api.github.com GET,HEAD /repos/` | only those requests to a host name, by their methods, their path or both ([rules by method and path](#rules-by-method-and-path)) |

A rule is written this way:

- A rule is plain ASCII, and a host's part has no spaces in it, while a rule by method and path has its methods and its path after the host, each after a space.
- A port is a number from 1 to 65535, or `*` for any port.
- A host's one trailing dot is dropped, so `example.com.` matches `example.com`.
- Brackets hold only an IPv6 address, and an address can't follow `*.`.
- A line that holds anything else, such as `example.com:x`, `a*.example.com` or `! example.com` with a space after the `!`, isn't a rule. A line that isn't a rule and doesn't start with `!` is skipped, and the file's other lines still apply. The proxy names each such line in its own log.

> [!WARNING]
> When a line that isn't a rule starts with `!`, it reads as a deny, but the proxy can't tell what it would deny. So while the line is there, the proxy of every VM that reads the file denies every new connection apart from the VM's [links](#links-between-vms), whatever the other rules allow. Connections that are already open stay open. Each refusal names the file and the line, and it suggests no `devvm network allow` command, because no rule would help.

devvm warns about each line that isn't a rule, with the file and the line's number, and the warning for a broken deny says that the proxies deny everything until it's fixed. It warns when you open a confined VM whose proxy reads the file, when `devvm network allow`, `deny` or `rm` changes the file, and in `devvm doctor`. `devvm network` also marks the line. Fix the line or remove it, and the change applies from the next connection.

The proxy never dials a denied address, even for an allowed name that resolves to it or to its NAT64 or 6to4 form. A missing file has no rules. A file that changed but can't be read keeps its last rules, so a deny never lapses.

### Quiet hosts

Some tools send telemetry to a host of their own, and devvm's built-in policies leave such hosts out. Cursor's CLI has no setting that turns its metrics off, so it keeps sending them to `api3.cursor.sh` and getting blocked, many times a session. devvm's built-in policies mark a few such hosts quiet, with a `quiet HOST` line, for one exact host name. A quiet host is blocked as any host that no rule allows, and `devvm network log` still shows each block, ending `, quiet`. The VM gets no note before its prompt for it, though, and `devvm-why` doesn't list it, the `403` that the tool gets suggests no command, `devvm network log --blocked` shows `- (quiet)` instead of one, and `devvm network lock` never offers it.

A block is quiet only when no rule of yours covers the host and nothing else refused it, such as its address. To allow a quiet host anyway, add it as any other host, such as `devvm network allow api3.cursor.sh`, and your rule wins. Only devvm's own policies can mark a host quiet. In one of your files, a `quiet` line isn't a rule, so the proxy skips it, and devvm warns about it as it warns about any other line that isn't a rule, saying that only devvm's policies can mark a host quiet. These are the quiet hosts:

- `api3.cursor.sh`, the host Cursor documents for its editor's Tab requests, which Cursor's CLI uses for its metrics (in a `devvm cursor` window, Tab's requests come from the Cursor app on this machine, not the VM);
- `telemetry.go.dev`, Go's telemetry uploads, which every VM turns off.

### Changing rules

`devvm network` shows this folder's VM's allowlist after its tier. It prints each of the VM's policies that holds more than comments, and then its rules without their comments. A built-in policy has its name after `# devvm:`, because it has no file you could edit, and one of your files has its path after `#`, followed by `(denies only)` on the internet and none tiers, where it shows only the file's denies. An open VM has no policies, so `devvm network` prints its tier alone. A built-in policy's quiet line is marked `# blocked, without a notice in the VM`, and a line that isn't a rule is marked `# not a rule, skipped`, with its line number:

```text
allowlist
# devvm: layers/base.policy
  ports.ubuntu.com
  github.com
# /Users/you/.local/share/devvm/vms/71a99b1076d92c/policy
  example.com
  example.com:x  # not a rule, skipped (line 2)
```

| | |
| --- | --- |
| `devvm network allow RULE...` | add the rules to the VM's own file |
| `devvm network deny RULE...` | add them as denies, adding `!` when it's missing |
| `devvm network rm RULE...` | remove the lines with these rules, a deny with its `!`, and warn about one that isn't there |
| `-g`, `--global` | with `allow`, `deny` or `rm`, change `~/.config/devvm/policy`, for every VM, instead, which needs no VM |
| `--method METHODS`, `--path PATH` | with `allow`, `deny` or `rm`, narrow each rule to those requests ([rules by method and path](#rules-by-method-and-path)) |

A rule is what [Rules](#rules) lists, without spaces or a comment, and `--method` and `--path` add its methods and its path. devvm refuses anything else before it adds any rule, and it also refuses `allow !RULE`. A rule that's already in the file, in any case, isn't added again, and a rule by method and path is the same rule however its methods are written, while its path's case is its own. Each change applies at once, from the next connection. For an open VM, devvm notes that no rule applies to it, and for an allow on the internet or none tier, that only denies apply there. When `devvm network log --blocked` or a blocked request in the VM suggests a `devvm network allow` command, its rule is one that `devvm network allow` takes.

If a VM was deleted outside devvm, devvm deletes what it kept for that VM when it makes a new VM for the same project folder, so the new VM never gets the old one's rules.

### Rules by method and path

A rule for a host name can also name the requests it covers, by their methods, their path or both. So a VM can read from an API without changing anything there, or reach a host without pushing to it. `devvm network allow`, `deny` and `rm` take `--method` and `--path` for this, and the file gets the rule as one line, such as `api.github.com GET,HEAD /repos/`:

| | |
| --- | --- |
| `devvm network allow api.example.com --method GET,HEAD` | let the VM only read from `api.example.com`, a host that no other rule allows |
| `devvm network allow api.example.com --path /v1/` | let it reach only the paths below `/v1/` there |
| `devvm network deny api.github.com --method POST,PUT,PATCH,DELETE` | keep it from changing anything through GitHub's API, which devvm's rules allow |
| `devvm network deny github.com --path '/*/*/git-receive-pack'` | keep it from pushing to GitHub over HTTPS |
| `devvm network deny -g pastebin.com --method POST` | keep every VM from posting there |

Allows add up and a deny beats any allow, as for every rule:

- An allow by method and path lets through only the requests it names, and the host's other requests stay blocked unless another rule allows the whole host.
- So to narrow a host that a rule already allows whole, such as `api.github.com`, which devvm's own rules allow, add a deny of what the VM mustn't do. An allow by method and path for such a host narrows nothing, and devvm warns you so when you add one, naming the VMs where another rule allows the host whole, with `-g` each VM's own rules counted, and the deny to add instead. Where a whole allow names port 443 alone, such as `api.example.com:443`, an allow by method and path still narrows the host's plain HTTP on port 80, and the warning says that.
- A deny by method and path refuses only the requests it names, and the host's other requests go on.
- You can narrow any host this way, devvm's built-in ones, a secret's and an agent's included, for one VM or with `-g` for every VM.
- On the internet and none tiers your denies apply, by method and path too, while your allows apply only on the allowlist tier, as for every rule.

The methods are some of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, `TRACE` and `QUERY`, in any case, joined by commas, and devvm writes them in capitals in that order:

- `GET` doesn't include `HEAD`, so name both for reading.
- A deny refuses only the methods it names, so to keep a host read-only, name `POST`, `PUT`, `PATCH` and `DELETE`.
- A GraphQL API, such as GitHub's at `api.github.com/graphql`, takes its reads as `POST` too, so such a deny refuses them as well, and the GitHub CLI, which reads through it, fails there.
- Some servers take a request's method from a header or a parameter instead of the request itself, so the proxy judges a request by every method it names, and while a rule by method covers a host, it refuses a request whose method it can't read alike for every server ([requests by method and path](../internals/network.md#requests-by-method-and-path)).

A path is a pattern:

| Pattern | Example | Matches |
| --- | --- | --- |
| `*` | `/repos/*/*/issues`, `/*/*.git/info/refs` | any part of one segment |
| `**`, as a whole segment | `/repos/**` | one segment or more |
| a path that ends with `/` | `/repos/` | everything below it, as `/**` there would, so `/repos/a` and `/repos/a/b` but not `/repos` itself |
| a path without a `/` at its end | `/user` | also the request's path with one, so `/user/` |
| any path | `/search` | whatever the query, so `/search?q=x`, because the query never counts |

A path starts with `/`, and it has no `.` or `..` part, no doubled slash, and no space, `%`, `\`, `;`, `?` or `#`. Servers don't all read a request's path alike, so an allow matches only a path in a plain form that every server reads the same way, and a deny matches broadly. So a deny of `/gists/` refuses `/gists`, `/GISTS/1`, `/gists/`, `/gists.json`, `/%67ists/1`, `/x/../gists/1` and `/gists;x/1` alike, and a deny of `/admin` refuses `/admin.json` and `/admin.` but not `/administrator`. While a rule with a path covers a host, a request there whose path has an encoded character, such as a file name with a space, is refused unless a rule without a path allows it ([requests by method and path](../internals/network.md#requests-by-method-and-path)).

To see each request, the proxy decrypts the host's HTTPS on port 443 with [the VM's own CA](#secrets), as it does for a secret's hosts. That has these effects:

- The proxy checks every request on the connection, each one on a kept-alive connection and on HTTP/2 alike, with the rules as they are then, so a change to them applies to a decrypted tunnel from its next request. A tunnel that was open to the host before a rule narrowed it isn't decrypted, so it carries on unchecked until it closes.
- A tunnel to such a host on any other port is refused, because the proxy can't see the requests in it, while plain HTTP on any port gets the same check for each request.
- A tool in the VM that doesn't trust the VM's CA fails there, and `devvm network log` gives the reason `TLS with the VM failed`, which is why devvm reminds you when you add such a rule that the host is now decrypted. A host that a whole allow covers, with no deny by method and path, stays a tunnel.
- A rule by method and path needs a host name or `*.name`, so devvm refuses `--method` and `--path` with an address or a CIDR, which have no name to decrypt by, and with `**`, which would decrypt every host.
- A WebSocket is checked by the request that opens it, and what it carries after that isn't. A switch to any other protocol, such as `Upgrade: h2c`, and a `CONNECT` inside a decrypted tunnel are refused, because the proxy couldn't check the requests that would follow.

A request that the rules block gets `403` from the proxy, with the reason, and never reaches the host. `devvm network log` shows it with its method and its path, and `devvm-why` lists it. For a request that no allow covers, the proxy suggests the `devvm network allow` command for the whole host, and for one that a deny refuses it suggests nothing, because no allow lifts a deny. A request that a rule by method and path let through shows that rule in `devvm network log`:

```text
$ devvm network allow api.example.com --method GET --path /v1/
==> devvm added api.example.com GET /v1/ to devvm-some-project-71a99b1076d92c's rules.
The proxy now decrypts api.example.com's HTTPS on port 443 to check each request, so a tool that doesn't trust the VM's CA fails there, and a new tunnel to it on another port is refused. Tunnels to it that are open already carry on unchecked until they close.
https://devvm.dev/docs/safety/network/#rules-by-method-and-path
```

In the VM, a `GET` below `/v1/` reaches the host, while a `POST` there gets the proxy's answer:

```text
$ curl -X POST https://api.example.com/v1/charges
devvm: api.example.com:443 is blocked (no rule allows this method and path). Allow it on the host with: devvm network allow api.example.com
```

And on this machine, `devvm network log` shows both:

```text
2026-11-08T10:00:01Z  allowed  api.example.com:443  rule api.example.com GET /v1/, decrypted
2026-11-08T10:00:01Z  allowed  api.example.com:443  GET /v1/charges, rule api.example.com GET /v1/
2026-11-08T10:00:02Z  BLOCKED  api.example.com:443  POST /v1/charges: no rule allows this method and path
```

Learning stays by host: `devvm network lock` adds whole hosts, and you can narrow them afterwards with these flags ([learning the hosts a VM needs](#learning-the-hosts-a-vm-needs)). A line with a method that devvm doesn't know, such as `!api.github.com PSOT`, isn't a rule, so as a deny it makes the proxy deny everything until you fix it ([rules](#rules)). The shared Nix cache's own downloads from `cache.nixos.org` meet these rules too, as `GET`s of the paths the VM asked the cache for ([Nix](#nix)).

A VM's proxy is the devvm binary that started it, which is the one from before an upgrade until the VM's next launch. A proxy from before rules by method and path reads such a rule as a line that isn't a rule: it skips an allow by method and path, and a deny by method and path makes it deny the VM everything, until it starts again. Likewise, once one of a VM's secrets goes in by method or by a path pattern, such a proxy puts none of the VM's secrets in. So when you add or remove such a rule, or change such a secret, while a VM's proxy runs from another devvm binary, or from one devvm can't tell, devvm warns you and names the VM, and running `devvm` in the VM's folder starts its proxy again.

### devvm network --json

`devvm network --json` prints the VM's tier and its allowlist as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes. With `--json` the command only shows, so devvm refuses a tier with it.

| Field | What it holds |
| --- | --- |
| `policy` | `open`, `internet`, `allowlist`, `none` or `unenforced` |
| `learning_since` | for a learning VM, the time it started [learning](#learning-the-hosts-a-vm-needs) |
| `allowlist` | each policy that holds more than comments, in the order the proxy reads them, with the fields below |
| `builtin`, `file` | a built-in policy's name, or the path of one of your files |
| `rules` | the policy's rules: all of a file's rules, or on the internet and none tiers, for a file whose denies alone apply, its denies, as `devvm network` lists them under `(denies only)` |
| `denies_only` | `true` for a file whose denies alone apply |
| `not_rules` | each line that isn't a rule, with its `line` number and its `text` |
| `quiet` | a built-in policy's [quiet hosts](#quiet-hosts), there only when it has some |
| `transparent` | the VM's [transparent mode](#tools-that-ignore-the-proxy), with `on` and `from`, as `devvm network transparent --json` has it |

With `-g`, the object has only `policy`, which is the tier new VMs get, and `source`, which says where it came from: `config.toml`, `settings.json` for what `devvm network -g` or `devvm setup` saved, or `built-in` ([where a default comes from](../using/settings.md#where-a-default-comes-from)).

```json
{
  "version": 1,
  "policy": "allowlist",
  "source": "config.toml"
}
```

```json
{
  "version": 1,
  "policy": "allowlist",
  "allowlist": [
    {
      "builtin": "layers/base.policy",
      "rules": [
        "ports.ubuntu.com",
        "github.com"
      ],
      "not_rules": []
    },
    {
      "file": "/Users/you/.local/share/devvm/vms/71a99b1076d92c/policy",
      "rules": [
        "example.com"
      ],
      "not_rules": [
        {
          "line": 2,
          "text": "example.com:x"
        }
      ]
    }
  ],
  "transparent": {
    "on": true,
    "from": "default"
  }
}
```

On the none tier, a VM with Claude Code gets Claude Code's built-in policy whole, and its own file shows only its denies:

```json
{
  "version": 1,
  "policy": "none",
  "allowlist": [
    {
      "builtin": "shared-tools/claude.policy",
      "rules": [
        "api.anthropic.com",
        "claude.ai"
      ],
      "not_rules": []
    },
    {
      "file": "/Users/you/.local/share/devvm/vms/71a99b1076d92c/policy",
      "denies_only": true,
      "rules": [
        "!tracker.example.com"
      ],
      "not_rules": []
    }
  ]
}
```

## Inside the VM

Every login in the VM has the proxy settings in its environment (`HTTP_PROXY`, `HTTPS_PROXY` and their lowercase forms, `NO_PROXY` for the VM itself, and `NODE_USE_ENV_PROXY`), and the [CA variables](../using/settings.md#a-companys-ca) that every VM has point the tools that don't read the system's store at the VM's own CAs. devvm also points apt, sudo, ssh and Docker at the proxy ([how](../internals/network.md#the-vms-setup)), on every tier.

On a confined tier, a direct connection to an address fails at once, even as root, and no DNS query leaves the VM. Names resolve in the proxy, and inside the VM every name resolves to one stand-in address, which leads to the proxy too, so a tool that skips the proxy settings still reaches the hosts the rules allow over HTTPS and HTTP ([tools that ignore the proxy](#tools-that-ignore-the-proxy)). Such a tool's connection to another port of a name is refused at once, and nothing reaches the log for it ([what to do](../using/troubleshooting.md#a-tool-that-skips-the-proxy-fails)).

A blocked request gets `403`, and the response names the host, the reason and the rule that would allow it, but for a [quiet host](#quiet-hosts). The proxy also answers at `http://devvm.proxy/`:

- `/blocked` lists the last 50 blocks.
- `/blocked/new?after=N` gives the number of blocks so far, then those after the Nth, with each rule once, so a shell prompt can show what a command hit.
- `/ca.pem` is the VM's own CA certificate and the next one's, never their keys.
- `/nix/` is the shared Nix cache, in a VM with the `nix` layer ([below](#nix)).
- `/transparent/ports` lists the ports that [transparent mode](#tools-that-ignore-the-proxy) sends to the proxy, one a line.

A request for `http://devvm.proxy/` that comes through transparent mode, without the proxy settings, reaches `/blocked` alone and gets `403` for the rest, because that way asks the tool for no password, so a container without the proxy settings can't fetch the VM's CA or the Nix cache.

```sh
devvm-why   # in the VM: what the proxy blocked lately
```

In the VM, `devvm-why` prints what the proxy blocked lately, and a note before each prompt tells you when something new was blocked. These help too:

- When the proxy blocked nothing lately, `devvm-why` says so, and it adds that a connection refused, or a DNS error, for a host the tier allows means the tool skipped the proxy on a port or in a way devvm can't send to it, so you turn on the tool's own proxy setting or give it the address in `$HTTPS_PROXY` ([a tool that skips the proxy fails](../using/troubleshooting.md#a-tool-that-skips-the-proxy-fails)).
- With transparent mode on, `devvm-why` then says which ports it sends to the proxy, lists the connections to other ports that the VM refused lately, by port and time, and lists the names that tools looked up lately.
- devvm also tells Claude Code and Codex, in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, why a request may fail and to run `devvm-why`, that a tool that ignores the proxy settings still reaches the proxy over HTTPS, HTTP and the ports the rules name, and that a connection refused on another port while `devvm-why` shows nothing blocked means the tool skipped the proxy.

### Nix

In a VM with the [`nix` layer](../using/images.md#nix), Nix's daemon makes most of Nix's downloads, and devvm also points it at the proxy, so its downloads go through the same allowlists. The layer's allowlist has `cache.nixos.org` for Nix's binary cache, and `channels.nixos.org` and `releases.nixos.org` for its channels and flake registry, and GitHub, where most flakes' inputs come from, is in devvm's rules for every VM.

Nix asks the proxy's shared cache, `http://devvm.proxy/nix`, before cache.nixos.org, so every VM with the layer shares the packages that any of them downloaded ([shared downloads](../using/toolchains.md#nix-projects), [the shared Nix cache](../internals/network.md#the-shared-nix-cache)):

- The proxy fetches what the cache doesn't have from cache.nixos.org alone, and only while the VM's tier and rules let it reach `cache.nixos.org`. It keeps a package only when cache.nixos.org's signature on it checks, and the VM's Nix checks that signature again.
- A VM can only make the proxy fetch from cache.nixos.org: the proxy refuses anything a VM sends to store, and every request that isn't for one of Nix's cache files.
- Each request shows in `devvm network log` as `cache`, with whether it was a `hit`, `fetched`, or why it wasn't found. What one VM can learn of another through the cache is under [Attack vectors](attack-vectors.md#network).
- An open VM has its proxy too, so it shares the cache as well, and its proxy fetches from cache.nixos.org for it.

When a package isn't in the binary cache, Nix builds it from source, and the build downloads its sources from their own hosts, such as `ftpmirror.gnu.org`. The proxy blocks those hosts until you allow them, so the build fails, and the error, `devvm-why` and `devvm network log --blocked` name the host and the `devvm network allow` command that allows it.

## Behind a company proxy

When this machine reaches the internet only through a company proxy, the VM's proxy sends the requests it allows through that proxy. devvm reads `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`, or their lowercase forms, from the shell you run it in. When that shell sets neither `HTTPS_PROXY` nor `HTTP_PROXY`, devvm reads the `[proxy]` table of [`config.toml`](../using/settings.md#configtoml) instead, so the proxy works from every shell, including an editor's terminal that didn't load your profile. The shell's settings win whole: devvm never mixes them with `config.toml`'s, so a `NO_PROXY` alone in the shell still leaves `config.toml`'s proxy in effect, with its own `no-proxy`.

```toml
[proxy]
https = "http://alice@proxy.example.com:8080"
http = "http://alice@proxy.example.com:8080"
no-proxy = "localhost,.example.com"
```

devvm passes the settings to the VM's proxy when the proxy starts, and when you run devvm again with different settings, devvm starts the VM's proxy again with the new ones. So after you change them, run devvm again in the folder or `devvm restart`. When a VM's proxy went through a company proxy the last time it started and devvm finds none this time, devvm warns you and names `config.toml`, because a VM's proxy that a shell without the settings starts sends the VM's requests directly.

The VM's proxy uses the company proxy this way ([through a company proxy](../internals/network.md#through-a-company-proxy)):

- It still decides each request by the host name the VM asked for, against the same allowlists, and only then passes it on. It sends tunnels, which carry HTTPS and the requests it puts your secrets into, through `HTTPS_PROXY`'s proxy, and plain HTTP requests through `HTTP_PROXY`'s.
- It reaches the hosts that `NO_PROXY` names directly, and also `localhost` and loopback addresses. In `NO_PROXY`, `example.com` matches that host and its subdomains, `.example.com` matches only its subdomains, an address or a CIDR matches addresses, any entry can end in a port, and `*` matches every host.
- `devvm network log` adds `through host:port` to each request that went through the company proxy.

The company proxy's address and password follow these rules:

- The company proxy needs an `http://` address, such as `http://user:password@proxy.example.com:8080` in `HTTPS_PROXY`, and a proxy without a port is on port 80. devvm doesn't start a VM's proxy while `HTTPS_PROXY`, `HTTP_PROXY` or a `[proxy]` key names an `https://` or `socks5://` proxy, and it tells you which setting to change.
- The company proxy's password stays on this machine. The VM never gets it, and devvm writes it into no log, no image and no process's arguments.
- `config.toml` never holds the password, because it's a file you might copy to other machines or keep with your dotfiles, and devvm refuses a `[proxy]` URL that has one. Put only the user in the URL, as `http://alice@proxy.example.com:8080`, and save the password with `devvm secret set proxy`, which keeps it as it keeps the other [secrets](#saving-and-binding-secrets). devvm's proxies look it up themselves when they start, so no file of devvm's holds it in the clear.
- devvm doesn't make a new VM, start a VM's proxy, or build an image or the shared tools while `config.toml`'s proxy names a user and no password is saved, and it checks before anything starts.
- A VM's proxy reads the password only when it starts, so after you change it, run `devvm restart` in each VM's folder. No VM ever gets the `proxy` secret, and `devvm secret use proxy` refuses.

```sh
devvm secret set proxy   # save the company proxy's password, which devvm asks for
devvm restart            # in each VM's folder, once the password changed
```

> [!NOTE]
> The company proxy sees the name and port of every host the VM reaches, and if it inspects TLS, the [CA you add for it](../using/settings.md#a-companys-ca) also lets it read the traffic. The company proxy also looks up each name itself, so the VM's proxy can't check every address a name leads to. It still refuses an address that's this machine's own or internal, unless a rule names it, and it refuses a name when this machine's lookup gives an internal or denied address for it. A name that this machine can't look up goes to the company proxy unchecked only when an allowlist names it, as `example.com` or `*.example.com`. When only `**` allows a name that this machine can't look up, the VM's proxy refuses it, so `**` never lets a VM send the company proxy a name of its own making. What a VM can still reach through the company proxy, such as hosts on your company's network, is under [Attack vectors](attack-vectors.md#network).

### Open VMs and builds

These go through the company proxy too:

- **Open VMs.** An open VM goes through its proxy as every VM does, and its proxy sends its requests through the company proxy, from the shell's settings or `config.toml`'s, so the company proxy's password stays on this machine for it too. The VM never gets the shell's proxy settings, and Lima gets none to pass on to it. What an open VM reaches directly, past its proxy, such as the hosts its `NO_PROXY` names, doesn't go through the company proxy. Lima prints each proxy setting it gives a VM, password and all, as it starts the VM. devvm keeps the password out of its [run log](../using/troubleshooting.md#a-command-fails), and once the VM has started, it replaces the password with as many `*` in the log Lima keeps for the VM.
- **Builds.** Builds never get your company proxy's settings, and images never keep them. A build goes through the shell's company proxy, or else `config.toml`'s, as a VM's proxy does. When devvm builds an image, builds the shared tools or converts a `--from` container image on Lima behind a company proxy, the build goes through a proxy of devvm's on this machine, made for that build alone, which goes through your company proxy. The build's commands get only that proxy's address, with a password devvm makes for the build, while they run, and devvm stops the proxy when the build ends.
- **What a build runs.** So the code a build runs, which includes a container image's own, never sees your company proxy's address or password. That proxy also runs on this machine, so a company proxy on `localhost`, such as one a VPN client runs, works for builds too. A build's proxy allows any host, as `**` does. It sends a name that this machine can't look up on to the company proxy, so builds work where only the company proxy looks up outside names.
- **devvm's own downloads on this machine.** They are devvm's copy of mise and the versions it looks up for the shared tools, the list of Ubuntu's cloud images, and a container registry's answers for `--from`. With the shell's settings, each of them uses the shell's. With `config.toml`'s, devvm's own requests send the password only to the company proxy. When a VM's proxy refreshes a Codex or opencode login on this machine, the refresh goes through the same company proxy as the VM's requests, with the password kept on this machine.
- **The mise devvm runs on this machine.** It gets the proxy's URL with the password in it, in its environment, because mise takes a proxy's password nowhere else. devvm percent-encodes the password in that URL and mise decodes it before it sends it, so a password with characters such as `@`, `:`, `/`, `#`, `%` or a space works too. devvm never puts that URL in mise's arguments or in a file.

## Tools that ignore the proxy

On every tier but open, devvm sends a tool that ignores the proxy settings to the VM's proxy anyway, by the names it looks up. This is transparent mode, and it's on unless you turn it off. In the VM, every name resolves to one stand-in address, `198.18.0.1`, and a connection to that address on port 443, port 80 or a port that one of your allow rules names goes to the VM's proxy.

```sh
devvm network transparent       # show whether this folder's VM has transparent mode
devvm network transparent off   # turn it off for the VM, from its next launch
```

The proxy reads the host from the connection itself, from the TLS server name (SNI) on HTTPS and from each request's `Host` on plain HTTP, and it judges that host as it judges a request through the proxy settings, against the same allowlist, with the same secrets and the same log. A blocked HTTPS host gets the proxy's `403`, over a certificate for that host from the VM's own CA, so the tool shows the same message, with the `devvm network allow` command, that a request through the proxy settings would show. `devvm network log` and `devvm-why` mark each of these connections `without proxy settings`, and so does the note before the prompt. A tool such as workerd, which `wrangler dev` runs for Cloudflare's adapters, makes its own lookups and connections, and transparent mode is what lets it reach your allowed hosts from a confined VM.

Transparent mode covers HTTPS and HTTP to a host by name on ports 443 and 80, and on up to 32 more ports that your allow rules name for hosts by name, such as `api.example.com:8443`, the lowest first. It never covers port 22 or 53, even when a rule names one, because SSH names no host in its first bytes and port 53 is the VM's own names'. It leaves these to the tool's own proxy settings:

- **Other ports.** A connection to another port of a name is refused at once, and so is UDP to any port, QUIC included, so a client that tries HTTP/3 falls back to TCP. A client that uses HTTP/3 alone doesn't connect.
- **Protocols that name no host in their first bytes,** such as Postgres, MySQL, Redis, SSH and raw TCP, and those whose server speaks first, such as SMTP. The proxy refuses such a connection and logs why, even on a port a rule names.
- **Addresses.** A URL with an address in it, such as `https://1.2.3.4/`, is a direct connection, which fails, and so does a tool that looks a name up itself and then gives the proxy the stand-in address, which the proxy refuses with a reason that says so.
- **IPv6.** Names have no IPv6 address in the VM, so a client that uses IPv6 alone finds no address.
- **Names without a dot, and your network's names.** A name without a dot, such as `db`, resolves only when `/etc/hosts` has it, as the name of a [link between VMs](../using/links.md) does. The VM's resolver adds the search domain of this machine's network to such a name, and transparent mode can't tell `db.corp.example.com` made that way from one a tool names whole, so every name under that search domain gets no answer either, such as `printer.lan` or `git.corp.example.com` when your network's search domain is `corp.example.com`. A tool that reaches such a host needs its proxy settings.
- **A DNS server of yours on the VM's `127.0.0.1:53`.** Transparent mode sends the VM's lookups there to its own names, so such a server, and a [forward](../using/ports-size.md#ports) of port 53 to it, gets none. Put the server on another address or port, or turn transparent mode off for the VM.
- **Clients that don't trust the VM's CA,** such as most Docker containers, and those that pin a certificate. For a blocked HTTPS host they show a certificate error instead of the `403`, and the block is logged all the same.

With the [docker layer](../using/images.md#docker), Docker's containers get the same treatment as the VM's own programs, on Docker's default bridge and on the networks that `docker network create` and Compose make. A container without the proxy settings, whether its environment emptied them or a program made it through Docker's API without them, reaches what the VM's rules allow, as every program in the VM does. To give a container no network, run it with `--network none`, and a container on an `--internal` network reaches nothing either.

`devvm network transparent` shows a VM's transparent mode, `on` or `off`, and where that comes from: the VM's own setting, `config.toml`'s `transparent-proxy`, or devvm's default, which is on. `devvm network transparent off` and `devvm network transparent on` give the folder's VM its own setting, and `transparent-proxy = false` in [`config.toml`](../using/settings.md#transparent-mode) turns it off for every VM that has none of its own. A change applies at the VM's next launch, without a restart.

```console
$ devvm network transparent
on # devvm: default
$ devvm network transparent off
==> devvm turns transparent mode off for devvm-web-0c3c7077e29f06 at its next launch, and it applies while the VM is on a confined tier.
```

With transparent mode off, names don't resolve in a confined VM at all, so a tool that skips the proxy fails with a DNS error. Most tools, such as curl, get that error at once, on macOS and on a Linux host alike. On a Linux host, a few, such as workerd under `wrangler dev`, can wait about 40 seconds for each lookup before they fail. On the open tier there's no transparent mode, and the VM resolves names with Lima's DNS. A VM made from an image built before devvm had transparent mode lacks the programs it runs, so each launch says so, and `devvm update` makes the VM again from a newer image.

`devvm network transparent --json` prints the same as JSON, with `on` and `from`, which is `vm`, `config` or `default`, and `devvm network --json` has it too, as `transparent`:

```json
{
  "version": 1,
  "on": true,
  "from": "default"
}
```

On the open tier, a tool that ignores the proxy settings in the VM's environment connects directly, past its proxy. Its connections still work, because an open VM reaches everything this machine can, but they don't show in `devvm network log`, they don't go through your company proxy, and a request of theirs that needs one of your secrets carries only its placeholder, so the secret's host refuses it. On every other tier, a tool that transparent mode doesn't cover needs the proxy's address in its own settings. These common tools don't read `HTTP_PROXY` and `HTTPS_PROXY`, or read them only when told to:

- **Node's `fetch`** reads them only with `NODE_USE_ENV_PROXY=1`, from Node 24.0.0 and 22.21.0, or with `--use-env-proxy`, from Node 24.5.0 and 22.21.0. devvm sets `NODE_USE_ENV_PROXY` in every VM, so an older Node's `fetch` is the one that connects directly.
- **Python's aiohttp** reads them only when a program makes its session with `trust_env=True`.
- **Go programs** read them through Go's default HTTP transport, but a program that makes its own transport without a proxy function connects directly.
- **Java** reads none of them. It takes its proxy from the `https.proxyHost` and `https.proxyPort` system properties, such as `-Dhttps.proxyHost=…`, and `http.proxyHost` and `http.proxyPort` for plain HTTP.
- **The Docker daemon** reads them only from its own environment as it starts, or from its `daemon.json`, never from your shell. devvm points the daemon at the proxy itself, as [Inside the VM](#inside-the-vm) says.

## Secrets

Secrets stay on this machine, whatever the VM's tier. A VM gets a placeholder of its own for each one, and its proxy puts the real value into requests for that secret's hosts. An open VM can send a request around its proxy, but then the request carries only the placeholder, which the secret's host refuses ([tools that ignore the proxy](#tools-that-ignore-the-proxy)). The proxy puts each secret into one header:

| Secret | Hosts | Header | Used by |
| --- | --- | --- | --- |
| `claude` | `api.anthropic.com` | `Authorization` | every VM whose image has `claude` |
| `github` | `api.github.com`, `github.com`, `codeload.github.com`, `raw.githubusercontent.com` | `Authorization` | VMs it's bound to |
| [your own](#your-own-secrets) | the hosts you give it, for the paths and methods you give it | `Authorization`, or the header you give it | VMs it's bound to |

Agents' logins work the same way. When a VM's image has a coding agent and devvm's agent store has a login or an API key for it, the VM gets placeholders for the login, and so do Claude Code's MCP servers' secrets ([agents' logins and settings](../internals/network.md#agents-logins-and-settings)). A Claude Code, Codex or opencode login that you make inside a VM goes to the store, and the VM gets placeholders for it too ([Claude Code login](../using/coding-agents.md#claude-code-login)). A VM saves a login only for an agent that its image has, and its proxy refuses a login for any other agent, so a VM without Codex can never replace the store's Codex login. Because the store keeps one login for each agent, any VM with the agent can replace it, so a VM you don't trust can log your other VMs in to an account of its choosing ([logins made in a VM](../internals/network.md#logins-made-in-a-vm)).

### Saving and binding secrets

| Command | What it does |
| --- | --- |
| `devvm secret set claude` or `github` | asks for the Claude Code token or the GitHub token without showing it, and saves it (`--stdin` reads a line instead) |
| `devvm secret set proxy` | asks for the [company proxy](#behind-a-company-proxy)'s password, for the user that `config.toml`'s `[proxy]` names, and saves it (`--stdin` reads a line instead) |
| `devvm secret set NAME --host HOST --env VARIABLE` | asks for a [secret of your own](#your-own-secrets), such as an API key, and saves it with the hosts it goes to and the variable that VMs find its placeholder in (`--stdin` reads a line instead) |
| `devvm secret rm NAME` | deletes a saved secret, after asking |
| `devvm secret` | lists the secrets, each with its variable, header and hosts, a secret of your own's path and methods, whether it's saved, and the VMs using it |
| `devvm secret use NAME` | lets this folder's VM use the secret, and reach its hosts on port 443 |
| `devvm secret unuse NAME` | stops this folder's VM using it, though a VM whose image has `claude` always uses `claude` |

devvm saves a secret in the macOS Keychain, or elsewhere in `~/.local/share/devvm/secrets/<name>`, a file only you can read ([details](../internals/network.md#where-secrets-are-saved)). No devvm command prints a saved secret, because each VM's proxy reads the values it needs itself. devvm checks each value as you save it:

- It drops whitespace in a secret's value, except inside the `proxy` password, which loses only the whitespace around it.
- It refuses a value with a quote, a backslash or a control character, a value that holds a VM's placeholder, such as one copied from a VM's `GH_TOKEN`, and a `claude` value that isn't a setup token (`sk-ant-oat01-…`, from `claude setup-token`).
- A `CLAUDE_CODE_OAUTH_TOKEN` in devvm's environment wins over a saved one.

Each change applies in its own time:

- Saving, deleting, binding and unbinding apply at once to the proxies that are running.
- From its next launch, the VM gets the placeholder in `CLAUDE_CODE_OAUTH_TOKEN`, `GH_TOKEN` or your own secret's variable, and with `github` a git credential helper for `https://github.com`.
- A secret's hosts join the allowlist of each VM that uses it, on port 443 alone, whatever the VM's tier, so a VM on the none tier reaches them too.

To put a secret in, the proxy shows the VM a certificate from the VM's own CA, which no other VM trusts. Each CA lasts 30 days, and a VM that you don't open again keeps working for at least 30 days after its last launch with `devvm` in its folder. When its CA has ended, its proxy refuses its HTTPS connections to secrets' hosts, and `devvm network log --blocked` gives the reason `devvm's CA has ended`, so run `devvm` in the VM's folder to install new CAs ([the VM's CA](../internals/network.md#the-vms-ca)).

`devvm secret unuse` and `devvm secret rm` take a secret away from a VM at once, because the VM only ever had its placeholder. Neither command revokes what a VM made with the secret ([minting credentials with a secret](attack-vectors.md#secrets-and-keys-the-vm-may-use)), so rotate it if that matters. Use a fine-grained GitHub token, scoped to the repos your VMs need.

#### devvm secret --json

`devvm secret --json` prints the list as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes. Each of the `secrets` has its name, `saved`, which is `true` when it's saved or, for `claude`, in devvm's environment, and `used_by`, the names of the VMs using it. Then `custom` is `true` for a [secret of your own](#your-own-secrets), and `hosts` lists the hosts the proxy puts it in for, none for `proxy`. A secret that VMs get has its `header` and `env`, the variable that holds its placeholder, and `prefix` when its placeholder starts with one. A secret of yours also has its `scheme`, its `path`, its `methods` and `basic_user` when it has them. The JSON never holds a secret's value.

```json
{
  "version": 1,
  "secrets": [
    {
      "name": "claude",
      "saved": true,
      "used_by": [
        "devvm-some-project-71a99b1076d92c"
      ],
      "custom": false,
      "hosts": [
        "api.anthropic.com"
      ],
      "header": "Authorization",
      "env": "CLAUDE_CODE_OAUTH_TOKEN",
      "prefix": "sk-ant-oat01-"
    },
    {
      "name": "github",
      "saved": false,
      "used_by": [],
      "custom": false,
      "hosts": [
        "api.github.com",
        "github.com",
        "codeload.github.com",
        "raw.githubusercontent.com"
      ],
      "header": "Authorization",
      "env": "GH_TOKEN"
    },
    {
      "name": "proxy",
      "saved": false,
      "used_by": [],
      "custom": false,
      "hosts": []
    },
    {
      "name": "stripe",
      "saved": true,
      "used_by": [
        "devvm-some-project-71a99b1076d92c"
      ],
      "custom": true,
      "hosts": [
        "api.stripe.com"
      ],
      "header": "Authorization",
      "env": "STRIPE_API_KEY",
      "prefix": "sk_"
    }
  ]
}
```

### Your own secrets

Any other credential that a tool in a VM sends in a header, such as an API key for Stripe, OpenRouter or your company's own service, can be a secret of your own. You give it a name, the hosts it goes to and the variable that VMs find its placeholder in, and devvm asks for the value without showing it:

```text
$ devvm secret set stripe --host api.stripe.com --env STRIPE_API_KEY --prefix sk_
Paste the value of stripe, which VMs that use it send to api.stripe.com.
The VMs that use it can reach those hosts on port 443.
Token:
==> devvm saved the stripe secret in the Keychain.
$ devvm secret use stripe
==> devvm-web-71a99b1076d92c uses the stripe secret from its next launch, and its allowlist has api.stripe.com on port 443.
```

From its next launch, the VM has `STRIPE_API_KEY` set to its own placeholder, such as `sk_devvm-proxy-managed-secret-stripe-…`, and a tool there sends it as `Authorization: Bearer $STRIPE_API_KEY`. The proxy puts the real value in place of the placeholder only in requests to `api.stripe.com` on port 443, over TLS, and a request to any other host goes out with the placeholder alone. A program that loads a `.env` file gets the placeholder too, as long as the file doesn't set `STRIPE_API_KEY` itself, because most dotenv libraries leave a variable that's already set as it is. Keep the real key out of the project's `.env`, which the VM can read.

A secret of yours goes only to the VMs you use it in with `devvm secret use`, and each one's hosts then join that VM's allowlist on port 443. `devvm secret set` takes these flags:

| Flag | What it gives the secret |
| --- | --- |
| `--host HOST` | a host it goes to, such as `api.stripe.com`: repeat the flag, or join hosts with commas, for up to 16 |
| `--env VARIABLE` | the variable that VMs find its placeholder in, such as `STRIPE_API_KEY` |
| `--header HEADER` | the header it goes in, such as `X-Api-Key`, when it isn't `Authorization` |
| `--prefix PREFIX` | what its placeholder starts with, such as `sk_`, for a client that checks how its key starts |
| `--path PATH` | the path of the requests it goes in for, written as a [rule's path](#rules-by-method-and-path) is, such as `/api/` for everything below `/api/`, so a request to the host's other paths carries the placeholder alone, or `none` for every path |
| `--method METHODS` | the methods of the requests it goes in for, such as `GET,HEAD`, so a request with another method carries the placeholder alone, or `none` for every method |
| `--scheme SCHEME` | a scheme of its own before it in `Authorization`, besides `Bearer` and `Token`, such as `ApiKey`, or `none` |
| `--basic-user` | lets it go in as the username of `Basic` credentials, as Stripe and some other services take a key (`--basic-user=false` takes that away) |

The proxy puts the value in only when exactly one of the header's values holds the placeholder, in one of these shapes, so that the value lands where the server reads a credential:

- the placeholder alone, such as `X-Api-Key: PLACEHOLDER`;
- for `Authorization`, `Bearer`, `Token` or the secret's own `--scheme`, in any case, then one space and the placeholder;
- for `Authorization`, `Basic` credentials whose password is the placeholder, after a username of printable ASCII up to 64 characters, such as git's `x-access-token:PLACEHOLDER`;
- with `--basic-user`, `Basic` credentials whose username is the placeholder, before a password of printable ASCII up to 64 characters, such as `PLACEHOLDER:`.

The placeholder in any other shape stays as it is, and the proxy's own log says why without the value. That includes bytes of the VM's own around it, the placeholder twice, a `Digest` field, and the username of `Basic` credentials without `--basic-user`, because a server can show the VM those parts of a request it refuses, such as a username it doesn't know. The same shapes apply to the `github` and `claude` secrets, which the GitHub CLI, git and Claude Code send as `Bearer`, `token` and git's `Basic` password.

Without `--path` and `--method`, the value goes in for every path and every method. With them, the proxy judges each request as it judges an allow by method and path: by its method as written and each method it names in a method-override header or a `_method` parameter, and by its path only in a plain form ([rules by method and path](#rules-by-method-and-path)). So a key that may only read goes in for `GET` and `HEAD` alone:

```text
$ devvm secret set reports --host api.example.com --env REPORTS_API_KEY --method GET,HEAD
Paste the value of reports, which VMs that use it send to api.example.com.
The VMs that use it can reach those hosts on port 443.
Token:
==> devvm saved the reports secret in the Keychain.
```

A `POST` from the VM to `api.example.com` then carries the placeholder, which the host refuses, while a `GET` gets the key. The proxy judges every request on a connection, so a kept-alive connection or an HTTP/2 one gets the key only in the requests that have those methods. The `github` and `claude` secrets go in for every method. To keep a VM from writing with your GitHub token, give it a deny instead, such as `devvm network deny api.github.com --method POST,PUT,PATCH,DELETE`, which refuses those requests before they leave the proxy.

devvm refuses a secret that breaks these rules:

| Part | Rules |
| --- | --- |
| name | lowercase letters, digits and dashes, and not `claude`, `github` or `proxy`, nor starting with a coding agent's name, such as `codex-key` |
| host | a host name, such as `api.example.com`. devvm refuses a wildcard, such as `*.example.com`, because some domains let anyone own a name under them. It also refuses an address, any port but 443, and a name that's only ever internal, such as `localhost`, `printer.local` or a Tailscale or `nip.io` name. The proxy refuses an internal address whatever name resolves to it anyway |
| header | not `Host`, `Cookie`, `Content-Length`, `Content-Type`, `Transfer-Encoding`, `Expect`, `Forwarded`, `Via`, an `X-Forwarded-` or `Proxy-` header, or one that ends at the next hop, such as `Connection`, because the proxy drops or sets those itself. Nor one that servers commonly log or send back, such as `Origin`, `Referer`, `User-Agent`, `X-Request-Id` or `Traceparent` |
| variable | not one that devvm or the VM's shell sets, such as `PATH`, `GH_TOKEN`, `HTTPS_PROXY`, `NPM_CONFIG_PREFIX` or any `DEVVM_` variable, nor one that changes what a program runs or trusts, such as a `GIT_` or `PYTHON` variable, `NODE_OPTIONS`, `EDITOR` or `SSL_CERT_DIR`, nor another secret's |
| prefix | not holding `devvm-proxy-managed`, which every placeholder holds after its prefix |
| value | printable ASCII alone and at most 3,800 characters, and devvm refuses a longer one before it touches the one saved |

To save a new value or change a secret, give `devvm secret set` only what changes:

```sh
devvm secret set stripe                                            # save a new value, such as after you rotate the key
devvm secret set stripe --host api.stripe.com,files.stripe.com     # give it a second host
devvm secret rm stripe                                             # delete it, after asking
```

- Run without the other flags, `devvm secret set NAME` keeps the secret's hosts, header, variable, prefix, path, methods and scheme. To change one of those, give only its flag with the new value, and the rest stay as they are.
- `--host` gives the whole list of hosts each time. A host the secret loses goes from every VM at once. A host it gains reaches only the VMs you then run `devvm secret use NAME` in again, so a host you add never gets the value from a VM you didn't give it to, and devvm names the VMs that keep the hosts they had.
- To take a prefix away, delete the secret and save it again.
- Running VMs keep the variable they launched with, so after you change `--env` or `--prefix`, a VM that uses the secret gets the new placeholder at its next launch.

`devvm secret rm NAME` deletes the secret with its definition after asking, and the question names the VMs that use it, which stop using it before it goes. It also deletes a value that a failed save left without its definition. Each VM records the secret it was bound to by an ID that the secret gets as it's first saved, so a secret saved again later by the same name goes to none of those VMs until you use it there again. A VM still bound to an earlier secret of the name gets nothing of the new one, its launch says so, and `devvm secret unuse NAME` takes the name away whether a secret has it or not.

devvm keeps your secrets' hosts, headers, variables, prefixes, paths, methods and schemes in `~/.local/share/devvm/custom-secrets.json`, without their values, and checks each entry again whenever it reads the file. When it can't read the file, or an entry breaks the rules above, such as after an edit by hand, devvm secret and the launch of each VM that uses one of your secrets fail and name the file. Fix the entry, or delete the file and save your secrets again with devvm secret set ([details](../internals/network.md#where-secrets-are-saved)).

> [!WARNING]
> The proxy passes the replies from a secret's hosts to the VM as they are, so a host that sends the request's headers back, such as an echo or debug endpoint, shows the VM the real value. Give a secret only hosts that you trust with it, and a key scoped to what the VMs need. A secret's hosts are also open to the VMs that use it for requests without the placeholder, on every tier, the none tier included. So on a host where anyone can make an account, such as a storage or payments service, code in the VM can send data to an account of its own there.

## Logs

`devvm network log` shows the proxy's decisions for this folder's VM, oldest first, one line each. A line has the time, `allowed`, `BLOCKED` or `cache`, and the host and port. An allowed line then has the rule that allowed it (`, decrypted` for an intercepted tunnel), or the request (`GET /user`), with the [rule by method and path](#rules-by-method-and-path) that let it through when one did, and the secrets put in (`, token: github`). A blocked line has why it was blocked, after the request when the proxy saw one, and a `cache` line, a request to the [shared Nix cache](#nix), has the request and what came of it. A line ends `, without proxy settings` when [transparent mode](#tools-that-ignore-the-proxy) sent the connection to the proxy:

```text
2026-10-09T10:00:00Z  BLOCKED  example.com:443  not on the allowlist
2026-10-09T10:00:05Z  allowed  github.com:443  rule github.com
2026-10-09T10:00:06Z  allowed  api.github.com:443  GET /user, token: github
2026-10-09T10:00:07Z  BLOCKED  api.github.com:443  POST /gists: denied by !api.github.com POST,PUT,PATCH,DELETE
2026-10-09T10:00:08Z  cache    cache.nixos.org:443  GET /nix/yv1zf3y6all4j99aad30cb8i55dw9zmp.narinfo: hit
2026-10-09T10:00:09Z  allowed  api.open-meteo.com:443  rule **, without proxy settings
```

| | |
| --- | --- |
| `-n N`, `--lines N` | the last N (default 50) |
| `--follow` | then each new one as it happens, until Ctrl+C |
| `--blocked` | the blocks instead, one row per host and port, by when last blocked: `LAST`, `COUNT`, `HOST` and `ALLOW WITH`, the `devvm network allow` command that allows it |

`ALLOW WITH` is `-` for a request that the proxy refuses whatever the allowlist says, such as a GitHub call that makes a credential, and for a host that isn't a host name or an address, and `- (quiet)` for a [quiet host](#quiet-hosts). An open VM's log holds what went through its proxy and its links' connections, but not what the VM reached directly, and `devvm network log` says so. The proxy keeps each VM's log to its last 10 to 20 MiB by starting a new file once the current one reaches 10 MiB and keeping one older file, which `devvm network log` reads too. `devvm rm` deletes the VM's logs, and [How it works](../internals/network.md#log-files) describes the files.

### devvm network log --json

`devvm network log --json` prints the last N events as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes, and devvm refuses it with `--follow`. Each of the `events` has what the proxy logged, without the VM's name, and a log with nothing in it gives `"events": []`:

| Field | What it holds |
| --- | --- |
| `time`, `host`, `port` | when, and the host and port, which for a link's connection is the link's name |
| `event` | `tunnel` for an allowed connection, `intercept` for an allowed tunnel that the proxy decrypted, `forward` for an allowed request, `blocked`, or `nix-cache` for a request to the shared Nix cache. A link's connection is `link` when devvm carried it, and `link_refused`, `link_target_stopped` or `link_limit` when devvm refused it |
| `method`, `path` | the request's, when the event has them |
| `rule` | the rule that allowed it |
| `reason` | the reason it was blocked, or what came of a cache request |
| `agent` | the request's `User-Agent` header |
| `secrets` | the secrets put in |
| `refused` | for a request refused whatever the allowlist says |
| `quiet` | for a block of a [quiet host](#quiet-hosts) |
| `upstream` | the company proxy it went through |
| `target` | the VM that a link's connection went to |
| `via` | `dns` for a connection that [transparent mode](#tools-that-ignore-the-proxy) sent to the proxy |

Each field but `time`, `event`, `host` and `port` is there only when the event has it.

```json
{
  "version": 1,
  "events": [
    {
      "time": "2026-10-09T10:00:00Z",
      "event": "blocked",
      "host": "example.com",
      "port": 443,
      "method": "CONNECT",
      "reason": "not on the allowlist"
    },
    {
      "time": "2026-10-09T10:00:06Z",
      "event": "forward",
      "host": "api.github.com",
      "port": 443,
      "method": "GET",
      "path": "/user",
      "secrets": [
        "github"
      ]
    }
  ]
}
```

With `--blocked`, each of the `blocked` has the `last` time it was blocked, the `count`, the `host` and the `port`, and `allow`, the rule that `devvm network allow` takes to allow it. A block that `ALLOW WITH` shows as `-` has no `allow`, and a quiet host's has `"quiet": true` instead.

```json
{
  "version": 1,
  "blocked": [
    {
      "last": "2026-10-09T10:01:00Z",
      "count": 2,
      "host": "example.com",
      "port": 443,
      "allow": "example.com"
    }
  ]
}
```

## Links between VMs

A [link between VMs](../using/links.md) lets a program in one VM, the client, reach a port that another VM, the target, publishes. devvm carries a link's connections over ssh through this machine, so they use neither VM's network, and a confined VM reaches only what its tier allows and the links you added to it. A client's proxy is a second way into its links: it carries a request for one of the client's own links, by the link's name and port, to the same link on this machine, and passes it on as it is, without looking into it or putting a secret into it. It reaches no other socket on this machine and no other VM's links. The allowlist's rules don't apply to links, so `devvm network deny` doesn't stop one, and `devvm link rm` does.

A link joins the two VMs' trust. Code in the client can often run code in the target through the target's service, such as Postgres's `COPY … TO PROGRAM`, and it then has whatever the target has: its network, the secrets it may use and its project folder. So devvm keeps two rules:

- **A client links only to a target on its own tier or a stricter one.** An allowlist client links to allowlist and none targets, but not to internet or open ones, because a link to a target on a lower tier would let the client reach more through the target, around its own proxy. An open client may link to any target, and it then has the target's secrets and project folder within reach as well as its own network.
  - devvm checks this when you add a link, at each launch of the client and on every connection, and for a client on a confined tier, on every connection the target's tier must be in force as it runs, so devvm refuses the connection while the target shows as `unenforced`, and it warns you of such a target when you add the link, when you publish a port in its folder that the client links to, and when the client starts.
  - While such a link exists, devvm refuses to give the target a tier lower than the client's, such as `devvm network open`, and it refuses to give the client a tier higher than a target's, such as `devvm network allowlist` for a client that links to an open target.
- **The target publishes.** A link reaches only a port that the target's folder publishes with `devvm link publish`, and only on the target's own loopback. Publishing offers the port to any of your VMs, and each link is still added in the client's folder, so a VM reaches another's service only when you've run a command in both folders.

The first rule keeps a client's tier whole, but it doesn't keep the target's data in the target. A client on a confined tier can read whatever the target's service gives it and send that out through the hosts its own tier reaches. Link two VMs only when you'd trust each with what the other has.

A [group](../using/links.md#groups) links each of its VMs to the ports that the others publish, once you've run `devvm group join` in each VM's folder. Its VMs are all on one tier, so every link it makes keeps the first rule, and devvm refuses to change a VM's tier with `devvm network` while it's in a group. A group joins the trust of all its VMs, so put VMs in one only when you'd trust each with what the others have.

## Not covered

The tiers leave some things open, such as what an allowed host accepts beyond what [rules by method and path](#rules-by-method-and-path) narrow, whatever your secrets allow, such as pushes and API calls, and what a company CA that you add can read. [Attack vectors](attack-vectors.md) lists them under [Narrowed, not closed](attack-vectors.md#network) and [Left to you](attack-vectors.md#the-network). What transparent mode leaves to a tool's own proxy settings, such as other ports, databases' protocols, addresses and HTTP/3 alone, is under [tools that ignore the proxy](#tools-that-ignore-the-proxy).
