# What reviews kept finding

Reviews of devvm's code found the same kinds of mistake again and again, in code that different people and agents wrote for different features. Each section here is one of them, written as the standard that would have prevented it: the rule, what the reviews found, sometimes a short pair of devvm's code that breaks the rule and code that keeps it, and what guards it. A guard that names a test means `mise run test` fails when that test's case breaks, and "review only" means the reviewer checks it by hand.

A standard applies where a change touches its ground, and judgment decides the rest. A change to how the CLI prints a table has no VM input to bound, and a refactor that keeps its tests passing needs no new failing test.

## Fail closed, on every path

A path that refuses, fails, falls back or tries again leaves things at least as strict as they were. A security step, such as a VM's confinement, its `.git` protection or a host check, sits on the one path that every start, retry and restart takes, rather than in each branch, so a new branch can't miss it. What a confined process reports about itself is checked against what devvm gave it, and when devvm can't tell whether something still holds, it stops the thing it can't vouch for and says so.

A weaker mode is fine when the user sees it, the docs describe it and the maintainers chose it. devvm warns and runs a VM's proxy without its sandbox on a host that has none, for example, rather than refusing to start. What's never fine is a fallback that nobody sees.

The reviews found this on the start's paths more than a dozen times. An export and rm's check of a `--clone` VM started it without the launch's `.git` check and network policy self-check, until both went through the launch's own start. The broker counted its own `SIGTERM` to a hung child as a failure of `sandbox-exec` and started the child again unconfined, and it took the confinement that a child's ready named, so a child it had started unconfined could report `full seatbelt`. A refused tier change could leave a looser proxy running.

Wrong, keeping a running VM's proxy whatever it enforces:

```go
if _, up := p.PID(vm); up && !touched {
    p.Warn(msg.NetProxyKept(vm))
    return c, err
}
return c, unproxied(ctx, p, b, vm, err)
```

Right, as `netpolicy.Proxy.PrepareRunning` has it, keeping it only while it enforces the state's tier and both of its processes run:

```go
if _, up := p.PID(vm); up && childProc(p, vm).Running() && !touched && enforcesNow(p, st) {
    p.Warn(msg.NetProxyKept(vm))
    return c, err
}
return c, unproxied(ctx, p, b, vm, err)
```

**Guard:** each refusal and fallback that exists has tests, such as `TestPrepareRunningRefused`, `TestStartChildSandbox`, `TestSandboxFailed` and `TestReadConfined`, and the backend registry's guard refuses a missing feature (`TestGuardRefuses`). A new path is review only.

## Bound what you don't control

Anything that a VM, the proxy's child, a peer on a socket or another process can make devvm read, hold, wait for or do again has a bound in size, in count and in time. The bound goes on what devvm reads and holds, never on the pattern that recognizes it, so a long value is still found. A listener whose bytes a VM controls has read and header timeouts and a cap on its connections, and a wait on something another process holds, such as a lock, gives up after a deadline and says so. Arithmetic on a size a VM gave checks for overflow.

The reviews found a rotation that read a sparse `proxy.log` whole into memory, a redaction that read one unended line from its start every second, locks that a child could hold for ever, and a password pattern given a length limit that then skipped a registry's long token. A writer that caps what it keeps must not embed its buffer, because `io.Copy`, as `os/exec` copies a command's output, then reads past the cap through the buffer's `ReadFrom`, as `bounded` in `internal/vm/bounded.go` explains.

Wrong, as the proxy once read a log to rotate it:

```go
data, err := os.ReadFile(r.path)
```

Right, as `internal/proxy/rotate.go` reads one, through its descriptor and no more than it keeps:

```go
from := copyFrom(r.fd, fi.Size(), 2*r.limit)
if err := copyAtomic(r.path+".1", io.NewSectionReader(r.fd, from, fi.Size()-from), 0o600); err != nil {
```

**Guard:** `safefile`'s reads and `fetch.ReadCapped` have their caps, `safefile.Walk` reads a folder in batches and stops at its bound on the entries it reads (`TestWalkBound`), where `fs.WalkDir` reads each folder whole first, the structure test's `read-limits` rule fails on an `io.ReadAll` outside tests whose reader isn't an `io.LimitReader` made in the call, unless `unboundedReads` lists its function with why, and tests keep the bounds that reviews found: `TestRotateCopiesAHugeLog`, `TestInLongLine`, `TestLongSecrets`, `TestRotateLockHeld`, `TestNixLockWaits`, `TestBounded`, and `FuzzBrokerFrames` and `FuzzFrameStreams` for the broker's frames. A new input is review only.

