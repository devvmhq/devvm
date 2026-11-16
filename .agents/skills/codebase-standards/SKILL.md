---
name: codebase-standards
description: devvm's standards for keeping its code clean and safe, as a short checklist for a commit and a review, drawn from what reviews of devvm's history kept finding (fallbacks that fail open, unbounded or trusted input from a VM, secrets in arguments, hand-rolled parsers, swallowed errors, processes without an owner, runs that a kill or a second devvm breaks, copies of an owner, running VMs at an upgrade, overclaims, fixes without a failing test, tests that need one machine), and how new code matches the existing code's style. Use before a commit, when reviewing a change or a branch, and when a review's finding may be one a standard covers.
---

# devvm's codebase standards

Reviews of devvm's code kept finding the same mistakes, in code that different people and agents wrote for different features. This skill turns each of those into a standard, so a change meets it before a review has to find it, and it says how new code matches the code already there. It isn't a rulebook to apply line by line: a standard matters where a change touches its ground, and each one is here because it recurred or because missing it was costly.

- [standards.md](standards.md) has a section for each standard: the rule, what the reviews found, sometimes a pair of devvm's code that breaks it and code that keeps it, and its guard.
- [rules.md](rules.md) lists the rules that AGENTS.md and the Contributing pages hold, each with what guards it, so a reviewer knows what the gate checks and what's left to read.

A command follows the [cli skill](../cli/SKILL.md), prose and messages follow the [docs-voice skill](../docs-voice/SKILL.md), and a sandbox or a backend's emulation gets its threat check with the security-audit skill, so this skill doesn't restate them.

## The checklist

Go through the items that a change touches. Each links to its section in [standards.md](standards.md).

Safety:

