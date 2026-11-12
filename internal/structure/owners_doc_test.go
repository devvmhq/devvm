package structure

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
)

// update rewrites Contributing's table of the checks on owners from
// ownerDocs: go test ./internal/structure -run TestOwnersDoc -update.
var update = flag.Bool("update", false, "rewrite Contributing's checks on owners from ownerDocs")

// The page that holds the table, and the lines around it.
const (
	ownersPage  = root + "/docs/content/docs/contributing/structure.md"
	ownersBegin = "<!-- The table is generated from ownerDocs in internal/structure/owners_doc_test.go: go test ./internal/structure -run TestOwnersDoc -update -->"
	ownersEnd   = "<!-- end of the generated table -->"
)

// An ownerDoc is one owner's check, as Contributing's table says it: what
// checks it, the owner and why it is the only one, and where else the code
// may be.
type ownerDoc struct {
	check, owner, elsewhere string
}

// ownerDocs are the checks on owners, forbidigo's and the structure
// test's, in the order the table has them. The structure test's own are
// devvm()'s tables, and TestOwnersAgree checks that each owner they name is
// in one of these.
func ownerDocs() []ownerDoc {
	return []ownerDoc{
		{
			check:     "forbidigo, `testbuild.On`",
			owner:     "`internal/cli`, whose `readOverrides` is the only code that asks whether this is a test build",
			elsewhere: "nowhere: other packages take the overrides as fields",
		},
		{
			check:     "forbidigo, `os.FindProcess`, `syscall.Kill`, `unix.Kill` and `os.Process`'s `Kill` and `Signal`",
			owner:     "`internal/daemon`, which signals a background process only while its pid file and its arguments say it's still that process (`Proc`), or through the process it started (`Handle`)",
			elsewhere: "`internal/platform`; tests, which signal the children they started; and a child that hasn't been waited for yet, whose pid is still its own, with a `nolint` saying so",
		},
		{
			check:     "forbidigo, `unicode.IsGraphic`, `IsPrint`, `IsControl` and `Bidi_Control`",
			owner:     "`msg.Safe` in `internal/msg/safe.go`, the one rule for which characters reach the terminal, which `msg.Printable`, `msg.OneLine`, the UI's renderers and devvm agent save's diff follow, so a second rule can't let through what this one refuses ([Colors](cli.md#colors) says what it refuses)",
			elsewhere: "a check that isn't about the terminal, such as a path one line long, with a `//nolint:forbidigo` saying so",
		},
		{
			check:     "forbidigo, HTTP clients",
			owner:     "`internal/fetch`, whose `NewClient` sets each client's timeout and redirect policy (`HTTPSOnly` or `SameScheme`), and whose `Retry`, `Transient`, `ReadCapped`, `Get`, `Resolve` and `Download` try again after the failures that may pass, cap what a body or a download may hold, follow a HEAD's redirects to where they end and check a download's SHA-256 before anything uses it. `fetch.Client` is its own type, so a client that `NewClient` didn't make, such as `&fetch.Client{}`, fails every request, and a caller changes a client's transport through its `Transport` method",
			elsewhere: "`internal/proxy`, which is the standard library only, and tests, which make their own servers' clients",
		},
		{
			check:     "forbidigo, `os.WriteFile`, `os.Create` and `os.O_TRUNC`",
			owner:     "`internal/state`, whose `WriteFile` and `WriteFileMode` write a file whole through a temporary file in its folder, renamed over it, so a reader never sees half of it",
			elsewhere: "`internal/proxy`, which imports only the standard library and has its own writer, `writeAtomic`; `internal/msg/gen`, which imports no devvm package; `internal/stubtest`; tests; and, with a `nolint` saying why, a file written another way: a log that a process writes as it runs, a download that streams into its file, and `internal/daemon`'s pid and status files, each written in one write that a reader takes as missing until its line ends",
		},
		{
			check:     "forbidigo, `os.Open`, `os.OpenFile`, `os.ReadFile`, `os.OpenRoot`, `os.OpenInRoot`, `os.DirFS` and `os.Root`'s `Open`, `OpenFile`, `ReadFile` and `FS`, in the code that reads files a VM can write or swap: `internal/gitprotect`, `internal/agentlogin`, `internal/vm`'s `devcontainer.go`, `detect.go`, `paste.go` and `agentsave*.go`, `internal/netpolicy/rules.go`, and `internal/proxy`'s `config.go` and `policy.go`",
			owner:     "`internal/safefile`, which imports only the standard library, so the proxy reads with it too. Its `Read` and `Open` open a file without blocking and, unless the caller passes `Follow`, without following a symbolic link at the path's end. They then check the file they opened, not its name, so a FIFO, a device or a folder is refused with `ErrNotRegular`, a file past its cap with `ErrTooLong`, and, with `OneLink`, a file with another hard link with `ErrLinked`. A FIFO that the VM swaps in after a check can't hold devvm, and a link to `/dev/zero` can't flood it. `Checked` checks a file its caller opened another way in the same way, such as devvm paste's copied file, which `platform.OpenNoLinks` opens following no link but the system's, through `openat`, which the standard library doesn't have on macOS. `OpenRoot` opens beneath a folder through `os.Root`, never through a link out of it, and its `FS` lists and stats what's there but opens only regular files",
			elsewhere: "every other file, which is devvm's own or the user's; tests, which make their own fixtures; and, with a `nolint` saying why, what opens a file to write it or a folder to lock it",
		},
		{
			check:     "forbidigo, `os.Setenv`, `os.Unsetenv`, `os.Clearenv` and `syscall`'s",
			owner:     "each command's own environment, its `exec.Cmd`'s `Env`, because every command and background process devvm starts inherits devvm's. A VM's SSH agent is one: the finder's `target` carries its socket, which goes to the backend in `backend.ExecOpts.Agent`, `backend.ShellOpts.Agent` and `Confinement.AgentSocket`, and Lima sets `SSH_AUTH_SOCK` on that VM's `limactl` commands alone, so a run that reaches several VMs never gives one another's",
			elsewhere: "`daemon.Ready` in `internal/daemon/status.go`, which takes the status file's name out of a background process's own environment, and tests, which use `t.Setenv`",
		},
		{
			check:     "the structure test's `owners`, host commands",
			owner:     "the runners: the UI's `LoggedCmd`, which a package takes as a field and which runs a command into the run log, a backend's `Runner`, and `internal/hostgit`. A host command that one of them doesn't run is made only in the files `execSites` lists: those that make the command a runner then runs, such as Lima's `limactl` and mise's, and those whose command can't go into the run log, because it streams for as long as a link, the port forwarder or the desktop's relay lasts, asks on the terminal, outlives the run, or carries a secret",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, a backend's other interfaces",
			owner:     "`backend.As` in `internal/backend/guard.go`, because the registry's backends are behind its guard, whose type has only `backend.Backend`'s methods, so a type assertion to `backend.Forgetter` or `*lima.Lima` on one is always false",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, git",
			owner:     "`internal/hostgit`, which runs git without the repo's fsmonitor, hooks or filters, over ssh only, without the environment's `GIT_*` variables and prompts",
			elsewhere: "tests, which run git to make their repos",
		},
		{
			check:     "forbidigo, `os.UserHomeDir`, and the structure test's `owners`, `HOME`",
			owner:     "`paths.FromEnv` in `internal/paths/dirs.go`, which reads `HOME` once into `paths.Dirs.Home`, so a test or an e2e run that sets its own `HOME` moves every path devvm takes from it",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, `DEVVM_` variables",
			owner:     "`readOverrides` in `internal/cli/overrides.go`, which reads a test build's stand-ins, while a release reads none",
			elsewhere: "the settings in `envSettings`, and tests",
		},
		{
			check:     "the structure test's `owners`, shell quoting",
			owner:     "`internal/shell`, whose `Quote` and `QuoteJoin` quote words for a POSIX shell, so a value never runs as a command",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, the VM-name grammar",
			owner:     "`internal/vmname`, whose `IsVM` tells a project VM's name from an image's, so gc never takes a project VM for an image",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, the coding agents",
			owner:     "`internal/agents`, whose `Defs` hold every fact devvm keeps of each agent, so no copy of an agent's list or paths goes stale when one is added. A string literal that is an agent's name or a login file's kind, whole, or that holds an agent's folder below the home folder, fails, and code of one agent's own names the agent by its constant there ([adding a coding agent](extending.md#adding-a-coding-agent))",
			elsewhere: "`internal/stubtest`, whose stubs are named after the commands they stand in for, and tests",
		},
		{
			check:     "the structure test's `owners`, the toolchains",
			owner:     "`internal/image`, whose `SharedTools` holds every fact devvm keeps of each toolchain: its members, the files that call for it, its installer, its guest environment, its `PATH` folders and its version files. A string literal that is a toolchain's name or a member's, whole, fails, so code elsewhere asks the table, and a new toolchain is an entry there ([adding a toolchain](extending.md#adding-a-toolchain))",
			elsewhere: "`internal/stubtest`, and tests",
		},
		{
			check:     "the structure test's `owners`, image names",
			owner:     "`internal/image`, whose `Name` is an image's name and splits it into its layers (`Layers`, `Parent`, `Has`), whose `NameOf` joins layers into one, and whose `ParseName` reads the name the user gives, which `build.Names`' `Parse` checks against the layers there are. `devvm build`, `devvm rebuild` and `devvm images rm` read their names there once, and the rest of devvm passes the `image.Name` on",
			elsewhere: "tests",
		},
		{
			check:     "the structure test's `owners`, `state.ReadHash` and `project.Parents`",
			owner:     "the finder in `internal/vm/finder.go`, whose `target` finds the VM every command acts on: the folder's own, the nearest parent folder's under `--here` and `--parent` (`resolveFolder`), or one named (`devvm rm VM`). It finds a folder's VM by the state that records the folder (`finder.state`, by the path's hash first, then `recorded`), offers to move a VM whose folder moved there (`moveHere` in `internal/vm/move.go`, the match `here.movedFrom`), and names a new VM by the first of the folder's names whose VM folder is free (`newName`, `project.Nth`). Its `need` says what the command needs of the VM, and `need.check` is the one place that refuses one that isn't there, one made for an earlier folder at its path, one whose remake didn't finish and one whose creation didn't, the last with `refuseUnfinished`, which every start (`starter.bringUp` in `internal/vm/start.go`) makes too. It reads the folder devvm runs in only then, so the commands that act on no folder's VM run in a folder that's gone. The VM it finds is a `target` (`internal/vm/target.go`). It holds what the VM's backend told the finder, which each start and stop of the VM then updates, and it runs what a command does with that VM: `exec`, `shell`, `stop`, `running`, `ownSSH` and the name of its ssh config entry",
			elsewhere: "`internal/state`, and tests",
		},
		{
			check:     "the structure test's `owners`, `hostsafety.Walk`",
			owner:     "the start in `internal/vm/start.go`, whose `starter.walk` walks a VM's project folder once for each check of the folder on the host, for the git repos nested in it, which `.git` protection goes by (`gitprotect.Protector.Nested`), and on Linux hosts its set-id files, so a check never walks the folder twice. It walks when a VM is made and when a check finds it stopped, at a launch or at `devvm stop` (`starter.hostChecks`), and at `devvm git-protect` on a stopped VM, never at a launch into a running VM",
			elsewhere: "`internal/hostsafety`, and tests",
		},
		{
			check:     "the structure test's `owners`, `state.ReadAll`",
			owner:     "the inventory in `internal/vm/inventory.go`, which reads every VM's state (`ReadStates`), and with `ReadInventory` the backends' lists too, each within its wait, for every listing of VMs: `devvm ls`, `devvm space`, `devvm images` and `devvm gc`, the ssh config, `devvm gc --orphans`, the links, `devvm secret`'s list, finding a VM by its name and the mounts that devvm's SSH agent checks a key against. Its one failure policy is that a backend that can't list fails a listing (`Inventory.Err`) only when some VM's state names it, and that a state that can't be read is a row of its own, which the readers leave out and `ToolsInUse` and `Mounts` fail on. `state.VMNames`, which fails on such a state, stays for the readers below `internal/vm`: `netpolicy`, `ports` and `sshconfig.ChooseAlias`",
			elsewhere: "`internal/state`, and tests",
		},
		{
			check:     "the structure test's `owners`, `paths.Dirs.AgentSettings` and `paths.Dirs.AgentLogins`",
			owner:     "devvm's agent store: `agentStore` in `internal/vm/agents.go` for the agents' settings, which the mounts, the setup's copies, a remake's home and `devvm agent import` take, and `secrets.LoginFile` in `internal/secrets/secrets.go` for their logins, which the setup's copies and the proxy read, so nothing reads an agent's files from the home folder but `vm.AgentImport`",
			elsewhere: "`internal/paths`, and tests",
		},
		{
			check:     "the structure test's `owners`, paths in devvm's folders",
			owner:     "`internal/paths`, whose `Dirs` and `VM` name every file and folder devvm keeps, so the layout, its golden listing in `testdata/script/layout.txtar` and a VM's teardown miss none. A `filepath.Join` onto a `Data` or `Config` field fails, and so does an `os.MkdirTemp` or `os.CreateTemp` there whose name is a literal; a name from `paths`, such as `paths.ToolsBuildPrefix`, passes",
			elsewhere: "tests",
		},
	}
}