## Treat what a VM can write as hostile

A VM can write its project folder, its logs folder and the shared Nix cache, and it controls every byte it sends back. devvm reads all of it as an attacker's:

- **Files by descriptor.** A file in a folder that a VM can write is opened once, without following a link in any part of its path and without blocking on a FIFO, through `internal/safefile`, an `os.Root` or `proxy.OpenLog`, and checked to be a regular file with one link. From then on, devvm reads, writes, empties and changes it through that descriptor (`f.Truncate`, `f.Chmod`, `futimes`), never by its path again, because the VM can swap the name between two uses. A folder, such as the project folder, is checked by its identity rather than its path.
- **Checked against what it can't choose.** What a VM hands back is checked against a source the VM can't pick. The packages a build VM records are checked on the host against Ubuntu's signed archive lists, every pocket's included, and a cached package file is hashed again before devvm keeps it.
- **Escaped for where it goes.** Text from a VM is data. It's escaped for the format it's written into, such as a systemd unit, a plist, YAML or the ssh config, and control characters are refused or dropped (`msg.OneLine`, `msg.Printable`).
- **Nothing it can plant runs on the host.** Host tools run by absolute path, with a minimal environment, and without anything that a project's config can name, such as git's hooks, filters or `core.sshCommand` (`internal/hostgit`).

The reviews found devvm opening `proxy.log` again by its path at each start and emptying it by its path at each rotation, which would have followed a link a child left there into a file of yours, and a VM started on a project folder that was replaced after the finder found it. Host git once ran programs that a repo's config named.

Wrong, as the Nix cache once marked a file used, by its path:

```go
func touch(file string) {
    now := time.Now()
    _ = os.Chtimes(file, now, now)
}
```

Right, as `internal/proxy/nixcache.go` marks the file it opened (`touchFile` is `futimes` on macOS and `utimensat` with no path on Linux):

```go
func touch(f *os.File) { _ = touchFile(f, time.Now()) }
```

**Guard:** forbidigo refuses `os.Open`, `os.OpenFile`, `os.ReadFile` and `os.Root`'s openers in the files that the `safefile` row of [the checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners) lists, and forbidigo refuses `os.Chmod`, `os.Chtimes`, `os.Chown`, `os.Lchown` and `os.Truncate` in `internal/proxy`, `internal/vmlog` and `internal/vmssh`, but for a socket devvm has just made, with a `nolint` that says why. The child's seccomp filter on Linux refuses it a change by path (`TestSelf`). `TestOpenLogReplacesWhatIsPlanted`, `TestRotateCopiesWithoutItsPath`, `TestTrimNixCachePlanted`, `TestOneLineDropsInvisible` and `internal/hostgit`'s tests, such as `TestCommandNoHooks` and `TestCommandSSHOnly`, keep what reviews found, and the end-to-end `network` and `nix` sections plant links, FIFOs and hard links. A path-based change elsewhere is review only.

## Keep secrets and host data on the host

A secret never goes into a process's arguments, which any local user can read with `ps`, or into an environment that more than one process shares. It goes on standard input, into a file only you can read, or into the environment of the one child that needs it. A secret's value is a `redact.Secret`, revealed only where it's used ([Secrets in the code](../../../docs/content/docs/contributing/structure.md#secrets-in-the-code)).

What crosses into a VM is an allowlist: the fields devvm parsed and knows, never a whole file with the known secrets taken out. A log that a VM can drive goes through `internal/vmlog`, which caps and redacts it, or through the proxy's own rotating log, and a rotation masks the start of a secret its cut leaves (`redact.Unended`). Credentials never go into an image, and your agents' history never goes into a VM.

This was the most frequent security finding, about 15 times for secrets in arguments or a shared environment and about 11 for whole files sent to a VM. The VM's proxy password reached the guest through `limactl edit --set`, visible to `ps`, Codex and Claude Code VMs got every field of their login files rather than the ones devvm knows, and Lima's boot script, which runs under `set -x`, put the proxy password into the serial console log.

