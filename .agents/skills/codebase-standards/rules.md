# The rules with an owner elsewhere

These rules are written in [AGENTS.md](../../../AGENTS.md) and the [Contributing](../../../docs/content/docs/contributing/_index.md) pages, which stay their one home. Each table gives a rule in a few words, links to where it's written in full, and names what guards it, so a reviewer can skip what the gate already checks and read for the rest. A guard that names a test or a linter fails the gate when the rule breaks, except for the end-to-end checks, which `mise run e2e` runs, and the CI jobs a row names. "Review only" means nothing fails, so the reviewer checks it.

## Go code

| Rule | Written in | Guard |
| --- | --- | --- |
| Only `internal/ui` prints, and only `cmd/devvm` calls `os.Exit` | [Go lint rules](../../../docs/content/docs/contributing/_index.md#go-lint-rules) | forbidigo (`fmt.Print*`, `os.Stdout`, `os.Stderr`, `os.Exit`) |
| A function takes its caller's context, and a fresh one says why | [Go lint rules](../../../docs/content/docs/contributing/_index.md#go-lint-rules) | forbidigo (`context.Background`, `context.TODO`), contextcheck |
| Every `//nolint` names its linters and says why | [Go lint rules](../../../docs/content/docs/contributing/_index.md#go-lint-rules) | nolintlint |
| A function stays within 100 statements, 160 lines and a cognitive complexity of 75 | [Go lint rules](../../../docs/content/docs/contributing/_index.md#go-lint-rules) | revive |
| No type has more than 24 methods, no `func init`, and few package vars | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `max-methods`, `no-init` and `package-vars` |
| Packages import only down their layers | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `layers` |
| Test seams are fields, never package vars | [Go conventions](../../../docs/content/docs/contributing/_index.md#go-conventions) | reassign, and the structure test's `package-vars` |
| Dependencies stay on the allowlist, and the proxy's packages use the standard library alone | [Go lint rules](../../../docs/content/docs/contributing/_index.md#go-lint-rules) | depguard |
| External tools run as commands, through the UI's `LoggedCmd`, a backend's `Runner` or `internal/hostgit` | [CLI conventions](../../../docs/content/docs/contributing/cli.md#cli-conventions) | the structure test's `owners` (`commands`, `execSites`) |
| A string that goes into a shell is quoted with `shell.Quote` | [Go conventions](../../../docs/content/docs/contributing/_index.md#go-conventions) | the structure test's `literals` (`'\''` outside `internal/shell`), and `TestEmbeddedScripts` for scripts in Go strings |
| A `Wait`'s error and a `strconv` parse's are never assigned to `_` outside tests, but where `droppedErrors` says why | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `dropped-errors` (`droppedErrors`) |
| An `io.ReadAll` outside tests reads through an `io.LimitReader`, but where `unboundedReads` says why | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `read-limits` (`unboundedReads`) |
| Two folders' identities are compared only by `platform.SameFolder`, but where `exactFolderIDs` says why | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `folder-ids` (`exactFolderIDs`) |
| Code for one system goes behind build tags in `internal/platform` | [Go conventions](../../../docs/content/docs/contributing/_index.md#go-conventions) | forbidigo for the host's CPUs, memory and free space outside `internal/platform`, and the rest is review only |
| devvm builds with `CGO_ENABLED=0` | [Go conventions](../../../docs/content/docs/contributing/_index.md#go-conventions) | `mise.toml` and `.goreleaser.yaml` set it, and CI's `nocgo` job runs `internal/landlock`'s and `internal/proxy`'s tests that way |
| A nil value that may reach a dereference is fixed or triaged | [Nil checks](../../../docs/content/docs/contributing/_index.md#nil-checks) | `mise run nilaway` and CI's `nilaway` job, which warn without failing, and `mise run nilaway:linux`, which fails on a new finding or a stale baseline line and must pass before a push to `main` |
| A dependency with a known vulnerability is updated | [Tools and tasks](../../../docs/content/docs/contributing/_index.md#tools-and-tasks) | `mise run vuln` and CI's `vuln` job |

## Owners

| Rule | Written in | Guard |
| --- | --- | --- |
| Before adding a helper, a parser, a path, a reader, a message or a table, extend its owner | [One owner for each concern](../../../docs/content/docs/contributing/structure.md#one-owner-for-each-concern) | the checks below where an owner has one, and otherwise review only and [the consolidation pass](../../../docs/content/docs/contributing/structure.md#the-consolidation-pass) before a release |
| Each owner that has a check: `fetch`'s HTTP clients, `daemon`'s signals, `state`'s whole files, `safefile`'s reads, `paths`' folders and home, `msg.Safe`, `proxy.Public`, `proxy.NormHost`, `vmname.VM`, `redact.Secret`, `hostgit`, the agents' and toolchains' registries, and the rest | [The checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners) | the check each row of that table names, a forbidigo rule or the structure test, with `TestOwnersDoc` and `TestOwnersAgree` keeping the table and the checks together |
| A file a VM can write or swap is changed through what opened it, never by its path, in `internal/proxy`, `internal/vmlog` and `internal/vmssh` | [The checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners) | forbidigo (`os.Chmod`, `os.Chtimes`, `os.Chown`, `os.Lchown`, `os.Truncate`), with a `nolint` that says why for a socket devvm has just made |
| A secret's value is a `redact.Secret`, revealed only where it's used, and a file that holds one writes it through its own wire type | [Secrets in the code](../../../docs/content/docs/contributing/structure.md#secrets-in-the-code) | forbidigo (`Reveal`), the structure test's checks of embedding and `Reveal() string`, and `TestSecret`, while a new secret held in a string is review only |
| Two places that must agree but can't be one have a test that they agree | [Agreement tests](../../../docs/content/docs/contributing/structure.md#agreement-tests) | each pair's own test, such as `TestDefsComplete` and `TestToolchainsComplete`, while a new pair is review only |

## Processes and goroutines

| Rule | Written in | Guard |
| --- | --- | --- |
| Every goroutine has something that waits for it, or says why nothing does | [The structure test](../../../docs/content/docs/contributing/structure.md#the-structure-test) | the structure test's `goroutines` (`goroutineSites`) |
| A package that starts goroutines checks for leaks | [Goroutine leaks](../../../docs/content/docs/contributing/tests.md#goroutine-leaks) | the structure test's `goroutines`, which requires `leaktest.Main`, and `leaktest.Check` in a test that starts and stops them |
| The startup check only reports, and devvm never installs host software without asking | [AGENTS.md's Rules](../../../AGENTS.md#rules) | review only |
| A command fails by returning its error, and `execute` alone prints it | [CLI conventions](../../../docs/content/docs/contributing/cli.md#cli-conventions) | `TestExecuteShowsEachFailure`, and forbidigo's printing rule |
| Whatever a failure would leave registers a cleanup with `AddCleanup`, which gets a fresh context | [CLI conventions](../../../docs/content/docs/contributing/cli.md#cli-conventions) | review only |
| A VM's state changes through `state.VM.Update`, under its lock, taken in order | [The checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners), in its paragraph on `vm.json` | `TestLockOrder`, as test builds panic on a lock taken out of order, and forbidigo's `os.WriteFile` rule |

## The command line and messages

| Rule | Written in | Guard |
| --- | --- | --- |
| A new or changed command follows the cli skill's checklist, then its review | [the cli skill](../cli/SKILL.md) | the tests each of its rules names, and its review for the rest |
| Arguments and flag values are checked before anything runs, and a usage error exits with 2 | [Command arguments](../../../docs/content/docs/contributing/cli.md#command-arguments) | `TestUsageErrorsEveryCommand` |
| Every message is in `locales/en.toml`, called through its generated function, with its doc link | [Messages](../../../docs/content/docs/contributing/cli.md#messages) | `TestGeneratedFileIsCurrent`, `TestEveryMessageRenders`, `TestEveryMessageIsUsed` and `TestDocLinks` |
| Prose is never built with `fmt` | [Messages](../../../docs/content/docs/contributing/cli.md#messages) | review only |
| Every question declares what answers it | [Questions](../../../docs/content/docs/contributing/cli.md#questions) | `TestQuestionsDeclareAnswers` |
| Slow work runs under a status line, a `Status` with a label message or a `LoggedCmd` step, and none spans work that takes the terminal | [Status lines](../../../docs/content/docs/contributing/cli.md#status-lines) | the structure test's `status-lines` (`statusSites`), a lock's wait included, a test build's panic on a `nil` `Waiting` (`platform.CheckWaiting`), forbidigo's `syscall.Flock` rule, `TestLaunchStatusLines` and `TestCommandStatusLines` for the backend's calls, `TestVMLockWaits` and `TestCheckLayoutWaits` for the state's locks, `TestSSHAddOffStatusLine` for `ssh-add`, and `TestMessagesClearTheStatusLine`, `TestQuestionsHoldTheStatusLine`, `TestSecretAndPickerHoldTheStatusLine`, `TestTerminalHoldsTheStatusLine` and `TestStatusLinesNest` for the UI, while a caller's status line that `statusSites` names, a backend's methods in a command the tests don't run, and a wait's label are review only |
| Tests check which message and with what, never its wording | [Names mode](../../../docs/content/docs/contributing/cli.md#names-mode) | the CLI tests run in names mode unless a script turns it off, while a Go test is review only |
| Messages follow the docs voice, only shorter | [the docs-voice skill's Messages](../docs-voice/SKILL.md#messages) | review only, with its `scan.sh` |

## State and releases

| Rule | Written in | Guard |
| --- | --- | --- |
| Every release's state keeps working, and its frozen fixture is never edited | [Released state](../../../docs/content/docs/contributing/structure.md#released-state) | `TestFixtureVMStates`, `TestFixtureVMsRead`, `TestFixtureSettings`, `TestFixtureAgentStore`, `TestStateFixturesListed`, `TestFixtureScripts` and the `fixture_v*.txtar` scripts, and a review checks that a fixture's files are unchanged |
| A change to existing state gets a Breaking entry in the CHANGELOG | [Changing what devvm does with state](../../../docs/content/docs/contributing/structure.md#changing-what-devvm-does-with-state) | review only |
| A change to the layout updates its listing, and a new layout version adds a migration | [Changing the state layout](../../../docs/content/docs/contributing/structure.md#changing-the-state-layout) | `layout.txtar`, `TestMigrations` and `TestCheckLayout` |
| Nothing goes in a project folder: state is in devvm's data and config folders | [AGENTS.md's Rules](../../../AGENTS.md#rules) | review only, as the structure test's check of paths covers only devvm's own folders |
| devvm never deletes a project VM on its own | [AGENTS.md's Rules](../../../AGENTS.md#rules) | the structure test's `owners` for the VM-name grammar (`vmname.IsVM`), so gc never takes a project VM for an image, and the rest is review only |

## Backends

| Rule | Written in | Guard |
| --- | --- | --- |
| A backend implements `backend.Backend`'s four roles, declares `Caps` for every feature and returns its errors | [How a backend plugs in](../../../docs/content/docs/contributing/extending.md#how-a-backend-plugs-in) | `TestRegisterChecksCaps`, `TestCapsCheck`, the conformance suite (`TestConformance`, `TestConformanceFinds`) and the structure test's `guardOwner` |
| A missing feature fails, naming the VM and what to do, and never falls back silently | [How a backend plugs in](../../../docs/content/docs/contributing/extending.md#how-a-backend-plugs-in) | the registry's guard (`TestGuardRefuses`, `TestGuardRoles`) |
| Every emulation is refused until it's reviewed | [AGENTS.md's Backends](../../../AGENTS.md#backends) | `TestUnreviewedEgressRefused` and `TestUnreviewedLetThrough`, and the review itself is a design note, a threat check and end-to-end checks |
| Lima edits only what differs, in one edit | [AGENTS.md's Backends](../../../AGENTS.md#backends) | `TestStartEditsOnlyWhatDiffers`, `TestStartWantInOneEdit`, and the fixture scripts' launches without a Lima edit |

## Images, assets and scripts in the VM

| Rule | Written in | Guard |
| --- | --- | --- |
| An image's key hashes everything that goes into it, the backend included | [AGENTS.md's Rules](../../../AGENTS.md#rules) | `TestKeyChangesWithEachField`, while a new input that bypasses `image.Input` is review only |
| Every download in `assets/` is pinned and checked | [AGENTS.md's Rules](../../../AGENTS.md#rules) | `TestBumpCoversPins` checks that `scripts/bump.sh` moves every pin, and a review checks that each download has one |
| A script sources the prelude before it reads its variables | [One owner for each concern](../../../docs/content/docs/contributing/structure.md#one-owner-for-each-concern) | `TestPreludeUsers` |
| Scripts in the VM are bash 3.2-safe and shellcheck-clean | [AGENTS.md's Rules](../../../AGENTS.md#rules) | shellcheck in `mise run lint:code`, and `TestEmbeddedScripts` for scripts in Go strings, while bash 3.2 is review only |
| Shared tools stay read-only in VMs | [AGENTS.md's Rules](../../../AGENTS.md#rules) | the end-to-end `tools` section's checks that the share is read-only, even for root, while a write redirected elsewhere is review only |
| Credentials never go into images, the host's agent history never goes into VMs, and devvm's own files never get a shared copy on disk | [AGENTS.md's Rules](../../../AGENTS.md#rules) | review only |
| A new or changed shared tool updates its page and follows Contributing's checklist | [Adding a toolchain](../../../docs/content/docs/contributing/extending.md#adding-a-toolchain) and [Adding a coding agent](../../../docs/content/docs/contributing/extending.md#adding-a-coding-agent) | `TestToolchainsComplete`, `TestDefsComplete`, `TestSharedToolsPolicies` and `TestConfigTools` |
| No build script is JavaScript but the docs' `docs/render-diagrams.mjs`, and no npm package is installed outside mise's tools but its pinned pele | [AGENTS.md's Rules](../../../AGENTS.md#rules) | review only |

## Tests

| Rule | Written in | Guard |
| --- | --- | --- |
| New logic gets a Go table test and a CLI test | [Go test helpers](../../../docs/content/docs/contributing/tests.md#go-test-helpers) | the coverage baselines ([Coverage](../../../docs/content/docs/contributing/tests.md#coverage)) |
| Tests never touch the Keychain, real VMs or your folders | [Go test helpers](../../../docs/content/docs/contributing/tests.md#go-test-helpers) | `scripts/test.sh` gives the tests a throwaway `HOME`, and the CLI tests run in a sandbox against the stubs, and the rest is review only |
| Timeouts and lifecycles are tested in a synctest bubble | [Time and lifecycles](../../../docs/content/docs/contributing/tests.md#time-and-lifecycles) | review only ([Write tests that pass on any host](standards.md#write-tests-that-pass-on-any-host-at-any-load)) |
| A test runs git only through `stubtest.Git` | [Go test helpers](../../../docs/content/docs/contributing/tests.md#go-test-helpers) | the structure test |
| The tests pass in any time zone | [Any time zone](../../../docs/content/docs/contributing/testing.md#any-time-zone) | CI runs `mise run test` in UTC on macOS and with `TZ=America/Los_Angeles` on Linux |
| A change that touches VMs passes the end-to-end sections for what changed | [End-to-end tests](../../../docs/content/docs/contributing/tests.md#end-to-end-tests) | review only |

## Docs and commits

| Rule | Written in | Guard |
| --- | --- | --- |
| Every behavior change updates its page in the same change | [AGENTS.md's Rules](../../../AGENTS.md#rules) | review only |
| Prose follows the docs voice | [the docs-voice skill](../docs-voice/SKILL.md) | Vale (`AIWords`, `Filler`, `QuotedCommand`) and `TestUserPageProse` in `mise run lint:docs`, and the rest is review only |
| User pages name no repo paths, Go identifiers or message names | [Writing docs](../../../docs/content/docs/contributing/docs.md#writing-docs) | review only |
| A devvm command line in the repo is one devvm has | [The CLI rule tests](../../../docs/content/docs/contributing/cli.md#the-cli-rule-tests) | `TestCommandLines` |
| The CLI reference is generated | [The CLI reference](../../../docs/content/docs/contributing/docs.md#the-cli-reference) | `TestReferenceIsCurrent` |
| One commit for each task, in Conventional Commits, with no tracker's IDs | [AGENTS.md's Commits and tasks](../../../AGENTS.md#commits-and-tasks) | CI's PR title job (`.github/workflows/pr-title.yml`) checks a pull request's title, its squashed commit's subject, against AGENTS.md's types, and the rest is review only |