// ownersTable is the table ownerDocs make, in Markdown.
func ownersTable(docs []ownerDoc) string {
	var b strings.Builder
	b.WriteString("| Check | Owner | Allowed elsewhere |\n| --- | --- | --- |\n")
	for _, d := range docs {
		b.WriteString("| " + d.check + " | " + d.owner + " | " + d.elsewhere + " |\n")
	}
	return b.String()
}

// Contributing's table of the checks on owners is ownerDocs'.
func TestOwnersDoc(t *testing.T) {
	page, err := os.ReadFile(ownersPage)
	if err != nil {
		t.Fatal(err)
	}
	before, rest, ok := strings.Cut(string(page), ownersBegin+"\n\n")
	table, after, ok2 := strings.Cut(rest, "\n"+ownersEnd)
	if !ok || !ok2 {
		t.Fatalf("%s: no %q and %q around the owners' table", ownersPage, ownersBegin, ownersEnd)
	}
	want := ownersTable(ownerDocs())
	if table+"\n" == want {
		return
	}
	if !*update {
		t.Fatalf("%s: the owners' table isn't ownerDocs': run go test ./internal/structure -run TestOwnersDoc -update", ownersPage)
	}
	if err := os.WriteFile(ownersPage, []byte(before+ownersBegin+"\n\n"+want+ownersEnd+after), 0o644); err != nil { //nolint:gosec // a docs page, read by all
		t.Fatal(err)
	}
}