**Guard:** forbidigo refuses `Reveal` without a `nolint` that says why, `TestStartConfined` checks that the proxy's password is in no command's arguments and goes into the VM's `lima.yaml` instead, and `TestRotateCutSecret`, `TestUnended` and `FuzzSplit` keep the redaction. The end-to-end `network` section's `netNoLeaks` looks for each VM's proxy password and the section's tokens in every run log, proxy log and Lima log. A new argument, environment variable or file sent to a VM is review only.

## Parse once, with a real parser

A value is read with a real parser for its format, such as `BurntSushi/toml` for mise's lock or `net/netip` for an address, never with a regular expression or `strings.Split` over a format that has quoting or escapes. The function that checked a value is the one that reads it later, so the two can't disagree, and when devvm needs a tool's effective setting, it asks the tool rather than parsing its config again. A check of an address handles IPv6, and the IPv6 forms that carry an IPv4 address (`::ffff:`, NAT64 and 6to4), as `proxy.Public` does.

About 17 fixes came from this. Network policy files had a lenient reader in one place and a strict one in another, so a broken deny rule was skipped rather than refused. A ports value with a bad entry read as port 0, because one of several port parsers dropped `strconv.Atoi`'s error, until `backend.ParsePortRanges` became the only one. Transparent mode logged IPv6 connections as refusals, and a 6to4 address whose IPv4 address was private passed the proxy's floor as public.

**Guard:** each owner's parser has its table tests and often a fuzz target, such as `TestParsePortRanges`, `FuzzPublicHostSite` and `TestInternalOwn6to4`, and [the checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners) keep some readers in their owner. A new hand-rolled parser is review only.

## Never fail silently, and name the real cause

No error is dropped without a reason in a comment: not a `Wait`'s, not a parse's, not a `|| true` in a script. A loop that skips an item it can't handle says so, and a check whose output is empty or shorter than it should be fails rather than passing. An error that devvm turns into a message keeps its cause with `.Because(err)`.

Before a message names a cause, the code finds that cause. Errors that need different actions get different messages, and the advice in each one works in the state the user is in. A message that tells a user whose VM can't start to run devvm restart sends them in a circle.

The reviews found about 16 swallowed errors and about 17 messages that named the wrong cause. A shared tool that failed to mount was skipped with only a line on standard error, which no page described and nobody had chosen, so mise downloaded it into the VM instead. The proxy's floor, which refuses this machine's addresses, refused none when it read no addresses. A name the host couldn't resolve was blocked with "can't resolve it" whatever the cause, so a lookup cancelled because its client went away looked like a DNS failure.

**Guard:** errorlint, nilerr and nilnesserr catch some of it in the linters, and `TestExecuteShowsEachFailure` checks that a command's failure reaches the user. The structure test's `dropped-errors` rule fails on a `Wait`'s error or a `strconv` parse's assigned to `_` outside tests, unless `droppedErrors` lists its function with why. Any other error assigned to `_`, and a message's cause, are review only.

## Give every process an owner that ends it

Every process and goroutine devvm starts has an owner that bounds it and ends it: a context that cancels it, a `WaitDelay` for its pipes, a stopper that kills a half-started pipeline, and something that waits for it. An accept loop goes on after an error that leaves its listener open, through `daemon.AcceptRetry`, because a loop that returns at the first `EMFILE` leaves its port bound and dead, while `http.Server`'s `Serve` tries again itself.

The reviews found relays that outlived their forward and their link, and an ssh that a stuck relay held open. They also found the same accept loop written four ways, two of them returning at the first error, before `daemon.AcceptRetry` became its owner.

**Guard:** the structure test's `goroutines` rule lists every function that starts a goroutine with what waits for it, `leaktest.Main` and `leaktest.Check` fail on a goroutine that outlives its tests ([Goroutine leaks](../../../docs/content/docs/contributing/tests.md#goroutine-leaks)), and only `internal/daemon` and `internal/platform` signal a process by its pid (forbidigo). `TestAcceptRetry` and each loop's own test, such as `TestForwarderAcceptRetries`, keep the accept loops, and `TestProcStop` stops real processes. A process's bound is review only.

## Survive a kill and a second devvm

