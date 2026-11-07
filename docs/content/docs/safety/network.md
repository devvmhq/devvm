---
title: Network tiers
explainer: Choose what each VM can reach on the network
weight: 1
---

Every VM goes through a proxy that devvm runs for it on this machine, and the VM's network tier decides what the proxy lets it reach. New VMs get the allowlist tier by default, so they reach only the hosts on their allowlist. No VM holds your secrets, whatever its tier, because its proxy puts them into its requests. A secret is a credential devvm keeps on this machine for VMs to use: your GitHub token, Claude Code's token, an agent's login or an API key. An open VM, the one tier that isn't confined to its proxy, reaches anything this machine can, including this machine's own services on `localhost` and your LAN.

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

`--network TIER` chooses a new VM's tier, and `devvm network` with a tier changes an existing VM's, such as `devvm network internet` ([choosing a tier](#choosing-a-tier)). A higher tier is stricter, and a VM on any tier but open is confined: it reaches nothing but its proxy, and a direct connection from it fails.

An open VM runs on its backend's own network, so it reaches this machine and your LAN directly, as a VM without a proxy would. Its environment names its proxy all the same (`HTTP_PROXY`, `HTTPS_PROXY` and their lowercase forms), and the proxy reaches every host and address for it, this machine's `localhost` and your LAN included, so a tool behaves the same through the proxy as it does directly. Its `NO_PROXY` names the VM itself and this machine as the VM reaches it, `host.lima.internal`, so requests to those go directly. Every launch checks that an open VM's proxy is set in the VM and answers it, and when it doesn't, devvm warns you and opens the VM all the same. An open VM gets a placeholder for each secret it may use, as every VM does, and only its proxy puts the real value in. So a request that goes around the proxy, directly from the VM, carries only the placeholder, and the secret's host refuses it.

On the internet and none tiers, devvm reads only the denies of your policy files: the internet tier allows every public host itself, and the none tier allows only what the VM's agents, secrets and links need. `devvm network` marks each such file `(denies only)`, and when you add an allow to a VM on either tier, devvm says that the allow changes nothing until the VM is on the allowlist tier. On the open tier no rule applies at all.

Changing a VM from one confined tier to another applies at once, because the VM's proxy starts again with the new tier's rules and the VM keeps running. A change into or out of the open tier applies from the VM's next start, as [below](#choosing-a-tier) describes. `devvm ls` and `devvm network` show a VM's tier, or `unenforced` when a confined tier isn't in force ([when it isn't enforced](#when-it-isnt-enforced)).

## Choosing a tier

A new VM gets the allowlist tier unless you choose another, and the summary devvm shows before it makes the VM names its tier. When you customize the VM in a terminal, devvm lists the four tiers, each with what it reaches, and the tier the summary showed is chosen ([what a new VM gets](../using/images.md#what-a-new-vm-gets)):

```text
Network tier for devvm-some-project-71a99b1076d92c (up/down to choose, enter to confirm):
  ( ) open       reaches anything this machine can
  ( ) internet   reaches any public host
> (*) allowlist  reaches only the hosts you allow (recommended)
  ( ) none       reaches only its agents' and secrets' hosts and its links
```

Up and down move the choice, and enter gives the VM the chosen tier, so pressing enter at once keeps the tier the summary showed. `--network TIER` answers the question up front, such as `devvm --network internet`, and when there's no terminal to ask in, or you give `--no-input`, the VM gets the tier the summary showed. A VM keeps its tier. On an existing VM devvm ignores `--network` and warns you, naming the `devvm network` command that changes the tier.

To give every new VM another tier, save it with `devvm network -g`, such as `devvm network -g none`, or choose it in [devvm setup](../using/settings.md#devvm-setup). Each new VM's summary then shows that tier, and customizing the VM still asks, with Enter keeping the saved tier. `devvm network -g` shows what new VMs get, `allowlist` until you save another tier, followed by where it came from after a `#`, and `devvm network -g --json` prints it as `policy`, with `source`. devvm reads the saved tier only when it creates a VM, so the VMs you already have keep their own tier, and `--network` still chooses for one new VM. You can also set it with `network` in [`config.toml`](../using/settings.md#configtoml), which wins over the saved tier, and then `devvm network -g` refuses to change it and names the file ([where a default comes from](../using/settings.md#where-a-default-comes-from)).

Lima confines every VM to its proxy, so every VM can be on any tier, the docker layer's included. devvm never starts a VM on a confined tier with an open network, so if a VM's backend couldn't confine it, the VM could only be open: devvm would refuse to make it on any other tier unless you give `--network open`, and it would refuse to give it any other tier with `devvm network` ([when a backend lacks something](../backends/_index.md#when-a-backend-lacks-something)).

| | |
| --- | --- |
| `devvm network` | print this folder's VM's tier as `devvm ls` shows it, `open`, `internet`, `allowlist`, `none` or `unenforced`, and then its [allowlist](#changing-rules) |
| `devvm network open\|internet\|allowlist\|none` | change it: between confined tiers at once, and into or out of open from the VM's next start, which `devvm restart` gives you, and for a running VM, ask in a terminal whether to restart it now |
| `devvm network -g [open\|internet\|allowlist\|none]` | show or change what new VMs get |

When a change into or out of open finds the VM running, devvm asks in a terminal whether to restart it now, which stops anything running in it, and if you answer yes, it restarts the VM as `devvm restart` does, so the change applies at once. If you answer no, or there's no terminal, or you pass `--no-input`, the change waits until the VM starts again. If you give a running open VM a confined tier, the VM isn't confined yet, so `devvm ls` shows it as `unenforced`, and its next launch offers to restart it confined, as it does for a VM started outside devvm. If you make a VM open while it runs confined, its launches keep it confined to its proxy, which now applies an open VM's rules, until it next starts, and they warn you that the VM leaves its proxy's confinement when it next starts. In a terminal, each of those launches first asks whether to restart the VM now, with no as the default unless an agent's new folders wait too ([restarting](../using/vms.md#restarting-and-stopping)).

### When it isn't enforced

On every launch devvm checks on this machine that the VM still runs on the process devvm started confined, and that a direct connection from the VM to the internet fails. A VM that fails either check shows as `unenforced`, and devvm offers to restart it confined or stops it, as [the self-check](../internals/network.md#the-self-check) describes.

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

A policy file holds one rule per line. `#` starts a comment, and matching ignores case.

| Rule | Matches |
| --- | --- |
| `example.com` | that host only |
| `*.example.com` | any subdomain, at any depth, not `example.com` itself |
| `**` | any host, and any address, though an internal address still needs its own rule |
| `10.0.0.5`, `10.0.0.0/8`, `fd00::/8` | addresses, IPv4 or IPv6 (also what lets a connection past the internal-address refusal) |
| `RULE:22`, `[fd00::1]:22`, `10.0.0.0/8:8080` | only that port; without a port, 443 and 80 |
| `RULE:*`, `**:*` | any port |
| `!RULE` | deny: a deny beats any allow |

A rule is plain ASCII with no spaces in it, and a port is a number from 1 to 65535, or `*` for any port. A host's one trailing dot is dropped, so `example.com.` matches `example.com`. Brackets hold only an IPv6 address, and an address can't follow `*.`. A line that holds anything else, such as `example.com:x`, `a*.example.com` or `! example.com` with a space after the `!`, isn't a rule.

When a line that isn't a rule starts with `!`, it reads as a deny, but the proxy can't tell what it would deny. So while the line is there, the proxy of every VM that reads the file denies every new connection apart from the VM's [links](#links-between-vms), whatever the other rules allow. Connections that are already open stay open. Each refusal names the file and the line, and it suggests no `devvm network allow` command, because no rule would help. Any other line that isn't a rule is skipped, and the file's other lines still apply. The proxy names each such line in its own log.

devvm warns about each line that isn't a rule, with the file and the line's number, and the warning for a broken deny says that the proxies deny everything until it's fixed. It warns when you open a confined VM whose proxy reads the file, when `devvm network allow`, `deny` or `rm` changes the file, and in `devvm doctor`. `devvm network` also marks the line. Fix the line or remove it, and the change applies from the next connection.

The proxy never dials a denied address, even for an allowed name that resolves to it or to its NAT64 form. A missing file has no rules. A file that changed but can't be read keeps its last rules, so a deny never lapses.

### Changing rules

`devvm network` shows this folder's VM's allowlist after its tier. It prints each of the VM's policies that holds more than comments, and then its rules without their comments. A built-in policy has its name after `# devvm:`, because it has no file you could edit, and one of your files has its path after `#`, followed by `(denies only)` on the internet and none tiers, where it shows only the file's denies. An open VM has no policies, so `devvm network` prints its tier alone. A line that isn't a rule is marked `# not a rule, skipped`, with its line number:

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

A rule is what [Rules](#rules) lists, without spaces or a comment. devvm refuses anything else before it adds any rule, and it also refuses `allow !RULE`. A rule that's already in the file, in any case, isn't added again. Each change applies at once, from the next connection. For an open VM, devvm notes that no rule applies to it, and for an allow on the internet or none tier, that only denies apply there. When `devvm network log --blocked` or a blocked request in the VM suggests a `devvm network allow` command, its rule is one that `devvm network allow` takes.

If a VM was deleted outside devvm, devvm deletes what it kept for that VM when it makes a new VM for the same project folder, so the new VM never gets the old one's rules.

### devvm network --json

`devvm network --json` prints the VM's tier and its allowlist as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes. `policy` is `open`, `internet`, `allowlist`, `none` or `unenforced`, and `allowlist` has each policy that holds more than comments, in the order the proxy reads them. Each has `builtin`, a built-in policy's name, or `file`, the path of one of your files, then its rules and, in `not_rules`, each line that isn't a rule, with its `line` number and its `text`. On the internet and none tiers, a file whose denies alone apply also has `denies_only` set to `true`, and its `rules` are its denies, as `devvm network` lists them under `(denies only)`. Without `denies_only`, a file's `rules` are all of its rules. With `-g`, the object has only `policy`, which is the tier new VMs get, and `source`, which says where it came from: `config.toml`, `settings.json` for what `devvm network -g` or `devvm setup` saved, or `built-in` ([where a default comes from](../using/settings.md#where-a-default-comes-from)). With `--json` the command only shows, so devvm refuses a tier with it.

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
  ]
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

Every login in the VM has the proxy settings in its environment (`HTTP_PROXY`, `HTTPS_PROXY` and their lowercase forms, `NO_PROXY` for the VM itself, and `NODE_USE_ENV_PROXY`), and the [CA variables](../using/settings.md#a-companys-ca) that every VM has point the tools that don't read the system's store at the VM's own CAs. devvm also points apt, sudo, ssh and Docker at the proxy ([how](../internals/network.md#the-vms-setup)), on every tier. On a confined tier, a direct connection fails at once, even as root, and so does DNS, because names resolve in the proxy. A tool that skips the proxy, even for a host the rules allow, fails with a DNS or connection error rather than the proxy's `403`, and nothing reaches the log ([what to do](../using/troubleshooting.md#a-host-on-the-allowlist-doesnt-resolve)).

A blocked request gets `403`, and the response names the host, the reason and the rule that would allow it. The proxy also answers at `http://devvm.proxy/`:

- `/blocked` lists the last 50 blocks.
- `/blocked/new?after=N` gives the number of blocks so far, then those after the Nth, with each rule once, so a shell prompt can show what a command hit.
- `/ca.pem` is the VM's own CA certificate and the next one's, never their keys.
- `/nix/` is the shared Nix cache, in a VM with the `nix` layer ([below](#nix)).

In the VM, `devvm-why` prints what the proxy blocked lately, and a note before each prompt tells you when something new was blocked. When the proxy blocked nothing lately, `devvm-why` says so, and it adds that a DNS or connection error for a host the tier allows means the tool skipped the proxy, so you turn on the tool's own proxy setting or give it the address in `$HTTPS_PROXY` ([a host on the allowlist doesn't resolve](../using/troubleshooting.md#a-host-on-the-allowlist-doesnt-resolve)). devvm also tells Claude Code and Codex, in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, why a request may fail and to run `devvm-why`, and that such an error while `devvm-why` shows nothing means the tool skipped the proxy.

### Nix

In a VM with the [`nix` layer](../using/images.md#nix), Nix's daemon makes most of Nix's downloads, and devvm also points it at the proxy, so its downloads go through the same allowlists. The layer's allowlist has `cache.nixos.org` for Nix's binary cache, and `channels.nixos.org` and `releases.nixos.org` for its channels and flake registry, and GitHub, where most flakes' inputs come from, is in devvm's rules for every VM.

Nix asks the proxy's shared cache, `http://devvm.proxy/nix`, before cache.nixos.org, so every VM with the layer shares the packages that any of them downloaded ([shared downloads](../using/toolchains.md#nix-projects)). The proxy fetches what the cache doesn't have from cache.nixos.org alone, and only while the VM's tier and rules let it reach `cache.nixos.org`. It keeps a package only when cache.nixos.org's signature on it checks, and the VM's Nix checks that signature again. A VM can only make the proxy fetch from cache.nixos.org: the proxy refuses anything a VM sends to store, and every request that isn't for one of Nix's cache files. Each request shows in `devvm network log` as `cache`, with whether it was a `hit`, `fetched`, or why it wasn't found. What one VM can learn of another through the cache is under [Attack vectors](attack-vectors.md#network). An open VM has its proxy too, so it shares the cache as well, and its proxy fetches from cache.nixos.org for it.

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

The VM's proxy still decides each request by the host name the VM asked for, against the same allowlists, and only then passes it on. It sends tunnels, which carry HTTPS and the requests it puts your secrets into, through `HTTPS_PROXY`'s proxy, and plain HTTP requests through `HTTP_PROXY`'s. It reaches the hosts that `NO_PROXY` names directly, and also `localhost` and loopback addresses. In `NO_PROXY`, `example.com` matches that host and its subdomains, `.example.com` matches only its subdomains, an address or a CIDR matches addresses, any entry can end in a port, and `*` matches every host. `devvm network log` adds `through host:port` to each request that went through the company proxy.

The company proxy needs an `http://` address, such as `http://user:password@proxy.example.com:8080` in `HTTPS_PROXY`, and a proxy without a port is on port 80. devvm doesn't start a VM's proxy while `HTTPS_PROXY`, `HTTP_PROXY` or a `[proxy]` key names an `https://` or `socks5://` proxy, and it tells you which setting to change. The company proxy's password stays on this machine. The VM never gets it, and devvm writes it into no log, no image and no process's arguments.

`config.toml` never holds the password, because it's a file you might copy to other machines or keep with your dotfiles, and devvm refuses a `[proxy]` URL that has one. Put only the user in the URL, as `http://alice@proxy.example.com:8080`, and save the password with `devvm secret set proxy`, which keeps it as it keeps the other [secrets](#saving-and-binding-secrets). devvm's proxies look it up themselves when they start, so no file of devvm's holds it in the clear. devvm doesn't make a new VM, start a VM's proxy, or build an image or the shared tools while `config.toml`'s proxy names a user and no password is saved, and it checks before anything starts. A VM's proxy reads the password only when it starts, so after you change it, run `devvm restart` in each VM's folder. No VM ever gets the `proxy` secret, and `devvm secret use proxy` refuses.

The company proxy sees the name and port of every host the VM reaches, and if it inspects TLS, the [CA you add for it](../using/settings.md#a-companys-ca) also lets it read the traffic. The company proxy also looks up each name itself, so the VM's proxy can't check every address a name leads to. It still refuses an address that's this machine's own or internal, unless a rule names it, and it refuses a name when this machine's lookup gives an internal or denied address for it. A name that this machine can't look up goes to the company proxy unchecked only when an allowlist names it, as `example.com` or `*.example.com`. When only `**` allows a name that this machine can't look up, the VM's proxy refuses it, so `**` never lets a VM send the company proxy a name of its own making. What a VM can still reach through the company proxy, such as hosts on your company's network, is under [Attack vectors](attack-vectors.md#network).

### Open VMs and builds

An open VM goes through its proxy as every VM does, and its proxy sends its requests through the company proxy, from the shell's settings or `config.toml`'s, so the company proxy's password stays on this machine for it too. The VM never gets the shell's proxy settings, and Lima gets none to pass on to it. What an open VM reaches directly, past its proxy, such as the hosts its `NO_PROXY` names, doesn't go through the company proxy.

Lima prints each proxy setting it gives a VM, password and all, as it starts the VM. devvm keeps the password out of its [run log](../using/troubleshooting.md#a-command-fails), and once the VM has started, it replaces the password with as many `*` in the log Lima keeps for the VM.

Builds never get your company proxy's settings, and images never keep them. A build goes through the shell's company proxy, or else `config.toml`'s, as a VM's proxy does. When devvm builds an image, builds the shared tools or converts a `--from` container image on Lima behind a company proxy, the build goes through a proxy of devvm's on this machine, made for that build alone, which goes through your company proxy. The build's commands get only that proxy's address, with a password devvm makes for the build, while they run, and devvm stops the proxy when the build ends. So the code a build runs, which includes a container image's own, never sees your company proxy's address or password. That proxy also runs on this machine, so a company proxy on `localhost`, such as one a VPN client runs, works for builds too.

A build's proxy allows any host, as `**` does. It sends a name that this machine can't look up on to the company proxy, so builds work where only the company proxy looks up outside names.

devvm's own downloads on this machine go through the company proxy too. They are devvm's copy of mise and the versions it looks up for the shared tools, the list of Ubuntu's cloud images, and a container registry's answers for `--from`. With the shell's settings, each of them uses the shell's. With `config.toml`'s, devvm's own requests send the password only to the company proxy. The mise it runs on this machine gets the proxy's URL with the password in it, in its environment, because mise takes a proxy's password nowhere else. devvm percent-encodes the password in that URL and mise decodes it before it sends it, so a password with characters such as `@`, `:`, `/`, `#`, `%` or a space works too. devvm never puts that URL in mise's arguments or in a file. When a VM's proxy refreshes a Codex or opencode login on this machine, the refresh goes through the same company proxy as the VM's requests, with the password kept on this machine.

## Tools that ignore the proxy

On the open tier, a tool that ignores the proxy settings in the VM's environment connects directly, past its proxy. Its connections still work, because an open VM reaches everything this machine can, but they don't show in `devvm network log`, they don't go through your company proxy, and a request of theirs that needs one of your secrets carries only its placeholder, so the secret's host refuses it. On every other tier such a tool's direct connection fails, so it needs the proxy's address in its own settings. These common tools don't read `HTTP_PROXY` and `HTTPS_PROXY`, or read them only when told to:

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

Agents' logins work the same way. When a VM's image has Codex or opencode and devvm's agent store has a login for it, the VM gets placeholders for the login, and so do Claude Code's MCP servers' secrets ([agents' logins and settings](../internals/network.md#agents-logins-and-settings)). A Claude Code, Codex or opencode login that you make inside a VM goes to the store, and the VM gets placeholders for it too ([Claude Code login](../using/coding-agents.md#claude-code-login)). Because the store keeps one login for each agent, any VM with the agent can replace it, so a VM you don't trust can log your other VMs in to an account of its choosing ([logins made in a VM](../internals/network.md#logins-made-in-a-vm)).

### Saving and binding secrets

| Command | What it does |
| --- | --- |
| `devvm secret set claude` or `github` | asks for the Claude Code token or the GitHub token without showing it, and saves it (`--stdin` reads a line instead) |
| `devvm secret set proxy` | asks for the [company proxy](#behind-a-company-proxy)'s password, for the user that `config.toml`'s `[proxy]` names, and saves it (`--stdin` reads a line instead) |
| `devvm secret rm NAME` | deletes a saved secret |
| `devvm secret` | lists the secrets, whether each is saved, and the VMs using each |
| `devvm secret use NAME` | lets this folder's VM use the secret |
| `devvm secret unuse NAME` | stops this folder's VM using it, though a VM whose image has `claude` always uses `claude` |

devvm saves a secret in the macOS Keychain, or elsewhere in `~/.local/share/devvm/secrets/<name>`, a file only you can read ([details](../internals/network.md#where-secrets-are-saved)). No devvm command prints a saved secret, because each VM's proxy reads the values it needs itself. devvm drops whitespace in a secret's value, except inside the `proxy` password, which loses only the whitespace around it. It refuses a value with a quote, a backslash or a control character, and a `claude` value that isn't a setup token (`sk-ant-oat01-…`, from `claude setup-token`). A `CLAUDE_CODE_OAUTH_TOKEN` in devvm's environment wins over a saved one.

Saving, deleting, binding and unbinding apply at once to the proxies that are running. From its next launch, the VM gets the placeholder in `CLAUDE_CODE_OAUTH_TOKEN` or `GH_TOKEN`, and with `github` a git credential helper for `https://github.com`.

To put a secret in, the proxy shows the VM a certificate from the VM's own CA, which every VM has, an open one's included, and which no other VM trusts, so one VM's proxy can't make certificates that another VM accepts. Each CA lasts 30 days, and its key stays on this machine. Each launch installs the VM's current CA and the next one, which starts a week before the current one ends, and the proxy switches to the next CA then on its own, without a launch. So a VM that you don't open again keeps working for at least 30 days after its last launch with `devvm` in its folder. Connecting with `ssh` alone isn't a launch. When a VM runs past its next CA's end without a launch, its proxy refuses its HTTPS connections to secrets' hosts, and `devvm network log --blocked` gives the reason `devvm's CA has ended`. The VM's other requests keep working. Run `devvm` in the VM's folder to install new CAs, which also starts the VM's proxy again and ends the connections open through it ([how](../internals/network.md#the-vms-ca)). `devvm rm` deletes a VM's CAs with the VM, and a VM keeps them when its tier changes.

`devvm secret unuse` and `devvm secret rm` take a secret away from a VM at once, because the VM only ever had its placeholder. Neither command revokes what a VM made with the secret ([minting credentials with a secret](attack-vectors.md#secrets-and-keys-the-vm-may-use)), so rotate it if that matters. Use a fine-grained GitHub token, scoped to the repos your VMs need.

#### devvm secret --json

`devvm secret --json` prints the list as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes. Each of the `secrets` has its name, `saved`, which is `true` when it's saved or, for `claude`, in devvm's environment, and `used_by`, the names of the VMs using it. The JSON never holds a secret's value.

```json
{
  "version": 1,
  "secrets": [
    {
      "name": "claude",
      "saved": true,
      "used_by": [
        "devvm-some-project-71a99b1076d92c"
      ]
    },
    {
      "name": "github",
      "saved": false,
      "used_by": []
    },
    {
      "name": "proxy",
      "saved": false,
      "used_by": []
    }
  ]
}
```

## Logs

`devvm network log` shows the proxy's decisions for this folder's VM, oldest first, one line each. A line has the time, `allowed`, `BLOCKED` or `cache`, and the host and port. An allowed line then has the rule that allowed it (`, decrypted` for an intercepted tunnel), the request (`GET /user`) and the secrets put in (`, token: github`), a blocked line has why it was blocked, and a `cache` line, a request to the [shared Nix cache](#nix), has the request and what came of it:

```text
2026-10-09T10:00:00Z  BLOCKED  example.com:443  not on the allowlist
2026-10-09T10:00:05Z  allowed  github.com:443  rule github.com
2026-10-09T10:00:06Z  allowed  api.github.com:443  GET /user, token: github
2026-10-09T10:00:07Z  cache    cache.nixos.org:443  GET /nix/yv1zf3y6all4j99aad30cb8i55dw9zmp.narinfo: hit
```

| | |
| --- | --- |
| `-n N`, `--lines N` | the last N (default 50) |
| `--follow` | then each new one as it happens, until Ctrl+C |
| `--blocked` | the blocks instead, one row per host and port, by when last blocked: `LAST`, `COUNT`, `HOST` and `ALLOW WITH`, the `devvm network allow` command that allows it |

`ALLOW WITH` is `-` for a request that the proxy refuses whatever the allowlist says, such as a GitHub call that makes a credential, and for a host that isn't a host name or an address. An open VM's log holds what went through its proxy and its links' connections, but not what the VM reached directly, and `devvm network log` says so. The proxy keeps each VM's log to its last 10 to 20 MiB by starting a new file once the current one reaches 10 MiB and keeping one older file, which `devvm network log` reads too. `devvm rm` deletes the VM's logs, and [How it works](../internals/network.md#log-files) describes the files.

### devvm network log --json

`devvm network log --json` prints the last N events as one JSON object, as [output for scripts](../using/command-line.md#output-for-scripts) describes, and devvm refuses it with `--follow`. Each of the `events` has what the proxy logged, without the VM's name. That's the `time`, the `event`, the `host` and the `port`, and, when the event has them, the request's `method` and `path`, the `rule` that allowed it, the `reason` it was blocked or what came of a cache request, the request's `User-Agent` header as `agent`, the `secrets` put in, `refused` for a request refused whatever the allowlist says, the company proxy it went through as `upstream`, and the VM that a link's connection went to as `target`. The `event` is `tunnel` for an allowed connection, `intercept` for an allowed tunnel that the proxy decrypted, `forward` for an allowed request, `blocked`, or `nix-cache` for a request to the shared Nix cache. A link's connection is `link` when devvm carried it, and `link_refused`, `link_target_stopped` or `link_limit` when devvm refused it, with the link's name as its `host`. A log with nothing in it gives `"events": []`.

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

With `--blocked`, each of the `blocked` has the `last` time it was blocked, the `count`, the `host` and the `port`, and `allow`, the rule that `devvm network allow` takes to allow it. A block that `ALLOW WITH` shows as `-` has no `allow`.

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

- **A client links only to a target on its own tier or a stricter one.** An allowlist client links to allowlist and none targets, but not to internet or open ones, because a link to a target on a lower tier would let the client reach more through the target, around its own proxy. devvm checks this when you add a link, at each launch of the client and on every connection, and for a client on a confined tier, on every connection the target's tier must be in force as it runs, so devvm refuses the connection while the target shows as `unenforced`, and it warns you of such a target when you add the link, when you publish a port in its folder that the client links to, and when the client starts. While such a link exists, devvm refuses to give the target a tier lower than the client's, such as `devvm network open`, and it refuses to give the client a tier higher than a target's, such as `devvm network allowlist` for a client that links to an open target. An open client may link to any target, and it then has the target's secrets and project folder within reach as well as its own network.
- **The target publishes.** A link reaches only a port that the target's folder publishes with `devvm link publish`, and only on the target's own loopback. Publishing offers the port to any of your VMs, and each link is still added in the client's folder, so a VM reaches another's service only when you've run a command in both folders.

The first rule keeps a client's tier whole, but it doesn't keep the target's data in the target. A client on a confined tier can read whatever the target's service gives it and send that out through the hosts its own tier reaches. Link two VMs only when you'd trust each with what the other has.

A [group](../using/links.md#groups) links each of its VMs to the ports that the others publish, once you've run `devvm group join` in each VM's folder. Its VMs are all on one tier, so every link it makes keeps the first rule, and devvm refuses to change a VM's tier with `devvm network` while it's in a group. A group joins the trust of all its VMs, so put VMs in one only when you'd trust each with what the others have.

## Not covered

The tiers leave some things open, such as what an allowed host accepts, whatever your secrets allow, such as pushes and API calls, and what a company CA that you add can read. [Attack vectors](attack-vectors.md) lists them under [Narrowed, not closed](attack-vectors.md#network) and [Left to you](attack-vectors.md#the-network).