1. **[Fail closed, on every path.](standards.md#fail-closed-on-every-path)** A refusal, a failure or a retry leaves things as strict as they were, and a weaker mode is visible, documented and chosen.
2. **[Bound what you don't control.](standards.md#bound-what-you-dont-control)** What a VM or another process can make devvm read, hold or wait for has a cap in size, count and time.
3. **[Treat what a VM can write as hostile.](standards.md#treat-what-a-vm-can-write-as-hostile)** Reach its files by descriptor, check what it hands back, escape its text, and run nothing it can plant.
4. **[Keep secrets and host data on the host.](standards.md#keep-secrets-and-host-data-on-the-host)** A secret stays out of arguments and shared environments, and only an allowlist crosses into a VM.
5. **[Parse once, with a real parser.](standards.md#parse-once-with-a-real-parser)** The function that checked a value reads it, and an address check knows IPv6.

Failures and processes:

1. **[Never fail silently, and name the real cause.](standards.md#never-fail-silently-and-name-the-real-cause)** An error is never dropped without a reason, and a message names the cause the code found.
2. **[Give every process an owner that ends it.](standards.md#give-every-process-an-owner-that-ends-it)** A context cancels it, something waits for it, and an accept loop goes on through `daemon.AcceptRetry`.
3. **[Survive a kill and a second devvm.](standards.md#survive-a-kill-and-a-second-devvm)** One good copy survives every step, and an OS file lock guards a change, with the state read again under it.

Changes:

1. **[Extend the owner, and wait for a third copy.](standards.md#extend-the-owner-and-wait-for-a-third-copy)** Find the owner first, and give a judge one owner at once.
2. **[Mind what's already running and released.](standards.md#mind-whats-already-running-and-released)** The CHANGELOG says what a running VM keeps, and a change to state runs on every frozen fixture.
3. **[Claim only what the code does, where precision belongs.](standards.md#claim-only-what-the-code-does-where-precision-belongs)** Precise pages hold every limit, and headline pages keep their short claims.

Tests:

1. **[Prove each fix with a test that fails without it.](standards.md#prove-each-fix-with-a-test-that-fails-without-it)** Its oracle comes from outside the code, an absence check has a control, and a sandbox is proven by attacking it.
2. **[Write tests that pass on any host, at any load.](standards.md#write-tests-that-pass-on-any-host-at-any-load)** A test stays off the real clock, assumes nothing about the machine, and uses stubs that behave like the real tool.

## Match the codebase

New code reads like the code around it. Before writing, read a neighbour in the same package and follow it. These are the habits that hold across the codebase, each with a place to see it:

- **Names** are short and plain: `bounded`, `target`, `parsePort`, `vmLog`, and a getter has no `Get` prefix. A file holds one concern and is named for it in lower case, its words run together, such as `linksession.go` or `cloneseed.go`. A `_` in a file's name marks a build tag, a test, a generated file or a part of a parent's file, such as `internal/cli`'s `gc_caches.go` for a subcommand, since each Cobra command has its own file. A large package maps its files in a `doc.go`, as `internal/vm/doc.go` does, or in a README, as `internal/proxy` does.
- **Comments** are full sentences that say what a thing is and why it's that way, starting with its name, as `bounded` in `internal/vm/bounded.go` explains why its buffer isn't embedded. A struct's fields have a comment each where the name isn't enough, as in `vm.Options` and `vm.Env`. A test starts with a comment that states the behavior it checks, and comments describe the code as it is, never its history.
- **Errors** that the user sees are messages: `msg.PortInvalid(strconv.Quote(v)).Because(err)`, never prose built with `fmt` ([Messages](../../../docs/content/docs/contributing/cli.md#messages)). An error usually passes up as it is, as `return err`. When a caller adds context, the wrap is lower case, says what was being done or on what, and uses `%w`, such as `fmt.Errorf("reading devvm's %s: %w", name, err)` or `fmt.Errorf("backend %s: %w", name, err)`. A sentinel is a package-level `ErrX` or `errX` from `errors.New`, such as `safefile.ErrTooLong`, and callers test it with `errors.Is`.
- **Dependencies are fields.** A type's dependencies and test seams are exported fields that its constructor fills and a test replaces, as `NewEnv` fills `vm.Env`'s and a test changes the ones it needs. A call's choices go in a struct of fields, as `vm.Options` holds what `devvm` was asked for, never functional options. A package's type embeds the one `msg.Reporter` its constructor takes, and a package-level variable is rare enough that the structure test lists each one.
- **Tests** are table tests next to the code, as a map from input to what's wanted, as `TestPortsRows` has, or a slice of structs (`for _, tc := range []struct{…}{…}`), with `t.Run` where cases need names of their own. They use the standard library alone, with no assertion library, call `t.Fatal` on a setup step's error, and name the case and what came out against what was wanted, such as `t.Errorf("rows for %s: %v, want %s", shown, got, want)`. They compare a message by its ID (`msg.IDOf`, `msg.Recorder`) and run host commands through `stubtest` ([Go test helpers](../../../docs/content/docs/contributing/tests.md#go-test-helpers)).
- **Scripts in the VM** follow AGENTS.md's rules for bash 3.2 and pins, and they run on every base devvm supports, Alpine's BusyBox included, which has no `findmnt`.
- **Prose** follows the docs-voice skill, and Go follows [Go conventions](../../../docs/content/docs/contributing/_index.md#go-conventions), which the linters and the structure test enforce.

## Reviewing a change

1. **Read the whole diff,** and the code around each change, because a path used twice or a copy of an owner spans more than the lines that changed.
2. **Check each standard the change touches,** and the style of the code it adds against its neighbours.
3. **Check the claims** that the change adds or touches, in the docs, the CHANGELOG, comments and the commit message, against the code.
4. **Prove the fixes:** revert each fix and run its test, which must fail.
5. **Hand off what another skill owns:** a command to the cli skill's review, prose to the docs-voice skill, and a sandbox or a backend's emulation to the security-audit skill.
6. **Run the gate,** `mise run fmt && mise run lint && mise run test`, and the end-to-end sections for what changed when the change touches VMs.
7. **Report each finding** with the standard it breaks, where it is and what would fix it. A finding that no standard covers is fine to report as it is.

## Adding a standard

A finding becomes a standard when it has recurred, or when missing it was costly, and when a contributor can apply it while writing or reviewing a change. A one-off, a rule that a linter or a test already enforces, or a detail of one backend doesn't qualify. Before adding one, look for a standard it extends, because a short list gets used. A new standard gets a section in [standards.md](standards.md) with its rule, what the reviews found and its guard, and a line in the checklist. When a check that enforces it lands, the standard names that check as its guard.