Assume the last run was killed at the worst moment, and that another devvm runs beside this one:

- **Killed runs.** A destructive step is ordered so that one good copy survives at every point, a "done" marker is written last, a file is written whole through `state.WriteFile`, and a cancelled or failed result is never cached.
- **Concurrent runs.** Two runs that change the same thing take an OS file lock (`platform`'s locks, `state.VM.Update`), and read the state again once they hold it. A lock is never a folder or a pid file, which a killed run leaves behind.

About 13 fixes each came from these. A second devvm took a remake that was still running for one that had stopped and finished it, and an undo that failed partway could leave the saved home deleted. The remake holds the VM's lock and reads its state again under it, and it deletes the saved home only after its record is gone. Two runs that pinned a VM's host keys at once could lose one's change to `known_hosts`, and retention's lock was a folder, which a crash left held.

**Guard:** forbidigo keeps whole-file writes in `internal/state`, and `TestLockOrder` panics in test builds on a lock taken out of order. The order of a destructive step is review only.

## Show what devvm waits for

Any work that can take noticeable time runs under the UI's status line, so the user sees what devvm waits for rather than a terminal that looks hung. Slow work is anything whose time devvm doesn't control: a host command, host git, a command in a VM, a backend's own commands, such as `limactl list` or `limactl --version`, a request over the network, a walk of a folder the user owns, a lookup of a folder's disk with `diskutil`, and a wait for a lock, a timeout or another process. A logged command shows its step on its own (`LoggedCmd`, a backend `Runner`'s `Run`), and the rest shows one with the Reporter's `Status`, its label a message in `locales/en.toml`. Status lines nest, so a phase can show one around steps that show their own. Work that takes the terminal never runs under one: a VM's shell, `ssh-add` asking for a passphrase, an agent's login in the VM.

The reviews found the newest commands' slow work all without one: devvm agent import walked and copied up to 128 MiB and ran git on each marketplace's config, devvm ls looked up each folder's disk after Lima's list, and devvm doctor read every VM's state, each with nothing on the screen. They also found the status line itself drawing over what devvm said: a warning landed on its line, and its redraw erased a question's prompt.

Wrong, as devvm ls read the VMs, Lima's list and a `diskutil` for each folder with nothing on the screen:

```go
inv, err := vm.ReadInventory(cmd.Context(), env.Dirs, env.Registry, vm.AllBackends)
```

Right, as `vm.ListAll` has it, under one status line that its steps' own lines nest in:

```go
defer e.Status(msg.ReadingVMs())()
inv, err := ReadInventory(ctx, e.Dirs, e.Registry, AllBackends)
```

A test records the label rather than a terminal: a `msg.Fake` gives each label to its `OnStatus`, and `Showing` is the one showing, so a seam the slow work goes through records what showed meanwhile, as `TestAgentImportStatus` does:

```go
im.Opencode = func(context.Context, ...string) ([]byte, error) {
    m, _ := im.fake.Showing()
    during = append(during, "opencode: "+m.ID())
    return nil, exec.ErrNotFound
}
```

**Guard:** the structure test's `status-lines` rule fails on a function that makes a host command, runs host git, walks a folder, looks up a volume, makes a request through `internal/fetch` or runs a backend's command outside a step with no status line of its own, unless `statusSites` lists it with why, such as the caller's status line it runs under. `TestLaunchStatusLines` fails on a backend call that a launch makes with no status line showing, `TestSSHAddOffStatusLine` on an `ssh-add` under one, and `TestMessagesClearTheStatusLine`, `TestQuestionsHoldTheStatusLine`, `TestSecretAndPickerHoldTheStatusLine`, `TestTerminalHoldsTheStatusLine` and `TestStatusLinesNest` keep the status line off what devvm says and asks. That a caller's status line covers a listed call, a backend's methods outside a launch and a wait on a lock are review only.

## Extend the owner, and wait for a third copy

Before adding a helper, a parser, a path, a file reader, a constant, a limit, a message or a table, find its owner and extend it, as [One owner for each concern](../../../docs/content/docs/contributing/structure.md#one-owner-for-each-concern) says. When the owner is out of reach, as most packages are for the proxy, move the owner to a package both can import.

An owner that exists is always extended. Code that has no owner yet is different: a second copy of it is a judgment call, because a copy can be cheaper than the wrong shared helper, so wait for a third before extracting one. The exception is a copy of a judge, such as what counts as a public address or the one form of a host, because two judges that drift apart let through what one of them refuses. Those get one owner at once, and a forbidigo rule or a structure-test check where the check can be exact.

The reviews found hosts read by hand in five packages besides the proxy, each lowercasing and trimming a dot in its own way, so a deny rule and the check it guarded could read one host as two, and `--from`'s registries judged public by a copy of `proxy.Public`.

Wrong, as `netpolicy.RiskOf` once read a host:

```go
host = strings.ToLower(strings.TrimSuffix(host, "."))
```

Right, in the one form that every judge of a host compares:

```go
h := proxy.NormHost(given)
```

**Guard:** the structure test's owner checks and the forbidigo rules in [the checks on owners](../../../docs/content/docs/contributing/structure.md#the-checks-on-owners), with `TestOwnersDoc` and `TestOwnersAgree` keeping that table current. Any other copy is review only, in [the consolidation pass](../../../docs/content/docs/contributing/structure.md#the-consolidation-pass) before a release.

## Mind what's already running and released

A user upgrades devvm with VMs running and with state from every release since 0.1.0, so a change says what happens to both:

- **Running VMs.** A change to what devvm runs beside a VM, such as its proxy, its port forwarder, its agent relay or its Lima config, gets a line under After you upgrade in `CHANGELOG.md`'s Unreleased section, saying what a running VM keeps until its next start and what to run to get the change at once. The new devvm still stops, lists and reads the logs of what the old one left running. What devvm does to a running VM follows the start it runs under, not the stored setting, which the user may have changed since.
- **Released state.** A change that adds a state field runs the commands that read and write it on each frozen `testdata/v<release>/` fixture, in the same commit, and so does a command the CHANGELOG tells users to run, as [Released state](../../../docs/content/docs/contributing/structure.md#released-state) says. A change to what devvm does with existing state gets a Breaking entry in the CHANGELOG.

The agent relay, the ssh port that moved to one of the ports browsers refuse, and the proxy that became two processes each needed an After you upgrade line that a review found missing. The ssh config's agent once followed the stored setting rather than the VM's running start, and the settings that devvm mount-cache and devvm network transparent added to `vm.json` went in without a run of either command on 0.1.0's state.

**Guard:** `TestFixtureScripts` and the `fixture_v*.txtar` scripts run today's commands on each release's state, and `TestV010ProxyConfigsStart` starts a broker and its child from 0.1.0's proxy configs. The CHANGELOG line is review only.

## Claim only what the code does, where precision belongs

The How it works pages, Staying safe, Contributing, the CHANGELOG, comments, commit messages and messages say what the code does, with its limits. A guarantee with "never", "only", "every" or "no" holds on every path, or it names the paths where it doesn't. Before a commit, check each claim the change makes, and each existing claim about the code it changed, against that code.

The README, the home page and Getting started keep their short headline claims, such as "ready in seconds" or "reach only the hosts you allow", without qualifiers. A wrong fact gets fixed everywhere, but the precision goes on the safety and How it works pages, which those pages link to.

Nearly every review found an overclaim on a precise page. The docs of an IPv6 fix said that every lookup in a confined VM with transparent mode off waits 35 to 40 seconds on a Linux host, from one tool's behavior. An end-to-end run on Fedora showed `getent` failing within 3 seconds and curl at once, and only workerd under wrangler dev waiting about 40 seconds.

Wrong, from one case:

```markdown
every lookup waits 35 to 40 seconds on a Linux host
```

Right, as the measurement showed:

```markdown
most tools fail at once on macOS and Linux alike, and a few, such as workerd under wrangler dev, can wait about 40 seconds on a Linux host
```

**Guard:** `TestCommandLines` checks every devvm command line in the repo, `TestDocLinks` the messages' doc links, lychee the docs' links and `TestReferenceIsCurrent` the CLI reference, and the docs-voice skill's `facts.sh` checks that a rewrite kept every fact. Whether a claim is true is review only.

## Prove each fix with a test that fails without it

A fix lands with a test that fails without it: revert the fix, or break the code it guards, watch the test fail, then put the fix back. Code that has no test gets one that pins its current behavior before it changes. When a change breaks an assertion, either the change is wrong or the commit says why the assertion changed, and an assertion is never loosened to make a test pass.

- **The oracle comes from outside the code under test:** the specification, a frozen copy of the old behavior, a list written by hand from the source, or the real program, never the code's own table or logic, which agrees with any bug.
- **An absence check has a control** that shows the probe can find what it looks for, such as the guest's own environment holding the token that `ps` must not show.
- **A security boundary is proven by attacking it.** A change to a sandbox, such as the proxy child's Seatbelt profile, its Landlock rules or its seccomp filter, is tested by a process under the real sandbox that tries what an attacker would, and other ways to the same thing, rather than by reading the rules back. The change also gets a threat check with the security-audit skill.

The reviews found tests that still passed with their fix reverted, expected values taken from the function under test, and absence checks that couldn't have found anything. The Landlock filter took three rounds of review, because each attack found a call the filter allowed, such as `prlimit64` setting another process's CPU limit.

Wrong, an oracle that is the code's own list:

```go
if got := SSHPorts(); !slices.Equal(got, sshPorts) {
```

Right, as `TestSSHPorts` builds its own from the Fetch standard's list and Chromium's gaps:

```go
want := slices.DeleteFunc(slices.Clone(fetch), func(n int) bool { return n < 1024 || slices.Contains(chromiumGaps, n) })
if got := SSHPorts(); !slices.Equal(got, want) || len(got) != 17 {
```

**Guard:** the coverage baselines fail `mise run test` when a package's coverage falls ([Coverage](../../../docs/content/docs/contributing/tests.md#coverage)), and `TestSelf` and `TestChildProfileConfines` attack the proxy child's sandboxes. That a test fails without its fix, and where its oracle comes from, are review only.

## Write tests that pass on any host, at any load

A test passes on a loaded Linux CI runner, on a Mac in another time zone, and under `git rebase --exec`, as it does on your machine:

- **Off the real clock.** A test of a timeout, a grace or a close runs in a `testing/synctest` bubble on stand-ins in memory ([Time and lifecycles](../../../docs/content/docs/contributing/tests.md#time-and-lifecycles)). A test that must use the real clock waits for the event, an absence included, rather than for a duration, and fails at once when what it waits on has exited.
- **No assumptions about the host.** A test brings its own git config (`stubtest.Git`), time zone, ports from its sandbox's block and temp folders, and it doesn't assume Linux or macOS, the number of CPUs, or GNU or BSD tools.
- **Stubs behave like the real tool.** A stub or fake is held to the real tool by a contract test or by golden output from the real tool, as the backend conformance suite holds the fake backend, the limactl stub and real Lima to one set of cases.
- **Stress what's timing-sensitive.** A test whose result depends on timing or order passes `go test -race -count=20` on a loaded Linux host before it counts as done, and a test that opens many connections has a cap and runs on a test host rather than your own machine.

Tests that assumed their host failed about 25 times, on a machine's git-lfs or system git config, a time zone, a BSD tool's flags or a CPU count. The broker's start tests failed on loaded Linux hosts, because hashing the test binary took most of the 4 seconds a start waits, and a reviewer's flood test used up a Mac's local ports and broke every ssh to the test hosts.

Wrong, sleeping out a grace on the real clock with a margin:

```go
time.Sleep(grace + time.Second)
if !fired.Load() {
```

Right, as `TestGraceTimer` checks the moment itself:

```go
synctest.Test(t, func(t *testing.T) {
    var fired atomic.Bool
    g := newGraceTimer(grace, func() { fired.Store(true) })
    tc.steps(g)
    time.Sleep(grace - time.Nanosecond)
    synctest.Wait()
    if fired.Load() {
```

**Guard:** the structure test checks that a test runs git only through `stubtest.Git`, CI runs the tests in UTC on macOS and with `TZ=America/Los_Angeles` on Linux ([Any time zone](../../../docs/content/docs/contributing/testing.md#any-time-zone)), each CLI test's sandbox holds a block of ports, and `TestConformance` runs the backend conformance suite on the limactl stub and on real Lima's instances, while `TestConformanceFinds` checks that the suite fails a backend that breaks it. A real-clock test, the stress run and a flood's cap are review only.
