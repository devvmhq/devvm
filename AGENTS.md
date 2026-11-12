# AGENTS.md

devvm: one isolated VM per project folder, cloned from a base image, with toolchains and agents shared read-only between VMs. It's a Go CLI that runs its VMs on Lima, behind a backend interface another backend could implement. Read [README.md](README.md) for what it is, and the [docs site](docs/content/docs/) for how each part works; [Contributing](docs/content/docs/contributing/_index.md) covers building, the tools, testscript and writing docs.

## Layout

Each package's concerns and identifiers: [The packages](docs/content/docs/contributing/structure.md#the-packages).

| Path | What |
| --- | --- |
| `cmd/devvm/` | the entry point (`ui.Main`); the only `os.Exit` |
| `internal/cli/` | the command line: one file per Cobra command, the run's services (`app.go`), argument checks, help, `--json`, the CLI tests, `docgen/`; `doc.go` maps it |
| `internal/vm/` | what each command does to a folder's VM, below the command line; `doc.go` maps the package and its owners |
| `internal/shell/`, `internal/vmname/` | quoting for a POSIX shell; a project VM's name grammar |
| `internal/project/`, `internal/gitprotect/`, `internal/hostsafety/`, `internal/hostgit/` | a project folder; `.git` protection; the folder's other host checks; every host git command devvm runs |
| `internal/sshconfig/`, `internal/sshagent/` | the ssh config for running VMs by name; each VM's SSH agent and devvm's own |
| `internal/build/`, `internal/image/` | building images and their records; image keys and names, and the shared tools' registry (`SharedTools`) |
| `internal/fetch/` | every HTTP client |
| `internal/upgrade/` | devvm upgrade; `upgradetest/` (tests only) |
| `internal/from/` | `--from`'s references and digests |
| `internal/redact/` | passwords out of logs |
| `internal/safefile/` | every read of a file a VM can write or swap |
| `internal/devcontainer/`, `internal/jsonc/` | a project's devcontainer.json; JSON with comments |
| `internal/tools/` | the shared tools: the lock, host mise, the daily check, tool image builds, the share |
| `internal/netpolicy/`, `internal/secrets/` | network policy; tokens on the host, the VM's bindings and your own secrets |
| `internal/trust/` | your CAs, and each VM's own CA |
| `internal/agents/` | the coding agents' registry: one `Def` each, which every list of the agents derives from |
| `internal/agentlogin/` | agents' login and settings files on the host, and their placeholders |
| `internal/proxy/` | the proxy, run as `devvm _proxy`; see its `README.md` |
| `internal/daemon/` | devvm's background processes; the only place a process is signalled by pid |
| `internal/desktop/` | devvm desktop's relay on the host |
| `internal/backend/` | `Backend`, its roles, `Caps`, the registry and its guard; `backendtest/`, `registry/`, `lima/` |
| `internal/vmssh/` | devvm's own ssh to a VM (`Checked`), the port forwarder (`Forwards`), each start's host keys |
| `internal/ui/` | the only package that prints |
| `internal/msg/`, `locales/` | messages, `Reporter` and `Question` |
| `internal/ports/` | a VM's ports and the one allocator of host ports |
| `internal/paths/`, `internal/state/` | every folder and file devvm uses; `vm.json`, the layout version, files written whole |
| `internal/settings/` | the defaults: `config.toml` over `settings.json`, each value's check |
| `internal/platform/`, `internal/ubuntu/`, `internal/testbuild/` | build-tagged OS code; Ubuntu's cloud image (never downloads); whether this is a test build |
| `assets/` | the files devvm ships, embedded (`assets.FS`) |
| `internal/structure/` | the structure test |
| `internal/stubtest/` | the test stubs |
| `testdata/` | CLI tests, golden fixtures, coverage baselines, each release's frozen state (`v<release>/`), `lint/` |
| `tests/e2e/`, `tests/e2e-mise.lock` | end-to-end tests on real VMs |
| `scripts/`, `packaging/`, `.goreleaser.yaml`, `VERSION`, `CHANGELOG.md` | pins, releases, the test tasks' scripts, the Homebrew formula, devvm upgrade's key |
| `install.sh` | `curl \| bash`, asking before doctor's fixes |
| `docs/` | the docs site (Hugo + Hextra); `content/docs/cli/` is generated, never edit it |
| `mise.toml`, `.golangci.yml`, `.vale*`, `.markdownlint-cli2.yaml`, `.github/` | tools, tasks, linters, CI and Pages |
| `.agents/skills/security-audit/` | the security-audit skill, for backend reviews |
| `.agents/skills/cli/` | the cli skill: devvm's command line rules, the checklist for adding a command, the tests behind each rule, `judgment.md` for the rest |
| `.agents/skills/docs-voice/` | the docs-voice skill: the voice of the prose and messages, terms, tropes, `scan.sh` and `facts.sh` |

