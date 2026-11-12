---
title: Contributing
explainer: Build devvm, run its tests and follow its rules
weight: 100
---

The rules for working in the repo, for people and coding agents alike, are in [AGENTS.md](https://github.com/devvmhq/devvm/blob/main/AGENTS.md).

## Building from source

devvm is built with Go, pinned in `mise.toml`. With [mise](https://mise.jdx.dev) installed:

```sh
mise install
mise exec -- go build -o devvm ./cmd/devvm
./devvm --help
```

The binary has devvm's own files (`assets/`) built in, so it runs from anywhere: link to it from your `PATH`, or copy it there.

```sh
ln -s "$PWD/devvm" ~/.local/bin/devvm
```

After editing anything in `assets/`, build again before a manual run: a binary keeps the files it was built with.

## Tools and tasks

mise installs every tool at the version pinned in `mise.toml`, checked against `mise.lock`:

| Tool | Used for |
| --- | --- |
| go | building and testing |
| golangci-lint | Go linters and formatters (`.golangci.yml`) |
| gotestsum | running the tests |
| govulncheck | checking dependencies for known vulnerabilities |
| goreleaser | release builds ([Maintaining](maintaining.md#releasing)) |
| hugo-extended | the docs site |
| vale, markdownlint-cli2, lychee | linting the docs |
| node | running markdownlint-cli2 |
| shellcheck | linting shell scripts |
| actionlint, zizmor | linting the GitHub workflows, and checking them for security problems |
| minisign | signing releases |

Run the tasks with `mise run <task>`:

| Task | Does |
| --- | --- |
| `fmt` | formats Go code |
| `lint` | runs `lint:code` and `lint:docs` |
| `lint:code` | runs golangci-lint, shellcheck, actionlint and zizmor |
| `lint:docs` | runs the docs linters, the message link check, the command line check and the user pages' prose check, and builds the site, which fails on any warning |
| `test` | runs the tests with the race detector, then checks each package's coverage against its [baseline](tests.md#coverage) |
| `coverage` | runs the tests, then raises each package's coverage baseline to what they reach (`scripts/test.sh --raise`) |
| `vuln` | runs govulncheck |
| `deadcode` | lists functions nothing calls outside the tests: only `backendtest`'s, `stubtest`'s, `upgradetest`'s, `msg.IDOf`, `msg.NamesOf`, `msg.Recorder` and `msg.Fake`, which the tests use, and anything else it lists gets a caller or goes |
| `cli-docs` | generates the CLI reference |
| `docs` | generates the CLI reference, then serves the docs site |
| `e2e` | runs the end-to-end tests against real VMs |

`mise run test` needs a C compiler for the race detector: Xcode Command Line Tools on macOS, gcc on Linux. It gives each package 30 minutes rather than go test's 10, because the CLI tests run the built binary against many stubs and take far longer on a busy machine. Both test tasks run `scripts/test.sh`, which gives the tests a throwaway `HOME`.

Before and after every change, `mise run fmt && mise run lint && mise run test` must pass.

CI runs `lint` and `test` on macOS and Linux. A change that touches only the docs and Markdown files skips them: the Docs workflow runs `lint:docs` and builds the site instead, and on every push to `main` it deploys next.devvm.dev ([the docs sites](maintaining.md#the-docs-sites)).

## Go lint rules

golangci-lint enforces most of the [Go conventions](#go-conventions) in `.golangci.yml`:

| Rule | Enforces |
| --- | --- |
| forbidigo | only `internal/ui` prints (`fmt.Print*`, `os.Stdout`, `os.Stderr`); only `cmd/devvm` calls `os.Exit`; only `internal/ui` and `cmd/devvm` make a context (`context.Background`, `context.TODO`), every other function takes its caller's; only `internal/platform` reads the host's CPUs, memory and free space, which the rest reads through `platform.Host`; only `internal/cli` calls `testbuild.On`; only `internal/daemon` and `internal/platform` signal a process by its pid (`os.FindProcess`, `syscall.Kill`, `unix.Kill`, `os.Process`'s `Kill` and `Signal`); only `internal/fetch` makes an HTTP client or a transport (`http.Client`, `http.DefaultClient`, `http.Transport`, `http.DefaultTransport`, `http.Get` and the like), which the rest names as `fetch.Client`; nothing calls `os.UserHomeDir`, because the home folder is `paths.Dirs.Home`; only `internal/state` calls `os.WriteFile` or `os.Create` or opens a file with `os.O_TRUNC`, and the rest writes a file whole through `state.WriteFile`, or `state.WriteFileMode` for a mode other than 600; nothing but `daemon.Ready` changes devvm's own environment (`os.Setenv`, `os.Unsetenv`, `os.Clearenv` and `syscall`'s), and a command gets its own through its `Env` |
| depguard | the allowed modules, which are cobra, pflag, cobra's `doc` package for the CLI reference, go-i18n, BurntSushi's toml, `golang.org/x/text`, `x/term` and `x/sys`, and only the `blake2b` package of `x/crypto`, for minisign's prehashed signatures; the tests may also use go-internal. Nothing imports a module that cobra's `doc` package uses but isn't on the list. `internal/proxy`, `internal/daemon` and `internal/safefile` use only the standard library and each other |
| nolintlint | every `//nolint` names its linters and says why, and is still needed |
| reassign | no package reassigns another package's variables, tests included |
| revive | at most 100 statements or 160 lines a function (`function-length`), and a cognitive complexity of 75 (`cognitive-complexity`) |

A fresh context elsewhere, such as for a cleanup that outlives a canceled run, or a lookup that many requests share, takes `//nolint:forbidigo // <why>`.

`testdata/lint/forbidigo.go` breaks each forbidigo rule once, on a line that ends in `// want forbidigo`. `mise run lint:code` lints it with forbidigo alone and fails unless forbidigo reports exactly those lines, so a rule whose pattern stops matching fails there. A new rule gets its line in the file. `./...` leaves `testdata` out, so the file never fails the lint run of the code itself.

## Go conventions

The linters above check most of these, and the structure test checks others ([the structure test](structure.md#the-structure-test)). How a command fails, prints and cleans up is in [CLI conventions](cli.md#cli-conventions).

- **One binary, no C.** devvm builds with `CGO_ENABLED=0`, which `mise.toml` and `.goreleaser.yaml` set, and only the test tasks turn cgo on, for the race detector. `internal/proxy` and `internal/daemon` build only on unix systems for now, because they use `syscall.Kill`, `Setsid` and `Flock`.
- **External tools are commands, never linked.** devvm runs `limactl`, `docker`, `security`, `git`, `chflags` and the other tools it needs as commands, and it uses the standard library where that does the job rather than shelling out. A string that goes into a shell is quoted with `shell.Quote`.
- **Code for one system goes behind build tags** in `internal/platform`, in files named `*_darwin.go`, `*_linux.go` and `*_other.go`.
- **Test seams are fields** that a test sets, such as `Host`, `Exec`, `Runner`, `OS` and `Reporter`, and never package variables, which reassign keeps any package from setting in another. `internal/proxy`'s seams are fields of its `server`, which each test makes.
- **A new dependency needs the maintainers' yes** before it goes on depguard's list.
