# Changelog

What's changed on `main` since the latest release is in [Unreleased](#unreleased), at the bottom of this page.

## 0.2.0-rc.1

This is a release candidate for 0.2.0, and it adds to 0.1.1-rc.3. 0.2.0 takes 0.1.1's place, because its breaking changes need a new minor version. Install it with `devvm upgrade --to 0.2.0-rc.1`, the install script's `--version 0.2.0-rc.1`, or `brew install devvmhq/tap/devvm-rc`. Homebrew's `devvm` stays on 0.1.0.

Each change below links its section on [the 0.2.0 page](https://next.devvm.dev/docs/releases/0.2.0/), which has its full story, and the commit that made it.

### Highlights

- Claude Code's plugins, MCP servers' logins, claude.ai connectors and `--setup-token` login all work in a VM ([details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-a-vm)).
- The bugs you reported are fixed, among them 24-bit color, folder names such as `x--y`, and a folder's VM after your Mac restarts ([details](https://next.devvm.dev/docs/releases/0.2.0/#fixes)).
- Your secrets are safer from VMs: the part of each VM's proxy that reads its traffic holds only that VM's secrets, in a sandbox ([details](https://next.devvm.dev/docs/releases/0.2.0/#each-vms-proxy-runs-as-two-processes)).

### What to do

1. Run `devvm update` in each existing VM's folder, and give the first one several minutes, because it builds base and its shared tools again ([details](https://next.devvm.dev/docs/releases/0.2.0/#pins), [c9005ae](https://github.com/devvmhq/devvm/commit/c9005ae4be13f6ebe337111bdba980c3f595c7fe)).
2. Run `devvm restart`, or `devvm stop`, in the folder of each VM that was running when you upgraded, so it gets the new ssh port, proxy and SSH agent relay, and start Claude Code there in a new shell ([too many VMs at once](https://next.devvm.dev/docs/using/troubleshooting/#too-many-vms-at-once), [the proxy runs without a sandbox](https://next.devvm.dev/docs/using/troubleshooting/#the-proxy-runs-without-a-sandbox), [agent forwarding](https://next.devvm.dev/docs/using/ssh/#agent-forwarding), [Claude Code's plugins](https://next.devvm.dev/docs/using/coding-agents/#claude-codes-plugins), [details](https://next.devvm.dev/docs/releases/0.2.0/#at-most-17-vms-run-at-once), [details](https://next.devvm.dev/docs/releases/0.2.0/#each-vms-proxy-runs-as-two-processes), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-vm-and-its-ssh-agent), [details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-the-agent-store), [details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-a-vm), [e4ea123](https://github.com/devvmhq/devvm/commit/e4ea123927cc90022b5333ab15e38f75c69c876a), [aeea313](https://github.com/devvmhq/devvm/commit/aeea3135b27e0de898a449adb63b7742126b8ddb), [19f9900](https://github.com/devvmhq/devvm/commit/19f9900b8633108cf69b28ee7a253b08bafff70f), [7cc0dd7](https://github.com/devvmhq/devvm/commit/7cc0dd76edc858fea9afc90d71cbba8ac6f50f37), [e97db05](https://github.com/devvmhq/devvm/commit/e97db057fd93873a14b075e7d46615dc2d020ac4)).
3. Stop one of your running VMs before you start an 18th, because at most 17 VMs run at once, and a start past 17 fails and names the VMs that are running ([too many VMs at once](https://next.devvm.dev/docs/using/troubleshooting/#too-many-vms-at-once), [details](https://next.devvm.dev/docs/releases/0.2.0/#at-most-17-vms-run-at-once), [e4ea123](https://github.com/devvmhq/devvm/commit/e4ea123927cc90022b5333ab15e38f75c69c876a)).
4. Use a host's full name, such as `jenkins.corp.example.com`, where a tool in a VM reached it by a short name such as `jenkins`, because the proxy no longer adds this machine's DNS search domain ([a name that doesn't resolve](https://next.devvm.dev/docs/using/troubleshooting/#a-name-that-doesnt-resolve), [details](https://next.devvm.dev/docs/releases/0.2.0/#names-are-looked-up-as-theyre-written), [c872cfe](https://github.com/devvmhq/devvm/commit/c872cfe66c349533820c885b2ed11b5db617fc24)).
5. Disconnect the claude.ai connectors you don't want VMs to use, or deny `mcp-proxy.anthropic.com` with `devvm network deny`, because with your subscription's login, code in a VM can now act through your connectors ([details](https://next.devvm.dev/docs/releases/0.2.0/#your-claudeai-connectors), [#61](https://github.com/devvmhq/devvm/issues/61), [c1db911](https://github.com/devvmhq/devvm/commit/c1db911d1ba3856299bd59b1c4d74620b7d49408)).
6. If devvm 0.1.0 imported your Claude Code plugins, sign in again to any plugin that keeps a login of its own, because every VM with Claude Code could read that login ([Claude Code's plugins](https://next.devvm.dev/docs/using/coding-agents/#claude-codes-plugins), [details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-the-agent-store), [details](https://next.devvm.dev/docs/releases/0.2.0/#your-plugins-data-stays-on-this-machine), [7cc0dd7](https://github.com/devvmhq/devvm/commit/7cc0dd76edc858fea9afc90d71cbba8ac6f50f37), [e97db05](https://github.com/devvmhq/devvm/commit/e97db057fd93873a14b075e7d46615dc2d020ac4)).
7. If a folder at a VM's path is now on another volume, such as a copy of the disk or, on Linux, a rolled-back Btrfs snapshot, run `devvm adopt` there to give it that VM ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-folder-on-another-volume), [4ccdbf8](https://github.com/devvmhq/devvm/commit/4ccdbf8125fe4a655980e77a7454c209e98ada70)).
8. If a folder in devvm's agent store is a link, make it a folder of its own, because `devvm agent import` and `devvm agent save` refuse it, and a VM with Claude Code refuses a linked plugins folder ([devvm's agent store](https://next.devvm.dev/docs/using/coding-agents/#devvms-agent-store), [Claude Code's plugins](https://next.devvm.dev/docs/using/coding-agents/#claude-codes-plugins), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-linked-agent-store-is-refused), [details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-the-agent-store), [d939d1d](https://github.com/devvmhq/devvm/commit/d939d1d870cc0216b5a3036636b13489a869b71e), [7cc0dd7](https://github.com/devvmhq/devvm/commit/7cc0dd76edc858fea9afc90d71cbba8ac6f50f37)).

### Fixed

- `devvm agent login claude --setup-token` now shows Claude Code's address and the prompt for its code, and Ctrl-C ends an agent's login ([details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-setup-token-login), [details](https://next.devvm.dev/docs/releases/0.2.0/#ctrl-c-in-an-agents-login), [#57](https://github.com/devvmhq/devvm/issues/57), [6965efc](https://github.com/devvmhq/devvm/commit/6965efcc0a97f7f6334f00d0530007ecf459972c)).
- Programs in a VM, such as coding agents, now get your terminal's 24-bit color in devvm's shell, `devvm agent login` and `ssh` to a VM by name ([details](https://next.devvm.dev/docs/releases/0.2.0/#24-bit-color-in-a-vm), [#54](https://github.com/devvmhq/devvm/issues/54), [947decb](https://github.com/devvmhq/devvm/commit/947decb301c609cb8eba59a92fa6fa2c373945a4)).
- A login to an MCP server that you make in a VM now finishes and stays in that VM ([details](https://next.devvm.dev/docs/releases/0.2.0/#mcp-servers-logins), [#62](https://github.com/devvmhq/devvm/issues/62), [ea3037b](https://github.com/devvmhq/devvm/commit/ea3037bc157903525be51c83aedfd72c37da3cbb)).
- A plugin in a VM now keeps its data, and you can install plugins and update marketplaces there ([details](https://next.devvm.dev/docs/releases/0.2.0/#claude-codes-plugins-in-a-vm), [#56](https://github.com/devvmhq/devvm/issues/56), [e97db05](https://github.com/devvmhq/devvm/commit/e97db057fd93873a14b075e7d46615dc2d020ac4)).
- A project folder keeps its VM after your Mac restarts, though a folder that moved before its VM started again needs `devvm adopt VM` ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-project-folder-after-your-mac-restarts), [#60](https://github.com/devvmhq/devvm/issues/60), [2c0a894](https://github.com/devvmhq/devvm/commit/2c0a894a1cb7347f3d6ebbcb3c39390d07ab4ed8)).
- A plugin, marketplace or MCP server with a name such as `secret-scanner` no longer gets its settings redacted in a VM, so Claude Code stops refusing the VM's `settings.json` from the VM's next launch. Thanks to @jspiro for the report and @dhruv1220 for the tests' idea in [#58](https://github.com/devvmhq/devvm/pull/58) ([details](https://next.devvm.dev/docs/releases/0.2.0/#names-you-choose-in-an-agents-settings), [#55](https://github.com/devvmhq/devvm/issues/55), [f384829](https://github.com/devvmhq/devvm/commit/f3848294d8e1095f115ae13e224697484b38c47c)).
- A folder whose name has separators in a row, such as `x--y`, `foo (copy)` or `C++ stuff`, now gets a VM when you run devvm there again. Thanks to @se6a for the report ([details](https://next.devvm.dev/docs/releases/0.2.0/#folder-names-with-separators-in-a-row), [#52](https://github.com/devvmhq/devvm/issues/52), [ba5613e](https://github.com/devvmhq/devvm/commit/ba5613e1ed9bc1c2592f1fa1af81a6e89f97f513)).
- A warning now prints above the status line, so the status line no longer erases a question's prompt or what you typed ([details](https://next.devvm.dev/docs/releases/0.2.0/#warnings-and-the-status-line), [2b176c9](https://github.com/devvmhq/devvm/commit/2b176c92b3a4e44f83df5a94d8abc710ff5e8097)).
- A plain `ws://` WebSocket now connects through the proxy, where it used to get `502` ([details](https://next.devvm.dev/docs/releases/0.2.0/#plain-websockets), [f37c70a](https://github.com/devvmhq/devvm/commit/f37c70a5cd9ea6dcb3f86ffcb6346bba0ee65136)).

<details><summary>If you use Claude Code and devvm's agent store</summary>

- devvm now warns when `devvm agent import` or `devvm agent save` copies a marketplace holding a credential, when a project folder is inside or holds an agent's folder, and when an import leaves out a link ([details](https://next.devvm.dev/docs/releases/0.2.0/#credentials-in-a-copied-marketplace), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-project-in-an-agents-folder), [details](https://next.devvm.dev/docs/releases/0.2.0/#links-inside-an-agents-folder), [c9d0fdb](https://github.com/devvmhq/devvm/commit/c9d0fdb270c3b348291d64333e12e4cef7856f07), [c901b92](https://github.com/devvmhq/devvm/commit/c901b920efb08d6fdf2edbaa7cfd15b1941f83ab), [d939d1d](https://github.com/devvmhq/devvm/commit/d939d1d870cc0216b5a3036636b13489a869b71e)).
- A failed `devvm agent import` or `devvm agent save` now leaves the agent store as it was, and two of these commands at once, or a save and a VM's new Claude Code login, take turns ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-failed-import-changes-nothing), [details](https://next.devvm.dev/docs/releases/0.2.0/#two-devvms-at-the-agent-store), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-failed-save-changes-nothing), [details](https://next.devvm.dev/docs/releases/0.2.0/#devvm-agent-save-and-a-new-claude-code-login), [3101c85](https://github.com/devvmhq/devvm/commit/3101c85260b2b5f872c0e85c0287d2d08e13bbe1), [b04a1a9](https://github.com/devvmhq/devvm/commit/b04a1a9f1217cbf8069738b42d2072d4d84c53bd), [f80fd2d](https://github.com/devvmhq/devvm/commit/f80fd2df6031e1bb3ac86e5ec7d410cac0551104)).
- On a Mac, `devvm agent save` and `devvm agent import` no longer delete a store file whose name differs from yours only in case, such as `Foo.md` ([details](https://next.devvm.dev/docs/releases/0.2.0/#names-that-differ-only-in-case), [888078c](https://github.com/devvmhq/devvm/commit/888078c9cd830a68b04665a5d3217d041624aefb)).
- `devvm agent import` fails on an agent's folder past 20,000 entries, 128 MiB or 16 MiB in one file, and `devvm agent save` saves nothing past folders 128 deep or paths of 4,096 bytes ([details](https://next.devvm.dev/docs/releases/0.2.0/#devvm-agent-imports-bounds), [details](https://next.devvm.dev/docs/releases/0.2.0/#how-deep-a-save-goes), [c901b92](https://github.com/devvmhq/devvm/commit/c901b920efb08d6fdf2edbaa7cfd15b1941f83ab), [888078c](https://github.com/devvmhq/devvm/commit/888078c9cd830a68b04665a5d3217d041624aefb)).
- While devvm can't read a VM's state, `devvm agent import` follows no links and warns, so run `devvm doctor` and import again ([details](https://next.devvm.dev/docs/releases/0.2.0/#an-import-while-a-vms-state-cant-be-read), [b04a1a9](https://github.com/devvmhq/devvm/commit/b04a1a9f1217cbf8069738b42d2072d4d84c53bd)).
- `devvm agent save` no longer crashes on an agent's settings file that's a single JSON string ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-settings-file-thats-one-string), [1ecd1fa](https://github.com/devvmhq/devvm/commit/1ecd1faa735c32521c193b1b7ab710a2aea10906)).

</details>
<details><summary>If you use Cursor</summary>

- Cursor's CLI in a VM can no longer make some changes to your account, team or cloud agents, such as merging a cloud agent's pull request, so make those from Cursor on your machine ([details](https://next.devvm.dev/docs/releases/0.2.0/#cursors-cli-and-your-cursor-account), [872c2e2](https://github.com/devvmhq/devvm/commit/872c2e23c0c4edc28d690ce13a028d6b8b140d31)).

</details>
<details><summary>If you use a company proxy</summary>

- A launch, a `devvm network` change or a `devvm restart` that devvm refuses, such as for a missing `proxy` secret, now leaves a running VM running ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-refused-launch), [4a4aabe](https://github.com/devvmhq/devvm/commit/4a4aabe9a8dd4d85f08b8dbeaf4b3c0ec9aaf3a5)).
- A `NO_PROXY` entry with an IPv6 zone, or with a port that's 0 or isn't plain decimal, now matches no host ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-no_proxy-address-with-a-zone), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-no_proxy-entrys-port), [#59](https://github.com/devvmhq/devvm/issues/59), [8afdec7](https://github.com/devvmhq/devvm/commit/8afdec7de4bc2fbb5fb02a2f92f7918eae4dd5c4), [a7cc125](https://github.com/devvmhq/devvm/commit/a7cc12521e8d5361a87f179348c3cedff42bac1d)).

</details>
<details><summary>If you use links or forwarded ports</summary>

- A VM that forwards every port leaves out the 17 ports that VMs' ssh uses, so list a server's port on one of them, such as an X server's 6000, with `devvm ports` ([ports](https://next.devvm.dev/docs/using/ports-size/#ports), [details](https://next.devvm.dev/docs/releases/0.2.0/#at-most-17-vms-run-at-once), [e4ea123](https://github.com/devvmhq/devvm/commit/e4ea123927cc90022b5333ab15e38f75c69c876a)).
- `devvm link rm` now ends a link's open connections within seconds, and a stopped port forwarder or link session no longer leaves its ssh running ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-removed-links-connections), [details](https://next.devvm.dev/docs/releases/0.2.0/#stopped-forwarders-and-link-sessions), [c63f9a3](https://github.com/devvmhq/devvm/commit/c63f9a3340c507430108eb9e806b189f09346fdf), [90a64a4](https://github.com/devvmhq/devvm/commit/90a64a44d192dde1b413992925abba61bc41a6e5)).
- A forwarded port's or a link's connection now ends once its client has closed and the VM has sent nothing for 30 seconds, which can cut `nc -N` in front of a slow query ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-closed-connections-ssh-process), [c63f9a3](https://github.com/devvmhq/devvm/commit/c63f9a3340c507430108eb9e806b189f09346fdf)).
- After this machine briefly runs out of open files, forwarded ports keep answering on both `::1` and `127.0.0.1`, and `devvm desktop` keeps running ([details](https://next.devvm.dev/docs/releases/0.2.0/#forwarded-ports-over-ipv6), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-forwarder-after-a-burst), [details](https://next.devvm.dev/docs/releases/0.2.0/#devvm-desktop-after-a-burst), [608ab43](https://github.com/devvmhq/devvm/commit/608ab43714354833cca7fb96f8ab89c53ef7f49d), [f37c70a](https://github.com/devvmhq/devvm/commit/f37c70a5cd9ea6dcb3f86ffcb6346bba0ee65136), [df1d76b](https://github.com/devvmhq/devvm/commit/df1d76bbd9421a62901f424224dc2c64adf24ee6)).

</details>
<details><summary>If you use --clone</summary>

- `devvm clone fetch` and `devvm rm`'s fetch no longer fail with `clone_fetch_failed` on a VM that forwards no ports ([details](https://next.devvm.dev/docs/releases/0.2.0/#host-keys-whatever-a-vms-ports), [c1cf96e](https://github.com/devvmhq/devvm/commit/c1cf96e4b6c757d17519c6796fbb6e49dd75d5f6)).

</details>
<details><summary>If you use SSH agent forwarding</summary>

- `ssh-add` in a VM can still list and use your SSH agent's keys, but it can no longer remove, lock or add them ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-vm-and-its-ssh-agent), [19f9900](https://github.com/devvmhq/devvm/commit/19f9900b8633108cf69b28ee7a253b08bafff70f)).

</details>
<details><summary>If you use a Linux host</summary>

- devvm no longer warns that `.git/config` lost its pin when git replaces the file just as devvm pins it ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-pin-that-meets-gits-rename-on-linux), [90a64a4](https://github.com/devvmhq/devvm/commit/90a64a44d192dde1b413992925abba61bc41a6e5)).
- In a VM confined to its proxy with transparent mode off, a lookup now fails at once from the VM's next launch, instead of waiting, as `wrangler dev` did for about 40 seconds ([details](https://next.devvm.dev/docs/releases/0.2.0/#lookups-with-transparent-mode-off), [63bb1ff](https://github.com/devvmhq/devvm/commit/63bb1ff9a8d9cec29400628acd2efbb75e99b8bf)).
- When a VM's proxy can't get its whole sandbox, such as on a kernel whose Landlock is older than ABI 6, the VM keeps its network and `devvm doctor` warns, naming what's missing ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-child-without-its-whole-sandbox), [941c63c](https://github.com/devvmhq/devvm/commit/941c63c696b2315329028049b21fa70cdb1b2a0d)).

</details>
<details><summary>Security hardening: a VM that tries to attack your machine can now do much less</summary>

- Each VM's proxy now runs as two processes, and the one that reads the VM's network holds only that VM's secrets and can't read your files, reach the Keychain or connect to local services ([details](https://next.devvm.dev/docs/releases/0.2.0/#each-vms-proxy-runs-as-two-processes), [details](https://next.devvm.dev/docs/releases/0.2.0/#the-childs-sandbox-on-macos), [details](https://next.devvm.dev/docs/releases/0.2.0/#the-childs-sandbox-on-linux), [details](https://next.devvm.dev/docs/releases/0.2.0/#the-child-connects-only-through-devvm-_proxy), [aeea313](https://github.com/devvmhq/devvm/commit/aeea3135b27e0de898a449adb63b7742126b8ddb), [941c63c](https://github.com/devvmhq/devvm/commit/941c63c696b2315329028049b21fa70cdb1b2a0d), [1793353](https://github.com/devvmhq/devvm/commit/1793353dd920a48cf4964b788a665c50cc208147), [3da3843](https://github.com/devvmhq/devvm/commit/3da38436a7f598dfe59711b0f64589e966bcb915)).
- On a Mac, devvm refuses a project in its own folders whatever the case of their names, such as `~/.Config/devvm/layers`, so delete a VM already made there with `devvm rm` ([details](https://next.devvm.dev/docs/releases/0.2.0/#devvms-own-folders-in-another-case), [c41ca82](https://github.com/devvmhq/devvm/commit/c41ca82f6e98c2f41ce469de607ae0403c0ab4fc)).
- `devvm history import` refuses an export with a name such as `.claude/./settings.json`, so an export can't put an agent's login or devvm's setup files into a VM ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-history-exports-names), [e97db05](https://github.com/devvmhq/devvm/commit/e97db057fd93873a14b075e7d46615dc2d020ac4)).
- A VM can't fill a Linux host with processes, because devvm carries at most 256 of its connections to the proxy at once ([details](https://next.devvm.dev/docs/releases/0.2.0/#connections-to-the-proxy-on-linux-hosts), [f37c70a](https://github.com/devvmhq/devvm/commit/f37c70a5cd9ea6dcb3f86ffcb6346bba0ee65136)).
- A VM's proxy holds at most 2,048 of its connections, closes one that waits 10 minutes for its next request, and takes at most 64 KiB of headers ([details](https://next.devvm.dev/docs/releases/0.2.0/#what-a-vms-proxy-holds), [f37c70a](https://github.com/devvmhq/devvm/commit/f37c70a5cd9ea6dcb3f86ffcb6346bba0ee65136)).
- A `devvm network` change never leaves a VM running without its proxy ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-vm-never-runs-without-its-proxy), [4a4aabe](https://github.com/devvmhq/devvm/commit/4a4aabe9a8dd4d85f08b8dbeaf4b3c0ec9aaf3a5)).
- While a VM runs, Lima's console log and Lima's own logs for it each stay under 10 MiB, plus one older file of up to 10 MiB, so a VM can't fill your disk ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-vms-logs-and-their-caps), [9c74dac](https://github.com/devvmhq/devvm/commit/9c74dac11ea6100f3a583dd79fe3380b45573fab)).
- While they run, the port forwarder's and link session's logs stay under 1 MiB and the build proxy's under 10 MiB, and the run logs take at most 256 MiB together ([details](https://next.devvm.dev/docs/releases/0.2.0/#devvms-other-logs), [9c74dac](https://github.com/devvmhq/devvm/commit/9c74dac11ea6100f3a583dd79fe3380b45573fab)).
- Lima's console logs no longer keep a VM's proxy password ([details](https://next.devvm.dev/docs/releases/0.2.0/#passwords-in-limas-console-logs), [4d40958](https://github.com/devvmhq/devvm/commit/4d409589bbd0050c29deb4110b35326abcb9e7ba)).
- devvm now reads every host as the proxy does, so `devvm secret set`, `NO_PROXY`, MCP servers' URLs and `--from` no longer take a host with a letter such as the Kelvin sign as plain ASCII ([details](https://next.devvm.dev/docs/releases/0.2.0/#every-host-in-one-form), [4e92549](https://github.com/devvmhq/devvm/commit/4e925492ca22a58aab631675de76d3ca497251d6)).

</details>
<details><summary>Internal changes</summary>

- `devvm doctor` now warns once of each VM whose state devvm can't read, naming the VM, the file and why, and it changes nothing ([details](https://next.devvm.dev/docs/releases/0.2.0/#devvm-doctor-checks-every-vms-state), [5924a37](https://github.com/devvmhq/devvm/commit/5924a373f07a6c550d9696d6332e546559d9650f)).
- mise is 2026.11.10 in base and on this machine, the docker layer has Docker 29.10.0 and compose 5.8.0, and Cursor's CLI is 2026.11.17-f532c45 ([details](https://next.devvm.dev/docs/releases/0.2.0/#pins), [c9005ae](https://github.com/devvmhq/devvm/commit/c9005ae4be13f6ebe337111bdba980c3f595c7fe), [01e837a](https://github.com/devvmhq/devvm/commit/01e837a164a2d751339e07197fe264b3e3804959)).
- In a terminal, a status line now says what devvm waits for during its slow steps and while another devvm changes the same thing ([details](https://next.devvm.dev/docs/releases/0.2.0/#status-lines-for-slow-steps), [details](https://next.devvm.dev/docs/releases/0.2.0/#status-lines-while-devvm-waits), [2b176c9](https://github.com/devvmhq/devvm/commit/2b176c92b3a4e44f83df5a94d8abc710ff5e8097), [cb7177b](https://github.com/devvmhq/devvm/commit/cb7177b32c3f40c38848c8c5740073e245f79eca)).
- When devvm can't read a project folder, it names the folder and why, where it used to say the VM was made for an earlier folder ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-project-folder-devvm-cant-read), [cb7177b](https://github.com/devvmhq/devvm/commit/cb7177b32c3f40c38848c8c5740073e245f79eca)).
- A folder that moved while a restart renumbered its disk keeps its VM, once the VM has recorded its folder's volume at a launch ([a moved or renamed folder](https://next.devvm.dev/docs/using/vms/#a-moved-or-renamed-folder), [details](https://next.devvm.dev/docs/releases/0.2.0/#a-folder-moved-across-a-restart), [ba510ee](https://github.com/devvmhq/devvm/commit/ba510eec1f55a7cd656050eabe14c5b52ba5ed38)).
- The shared Nix cache downloads each package once, even when two requests for it come close together ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-nix-package-downloaded-twice), [90a64a4](https://github.com/devvmhq/devvm/commit/90a64a44d192dde1b413992925abba61bc41a6e5)).
- devvm now refuses a VM's size that isn't a number, naming it, where the VM used to start with the image's size, so set it again with `devvm size` ([details](https://next.devvm.dev/docs/releases/0.2.0/#a-size-that-isnt-a-number), [0b85f5f](https://github.com/devvmhq/devvm/commit/0b85f5f557a8b7a6b83f89533746c7bac3ecb7b2)).

</details>

## 0.1.1-rc.3

A release candidate for 0.1.1, adding to 0.1.1-rc.2. Install it with `devvm upgrade --to 0.1.1-rc.3`, the install script's `--version 0.1.1-rc.3`, or `brew install devvmhq/tap/devvm-rc`. Homebrew's `devvm` stays on 0.1.0.

### Breaking

- **Your layers run after devvm's prelude:** devvm puts one line of its own in place of your layer's `#!` line, which gives the layer `$devvm_arch`, `$devvm_libc` and `devvm_sha`. A layer that sets its own `devvm_arch` or another `devvm_` name keeps its own. In a layer without a `#!` line, the line numbers in bash's errors are one more than the file's ([your own layers](https://next.devvm.dev/docs/using/customizing/#your-own-layers)).

### After you upgrade

- Base, every image built on it and every shared tool image are built again once, because base now has `git-lfs` and each build script starts with devvm's prelude on one line, which keeps the line numbers in a build's errors the script's own.
- Run `devvm update` in each existing VM's folder. Until you do, a VM made from an older image says it's outdated, and every existing VM says there are newer shared tools at the same versions. The first update or new VM builds each of its shared tools again, one at a time, which takes several minutes.
- An existing `--clone` VM gets `git-lfs` only once it's made again, with `devvm clone off`, then `devvm update`, then `devvm clone on`. Until then, its Git LFS files stay pointers.

### New

- **Cursor ([#44](https://github.com/devvmhq/devvm/pull/44), thanks to @barnesoir):** `--agents cursor` gives a VM Cursor's CLI, as `cursor-agent` or `agent`, and `devvm agent login cursor` logs it in from a browser on any machine, with the login kept on this machine ([Cursor CLI login](https://next.devvm.dev/docs/using/coding-agents/#cursor-cli-login)). `devvm cursor` opens the folder's VM in the Cursor editor, as `devvm code` does in VS Code. Recent Cursor runs its Agent in the VM with your Cursor sign-in in memory, so `devvm cursor` warns unless your Cursor settings send Agent's requests through the app on this machine, and you should open only VMs you trust with it ([Cursor's Agent](https://next.devvm.dev/docs/using/ssh/#cursors-agent)).
- **`devvm paste` ([#38](https://github.com/devvmhq/devvm/pull/38), thanks to @DustyPolk):** sends the image, PDF or file you copied on this machine into the running VM and puts its path in the VM on your clipboard, so you can paste it into a coding agent. A copied file goes only after you say yes, and `devvm paste rm` deletes what it sent ([pasting images](https://next.devvm.dev/docs/using/coding-agents/#pasting-images)).
- **`devvm agent import` can follow your dotfiles' links:** a link out of an agent's folder, such as `~/.claude/skills/x` into a dotfiles repo, is copied when you say yes in a terminal, when you pass `--follow-links`, or when it leads into a folder that `config.toml`'s `follow-links` names, such as `["~/dotfiles"]`. The store gets copies, never links, and devvm never follows a link to your home folder, its own folders, places that hold secrets, such as `~/.ssh`, or an agent's logins or history ([links out of an agent's folder](https://next.devvm.dev/docs/using/coding-agents/#links-out-of-an-agents-folder)).
- **`--clone` copies your repo as it is here:** the VM's clone gets every branch, tag and remote-tracking branch, a detached HEAD, what you've staged and what you haven't, a rebase, merge or other operation in progress, and your stash. It also gets your remotes without credentials, the repo's hooks and its own settings, your initialised submodules, each with a clone of its own, and the Git LFS objects your checked-out files point to, as every VM now has `git-lfs`. A partial clone is copied with the objects it has, and the VM's git fetches the rest from its remote. Untracked and ignored files, such as `.env`, and credential helpers stay on this machine, and devvm names anything it leaves out that you'd expect in the VM. Because `origin` is real in the VM, code there that may use your GitHub token can push to it, which a network rule can stop ([what's copied](https://next.devvm.dev/docs/using/clone/#whats-copied), [the settings](https://next.devvm.dev/docs/using/clone/#the-settings), [submodules](https://next.devvm.dev/docs/using/clone/#submodules), [a partial clone](https://next.devvm.dev/docs/using/clone/#a-partial-clone), [Git LFS](https://next.devvm.dev/docs/using/clone/#git-lfs)).
- **All of a `--clone` VM's work comes back:** `devvm clone fetch` also saves commits that only the VM's detached HEAD has as `saved/devvm/HEAD` and the VM's stash entries as `saved/devvm/stash-N`, and it brings back each submodule's branches and the LFS objects the VM's work points to, without `git-lfs` on this machine. `devvm rm` and `devvm clone off` check all of these and ask before any of them goes ([deleting the VM](https://next.devvm.dev/docs/using/clone/#deleting-the-vm)).
- **Pins in your own layers:** a layer of yours can pin a download for each architecture and fetch the right one with `devvm-fetch "$URL" "$(devvm_sha TOOL)" OUT`, as devvm's own scripts do ([the example](https://next.devvm.dev/docs/using/customizing/#example-mailpit)).
- **Quiet hosts:** Cursor CLI's metrics host, `api3.cursor.sh`, and Go's `telemetry.go.dev` stay blocked, but their blocks no longer show before the VM's prompt, in `devvm-why` or in `devvm network lock`. `devvm network log` still lists them, each line ending `, quiet` ([quiet hosts](https://next.devvm.dev/docs/safety/network/#quiet-hosts)).

### Changed

- **`--clone` in a folder of repos or a git worktree** copies each repo as above. A worktree's clone also gets the repo's default branch, remote-tracking branches and tags, and devvm says that the repo's other local branches stay on this machine.
- **`--clone` refuses a repo with a grafts file** before it makes the VM, and says what to do ([requirements](https://next.devvm.dev/docs/using/clone/#requirements)).
- **The proxy counts more addresses as internal:** a 6to4 address, in `2002::/16`, is judged by the IPv4 address in it, as a NAT64 address is, so `2002:a00:1::1` is as private as `10.0.0.1`. A deny for an IPv4 address also covers its 6to4 form, but an allow doesn't, because anyone may run a 6to4 relay. Teredo, IPv6's and IPv4's documentation ranges and other special ranges are refused unless a rule names the address, and `--from`'s registries are judged by the same list ([the proxy](https://next.devvm.dev/docs/internals/network/#the-proxy)).
- **Pins:** base's mise is 2026.11.8, Docker's buildx is 0.39.0 and compose 5.7.1, and Nix is 2.35.6.

### Fixed

- **`--clone` of a shallow or SHA-256 repo ([#40](https://github.com/devvmhq/devvm/issues/40)):** a repo from `git clone --depth 1` or one that uses SHA-256 failed after devvm made the VM. Both work now, and a VM whose copy failed gets its clone the next time you run devvm in its folder. Thanks to @Josuesp1620 for the report and @dhruv1220 for finding the cause in [#42](https://github.com/devvmhq/devvm/pull/42) ([a copy that didn't finish](https://next.devvm.dev/docs/using/clone/#a-copy-that-didnt-finish)).
- **A stopped VM no longer keeps its listed ports from other VMs:** only a running VM holds a port. When the first VM starts again, devvm and `devvm ports` say that the other VM has the port, and the first VM takes it back within about 20 seconds of the other one stopping. Meanwhile, both VMs' apps share that port's site data and service workers in your browser ([two VMs and one port](https://next.devvm.dev/docs/using/ports-size/#two-vms-and-one-port)).
- **`devvm ports` says why a listed port isn't forwarded,** such as the forwarder's limit of 256 ports or Linux refusing a port below 1024, instead of always blaming a program on this machine ([a listed port isn't forwarded](https://next.devvm.dev/docs/using/troubleshooting/#a-listed-port-isnt-forwarded)). When a port's forward ends, so do its connections ([how it reaches the VM](https://next.devvm.dev/docs/internals/ports/#how-it-reaches-the-vm)).
- **A name that doesn't resolve says why:** a block now says whether this machine's DNS has no address for the name or didn't answer in time, and after a failure that may pass, the proxy asks once more before it blocks the name ([log files](https://next.devvm.dev/docs/internals/network/#log-files)).
- **dotnet on the allowlist tier** can reach DigiCert's OCSP responder, `ocsp.digicert.com` on port 80, which NuGet asks when it can't fetch DigiCert's revocation list ([dotnet](https://next.devvm.dev/docs/using/toolchains/#dotnet)).
- **`devvm history import FILE`** takes a `.tar.gz` made with macOS's `tar`, leaving out its AppleDouble `._` files ([importing history](https://next.devvm.dev/docs/using/coding-agents/#importing-history)).
- **opencode's links at `devvm agent import`** are copied or named as the other agents' are, where they were dropped without a word ([importing from this machine](https://next.devvm.dev/docs/using/coding-agents/#importing-from-this-machine)).

## 0.1.1-rc.2

A release candidate for 0.1.1, adding to 0.1.1-rc.1. Install it with `devvm upgrade --to 0.1.1-rc.2`, the install script's `--version 0.1.1-rc.2`, or `brew install devvmhq/tap/devvm-rc`. Homebrew's `devvm` stays on 0.1.0.

### After you upgrade

- Base and every image built on it are built again once, for transparent mode's programs.
- Run `devvm update` in an existing VM's folder to give it transparent mode, which new VMs get by default: its image predates the programs it needs.
- On a Linux host, run `devvm mount-cache none` for each existing VM, to stop the 9p cache bug below. Each launch reminds you until you do.
- If you used `devvm git-protect strict`, use `devvm clone on` for code you don't trust: strict is gone.
- Make new API keys for any VM you don't trust that had keys passed to it as they were.

### New

- **Transparent mode ([#35](https://github.com/devvmhq/devvm/issues/35)), on by default:** on every tier but open, tools that ignore the proxy settings, such as `wrangler dev`, static Go binaries and containers, reach the VM's proxy and its allowlist anyway. A VM made before needs `devvm update` first, and `devvm network transparent off` turns it off for a VM ([what it covers](https://next.devvm.dev/docs/safety/network/#tools-that-ignore-the-proxy)).
- **Folders of repos with `--clone` ([#3](https://github.com/devvmhq/devvm/issues/3)):** one VM gets a clone of each repo you pick, at the same paths. `devvm clone fetch` brings back every repo's work, and `devvm clone add PATH` adds one ([folders of repos](https://next.devvm.dev/docs/using/clone/#folders-of-repos)).
- **Rules by HTTP method and path:** `--method` and `--path` on `devvm network allow`, `deny` and `rm`, such as `devvm network deny api.github.com --method POST,PUT,PATCH,DELETE`.
- **Your own secrets:** `devvm secret set NAME --host HOST --env VARIABLE`, then `devvm secret use NAME`. The VM only ever sees a placeholder, and `--method` and `--path` limit where the real value goes.
- **Learning a VM's hosts ([#17](https://github.com/devvmhq/devvm/issues/17)):** `--network learn`, then `devvm network lock` to keep the hosts you choose.
- **`.git` protection on Linux ([#4](https://github.com/devvmhq/devvm/issues/4), [#20](https://github.com/devvmhq/devvm/pull/20)):** `.git` is locked read-only around QEMU, without root. Where a host can't, such as Ubuntu with AppArmor, devvm watches `.git`, warns at each launch, and names what changed.
- **Nested repos ([#3](https://github.com/devvmhq/devvm/issues/3)):** a VM protects the `.git` of each repo up to 4 folders down. `devvm doctor` suggests `safe.bareRepository explicit`.
- **pi and more providers ([#23](https://github.com/devvmhq/devvm/pull/23), [#31](https://github.com/devvmhq/devvm/pull/31), [#32](https://github.com/devvmhq/devvm/pull/32)):** pi is a fourth agent, and Gemini, Groq, Mistral, xAI and DeepSeek keys stay on this machine.
- **Prereleases on Homebrew ([#19](https://github.com/devvmhq/devvm/pull/19)):** `brew install devvmhq/tap/devvm-rc`.

### Changed

- **The 9p cache on Linux ([#36](https://github.com/devvmhq/devvm/issues/36)):** Lima's default cache could write files to this machine empty or cut short, and made SQLite and Gradle fail. New Linux VMs use `none`, and `devvm mount-cache none` switches existing ones. Under `none`, keep SQLite WAL databases and other files that programs map for writing on the VM's own disk, or use `--clone`. macOS doesn't change.
- **A `--clone` VM in a folder another VM mounts** is refused, and `devvm rm` asks before deleting one made earlier.
- **Logins:** a VM's copy of Codex's and Claude Code's login files keeps only the fields devvm knows. opencode, like pi, gets no key devvm doesn't bind, unless you list the provider in `config.toml`'s `[pass-keys]` ([#30](https://github.com/devvmhq/devvm/pull/30)).
- **Warnings** print once per command.
- **Nix** is 2.35.5.

### Removed

- **`devvm git-protect strict` ([#37](https://github.com/devvmhq/devvm/issues/37)):** it couldn't keep git on this machine out of submodules the VM adds, so it's a usage error now. Strict VMs become `on`, and devvm takes out the `diff.ignoreSubmodules = all` it set, keeping a value of your own ([VMs that were strict](https://next.devvm.dev/docs/safety/git-protection/#vms-that-were-strict)).

### Fixed

- opencode's history copy with SQLite 3.54 ([#26](https://github.com/devvmhq/devvm/pull/26)), a NUL byte in an ssh socket's path ([#25](https://github.com/devvmhq/devvm/pull/25)), a build VM reusing its clone's ssh port, a login saved for an agent the VM lacks ([#27](https://github.com/devvmhq/devvm/pull/27)), and `.git` protection locking a folder that only looks like a git folder ([#28](https://github.com/devvmhq/devvm/pull/28)).

## 0.1.1-rc.1

A release candidate for 0.1.1, for trying the changes below before the release. Install it with `devvm upgrade --to 0.1.1-rc.1`, or with the install script's `--version 0.1.1-rc.1`. Homebrew stays on 0.1.0.

- **A dotnet toolchain:** the .NET SDK is a shared toolchain, chosen by a folder's `*.sln`, `*.slnx`, `*.csproj`, `*.fsproj` or `global.json`, and a `global.json`'s `sdk.version` picks the SDK. devvm downloads Microsoft's archive for each platform and checks its SHA-512, global tools and workloads go to the VM's home folder, and the .NET CLI's telemetry is off ([toolchains](https://next.devvm.dev/docs/using/toolchains/)).
- **A network that inspects TLS:** devvm's own mise, which looks up the shared tools' versions on this machine, now trusts the CAs in `~/.config/devvm/ca` as well as the system's, so resolving works behind a proxy that re-signs HTTPS. When a certificate isn't trusted, devvm says so and points to the docs instead of saying to check the connection. On macOS, the CA also has to be trusted in the Keychain.
- **Release candidates:** devvm can ship prereleases, such as this one. `devvm upgrade --to` installs one, and devvm upgrade, the release check and the install script's default never pick one on their own.
- **Lima 2.2 or newer:** devvm needs Lima 2.2 at least, and it's tested with Lima 2.3. `devvm doctor` says when Lima is older.
- **Updated pins:** base's docker is 29.9.3, mise 2026.11.3, rustup 1.29.3, and tealdeer 1.9.1, with a newer oh-my-bash. The shared tools' backends are named, so mise's registry can't switch opencode to a source with no downloads. base installs ICU, which .NET needs, so base and every image built on it are built again once.

## 0.1.0

Released 2026-11-06.

The first release of devvm, which gives each project folder an isolated VM of its own, ready for you or a coding agent to work in. It runs on macOS 13 or newer and on Linux, on arm64 and x86_64, with Lima 2.0 or newer. It's tested on Apple silicon Macs and on Linux on both arm64 and x86_64, but not yet on Intel Macs.

- **A VM for each project folder:** running `devvm` in a folder makes its VM, or opens the one it has, and gives you a shell in it at the same path as on your machine. The VM mounts only that folder read-write, and with `--clone` it gets a clone of the repo of its own instead, whose work you fetch with `git fetch devvm`.
- **Ready in seconds:** every VM is a copy of a base image that devvm builds once, and the toolchains and coding agents are shared tools that VMs mount read-only. devvm detects node, python, rust and go from the project's files, and the built-in `docker`, `nix` and `desktop` layers, your own layers from `devvm layer new`, a project's `devcontainer.json` and `--from` with a container image change what a VM starts from.
- **Coding agents in the sandbox:** Claude Code, Codex and opencode run in the VM with your settings, skills and MCP servers. devvm keeps their settings and logins in a store of its own on your machine, `devvm agent import` copies them in from an agent you already use, `devvm agent login` makes an agent's login from the VM, and `devvm agent save` brings a VM's settings back to the store after you review the diff.
- **Network tiers and secrets:** every VM goes through a proxy that devvm runs for it, and its tier, which is open, internet, allowlist or none, decides what the proxy lets it reach. New VMs get the allowlist tier. On every tier, your secrets, such as your GitHub token and Claude Code token, stay on your machine, because the proxy puts them into the VM's requests.
- **`.git` protection:** on macOS, devvm locks a project's git hooks and config with a flag that even root in the VM can't clear, so code in the VM can't make your next git command on this machine run something.
- **Reaching VMs:** each running VM can be reached by name from ssh and from editors such as VS Code (`devvm code`), with your SSH agent only when you allow it. A VM forwards the ports you ask for, `devvm link` lets one VM reach a service that another VM publishes, and `devvm desktop` opens a VM's desktop in Screen Sharing on macOS, or serves it to a VNC viewer of your own on Linux.
- **Managing VMs and space:** `devvm ls`, `devvm stop`, `devvm restart`, `devvm update` and `devvm rm` manage VMs, and `devvm images`, `devvm gc`, `devvm space` and `devvm retention` show and clean up the images, shared tools and exports devvm keeps. devvm never deletes a project VM on its own, because it may hold unsaved work.
- **Installing and upgrading:** install devvm with Homebrew (`brew trust --formula devvmhq/tap/devvm`, then `brew install devvmhq/tap/devvm`) or with the install script, which checks the release's minisign signature and checksum and asks before it installs what devvm needs, such as Lima. `devvm doctor` checks this machine, and `devvm upgrade` installs a newer release once it has checked the signature.
- **For scripts:** many commands take `--json`, and `--no-input` makes devvm fail where it would ask, naming the flag that answers the question.

## Unreleased

These changes are on `main` and go out in the next release.