## Tasks

`mise run <task>`: `fmt`, `lint` (`lint:code`, `lint:docs`), `test`, `coverage` (commit the baselines it raised with the change), `vuln`, `deadcode` (anything but the tests' helpers gets a caller or goes), `cli-docs` (after changing a command's help, flags or docs page; commit the pages), `docs`, `e2e`: [Tools and tasks](docs/content/docs/contributing/_index.md#tools-and-tasks).

## Rules

- **The gate:** `mise run fmt && mise run lint && mise run test`, all passing, before starting a task and before marking it done; a gate failing at the start gets fixed first. Anything touching VMs also passes `mise run e2e` in the background: the sections for what changed (`--only`).
- **Nothing in project folders:** state goes in `~/.local/share/devvm` and `~/.config/devvm`.
- **devvm's own files never get a shared copy on disk:** the proxy reads them from the binary, base.yaml and scripts go on stdin, `--from`'s build writes its scripts into its own temp folder.
- **Credentials never go into images.** Host agent history never goes into VMs.
- **Never auto-delete project VMs:** they may hold unsaved work. Warn instead.
- **Every release's state keeps working** until 1.0: a change to state runs its new commands on every frozen `testdata/v<release>/` fixture (never edited or regenerated) in the same commit, and a change for existing state gets a Breaking changelog entry ([Released state](docs/content/docs/contributing/structure.md#released-state)).
- **The startup check only reports.** Never install host software without asking.
- **Image keys hash everything that goes into an image**, the backend included; a tool image's key is in [Building tools](docs/content/docs/internals/images.md#building-tools).
- **Pin every download** in `assets/`'s `layers/`, `features/`, `shared-tools/build.sh`, `oci/build.sh`: `<tool>_version` and `<tool>_sha256_arm64`/`_amd64` at the top, picked with the prelude's `devvm_sha`, fetched with `devvm-fetch` (`oci/build.sh`: `sha256sum`). No `curl | sh`, no "latest". Upstream-checked exceptions: shared tools (`MISE_LOCKED=1`), Ubuntu's signed `SHA256SUMS`, Lima in `install.sh`. Go checks a download's SHA-256 before use.
- **Shared tools are read-only in VMs:** whatever writes into a tool's folder (global installs, self-updates, `corepack enable`, `rustup component add`) is redirected (`~/.npm-global`, `go_set_gobin = false`), done in `assets/shared-tools/build.sh`, or off.
- **In-VM scripts are bash 3.2-safe and shellcheck-clean** (`assets/`, `install.sh`): guard empty arrays (`${a[@]+"${a[@]}"}`), no associative arrays. Everything else is Go.
- **Docs:** an easy-to-follow guide in full, joined sentences, every fact kept, written with the docs-voice skill; every behavior change updates its page in the same task, and the README stays high level ([Writing docs](docs/content/docs/contributing/docs.md#writing-docs)).
- **Adding or changing a command** follows the cli skill (`.agents/skills/cli/`): its checklist, then its review.
- **Adding or changing a shared tool** updates its page and follows Contributing's checklist: `using/toolchains.md` for a toolchain ([Adding a toolchain](docs/content/docs/contributing/extending.md#adding-a-toolchain)), `using/coding-agents.md`'s Supported agents for an agent ([Adding a coding agent](docs/content/docs/contributing/extending.md#adding-a-coding-agent)).
- **Docs by reader:** user pages name no repo paths, Go identifiers or message names; How it works explains mechanisms; Contributing has the paths and identifiers ([Writing docs](docs/content/docs/contributing/docs.md#writing-docs)).
- **User pages lead with doing:** in `getting-started/`, `using/`, `safety/`, `backends/` and `compared/`, a task shows its command, and prose never runs to back-to-back paragraphs; one explanatory paragraph is fine, and longer mechanism moves to How it works, linked, every fact kept.
- **Extend the owner, don't copy it:** before adding a helper, parser, path, file reader, message or table, find its owner and extend it; a new owner gets a forbidigo rule or a structure-test check only when a second copy would be a bug ([Contributing](docs/content/docs/contributing/structure.md#one-owner-for-each-concern)).
- **Edit files with the editor tools,** not `sed`, `awk`, `python`, heredocs or `cat >`.
- **Ask first** before anything outward-facing (publishing, releases, repos, pushing) and before deleting anything you didn't create.
- **A task that turns out wrong or incomplete:** say so and propose the change; don't quietly do something else.

## Go conventions

- **Only `internal/ui` prints, functions take their caller's `ctx`, and every `//nolint` names its linters and says why;** functions stay within revive's limits ([Go lint rules](docs/content/docs/contributing/_index.md#go-lint-rules)).
- **Fail by returning:** `RunE` returns the error and `execute` alone prints it; signals, exit codes and the run log: [CLI conventions](docs/content/docs/contributing/cli.md#cli-conventions).
- **Only `cmd/devvm` and `internal/cli` import `internal/ui`;** other packages print and ask through the one `msg.Reporter` their constructors take ([Messages](docs/content/docs/contributing/cli.md#messages)).
- **Arguments and flag values are checked before anything runs**, with `args.go`'s checks, never Cobra's; a usage error exits 2 ([Command arguments](docs/content/docs/contributing/cli.md#command-arguments)).
- **Cleanups:** whatever a failure would leave (a build VM, a temp file) registers with the UI's `AddCleanup` and is removed once done; one only for interrupts checks `Interrupted()` and stays once interrupted, as the cleanups run after the command returns. Cleanups get a fresh context.
- **External commands run with the UI's `LoggedCmd(ctx, step, cmd)`**, a failure after one returned as `backend.Logged(m)` ([CLI conventions](docs/content/docs/contributing/cli.md#cli-conventions)).
- **OS-specific code goes behind build tags** in `internal/platform` (`*_darwin.go`, `*_linux.go`, `*_other.go`).
- **Dependencies:** the depguard allowlist (cobra, pflag, cobra/doc, go-i18n, BurntSushi/toml, x/text, x/term, x/sys, x/crypto's `blake2b` only (minisign's prehash); go-internal in tests; cobra/doc's own deps never imported directly). `internal/proxy`, `internal/daemon` and `internal/safefile` are stdlib only. Adding one: ask.
- **External tools are commands, never linked** (`limactl`, `docker`, `security`, `git`, `chflags`…); prefer the standard library to shelling out. Strings into a shell are quoted with `shell.Quote`.
- **Test seams are fields** tests set (`Host`, `Exec`, `Runner`, `OS`, `Reporter`…), never package vars (reassign: no package sets another's); `internal/proxy`'s are its `server`'s, which each test makes.
- **The structure test** holds types to 24 methods, bans `func init`, limits package vars, checks the layers and keeps each concern in its owner (its failure names the owner: extend that); read [The structure test](docs/content/docs/contributing/structure.md#the-structure-test) before raising a limit or listing an exception, and regenerate [the checks on owners](docs/content/docs/contributing/structure.md#the-checks-on-owners) after changing its tables (`go test ./internal/structure -run TestOwnersDoc -update`).
- **The CLI reference is generated:** `TestReferenceIsCurrent` fails on a stale one (`mise run cli-docs`).
- `CGO_ENABLED=0`. `internal/proxy` and `internal/daemon` are unix-only for now (`syscall.Kill`, `Setsid`, `Flock`).

## Backends

- **A backend implements `backend.Backend`'s four roles, declares `Caps` for every feature and returns errors, never ending the run:** [How a backend plugs in](docs/content/docs/contributing/extending.md#how-a-backend-plugs-in); a new `Feature` goes in `backend.Features` and [the feature table](docs/content/docs/internals/backends.md#the-feature-table).
- **A missing feature fails, naming the VM and what to do; never fall back silently** ([Missing and unreviewed features](docs/content/docs/internals/backends.md#missing-and-unreviewed-features)).
- **Every emulation is refused until reviewed:** a design note in `docs/content/docs/internals/design.md`, a threat check with the security-audit skill, and e2e checks proving the guarantee; then it's marked reviewed in `Caps`.
- Backend-specific code stays in its package; what isn't (`.git` protection, the proxy) stays out.
- **Lima edits only what differs:** any `limactl edit` gives the VM new ssh host keys at its next boot, so `Start` compares first and makes one edit ([What devvm sets on a VM](docs/content/docs/internals/lima.md#what-devvm-sets-on-a-vm)).

## Messages

- **Every message lives in `locales/en.toml`**, called through its generated function (`go generate ./internal/msg` after editing), with its doc link in `internal/msg/docs.go`; prose is never built with `fmt` ([Messages](docs/content/docs/contributing/cli.md#messages)).
- **Every question declares what answers it** (`AnsweredBy`, `RefusedWith`), and callers never check for a terminal ([Questions](docs/content/docs/contributing/cli.md#questions)).
- **Messages use the docs voice, only shorter:** the docs-voice skill's [Messages](.agents/skills/docs-voice/SKILL.md#messages) and Terms rules; the mechanism goes in the doc link.
- **Tests check which message and with what, never its wording** ([Names mode](docs/content/docs/contributing/cli.md#names-mode)).

## Testing

- **New logic needs a test:** Go table tests next to the code, and CLI tests (testscript against the built binary and the stubs); neither touches the Keychain, real VMs or your folders ([Tests](docs/content/docs/contributing/tests.md)).
- **Never touch VMs, images or containers devvm's tests didn't make:** your own stay as they are. Manual runs: `DEVVM_LIMA_HOME=~/.lt LIMA_HOME=~/.lt XDG_DATA_HOME=/tmp/x/data XDG_CONFIG_HOME=/tmp/x/config DEVVM_SECRET_SERVICE=devvm-sandbox-test` (devvm's Lima home is `$XDG_DATA_HOME/devvm/lima` unless `DEVVM_LIMA_HOME`; `LIMA_HOME` is for your `limactl`). Add a `config.toml` with `features = []` and pass `--network TIER` so nothing prompts. Delete test Keychain items. Never use your own tokens or logins: a network-policy VM reads its agent's login file from `HOME`, so give it its own (the Keychain needs the real `HOME`: test it apart).
- **End-to-end:** `mise run e2e -- --only SECTIONS` in the background for what changed, `--from` before merging `assets/` or `--from` changes ([End-to-end tests](docs/content/docs/contributing/tests.md#end-to-end-tests)).
- **Linux hosts:** `LIMA_HOME=~/.lt-linux-go`, VM `linuxhost`, set up as [Linux hosts](docs/content/docs/contributing/testing.md#linux-hosts) says, apart from any Lima home of your own.
- **Unreviewed emulations** are checked through `DEVVM_UNREVIEWED=backend:feature`, in test builds only ([Unreviewed emulations](docs/content/docs/contributing/testing.md#unreviewed-emulations)).
- **Delete test VMs and temp files afterwards.** Long builds run in the background. Tests that `chflags` real files unlock them (`chflags -R nouchg`), only in their own temp folders.
- **The testscript runner stops the sandbox's proxies** (`pkill -f` on `_proxy -config` in the sandbox): the stub hostagent is gone before they look, so they'd wait 15 minutes. It also stops the sandbox's port forwarders, link sessions and stub ssh masters.
- **`limactl shell` reads stdin:** give it none unless the input is meant for it.
- **Boot problems:** read `<devvm's Lima home>/<vm>/serialv.log` before restarting; every start overwrites it.

## Gotchas learned

- **Lima `vz`:** raw disks only; clones are APFS copy-on-write (`du` overcounts). Direct kernel boot needs an uncompressed arm64 `Image`. Stopping sends a GPIO power key, so guests need `gpio_keys`, or Lima hard-kills them after 30 s. Lima can't shrink disks. virtiofs tags are `lima-<hash>`. The project mount is `/.vol/<dev>/<ino>` at its path; vz serves a share by the path it had at the start, so a rename while it runs gives "Operation not permitted". After root touches a directory on a virtiofs mount, the guest sees it as root's for a few seconds (git: "dubious ownership"). `mounts: []` doesn't remove the home mount from `base: template:…`: pass `--mount-none` (or `--mount-only`) to `limactl create`/`clone`.
- **`limactl list --json` fills in the config:** mounts get `mountPoint`, port forwards a `guestIP` (`127.0.0.1`) and host side; compare only what devvm set. `--set` applies after `--mount-none`. `limactl create` downloads the image at once (no dry run).
- **`limactl shell` quotes each argument for the guest's shell**, so `Exec` passes `sh -c` and one `shell.QuoteJoin` string. QEMU's guestfwd command goes through `/bin/sh -c` too, commas doubled.
- **The ssh socket path limit (103 characters at most: macOS's 104 include the NUL)** bounds VM names (`NameMax`).
- **Linux hosts (QEMU):** mounts are 9p, and a 9p share mounts once per guest. Debian's tmpfs `/tmp` hides anything mounted below it during boot. Project VMs' qcow2 disks are overlays of their image's, so never start an image VM or write its disk after it's built.
- **Shared tools:** mise rewrites `latest`/`24`/`lts` links in a tool's folder after any install, so those folders are local in the VM; only version folders are mounted.
- **Lima can report a failed start for a VM that is running but degraded;** starting tolerates it.
- **Linux's `pgrep -f` matches the shell running it** (procps), so e2e's patterns are written `[_]proxy`. mise skips minisign on linux/arm64 (no build).

## Commits and tasks

- **One commit when each task is complete**, its docs and review included. Conventional Commits: `type(scope): summary`, types `feat`, `fix`, `docs`, `test`, `refactor`, `build`, `ci`, `chore`.
- **Don't commit until the gate passes**, with whatever `mise run fmt` changed in the commit.
- **Task IDs from any tracker never go in commit messages, code comments or docs.**
- **AGENTS.md stays short:** one line per path or rule. Inventories (functions, messages, test names) go in the commit message, not here.