// Every owner devvm()'s tables name has its row in ownerDocs, so the table
// lists each check the structure test makes.
func TestOwnersAgree(t *testing.T) {
	r := devvm()
	owners := []string{r.guardOwner, r.envOwner, r.pathsOwner}
	for _, m := range []map[string]string{r.commands, r.envOwners, r.callers} {
		for _, o := range m {
			owners = append(owners, o)
		}
	}
	for _, m := range []map[string]literalOwner{r.literals, r.names, r.separators} {
		for _, o := range m {
			owners = append(owners, o.dir)
		}
	}
	docs := ownerDocs()
	for _, o := range owners {
		if !slices.ContainsFunc(docs, func(d ownerDoc) bool { return strings.Contains(d.owner, "`"+o+"`") }) {
			t.Errorf("%s owns a structure test's check, but no ownerDocs row names it as an owner", o)
		}
	}
}

// An agent's paths below the home folder are its settings folder, with a
// slash when it's one folder's name, and its history outside that folder.
func TestAgentHomePaths(t *testing.T) {
	defs := []agents.Def{
		{Folder: ".a", History: []string{".a", ".a.json"}},
		{Folder: ".config/b", History: []string{".config/b/sessions", ".local/state/b"}},
	}
	got := agentHomePaths(defs)
	want := []string{".a/", ".a.json", ".config/b", ".local/state/b"}
	if !slices.Equal(got, want) {
		t.Errorf("agentHomePaths = %q, want %q", got, want)
	}
}
