// Package structure checks the code's shape: how many methods a type has,
// that no func init runs, which package-level vars there are, and which
// packages import which. It has only tests.
package structure

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
)

// root is the repo's root, from this package's folder.
const root = "../.."

// Why a var in allowedVars is constants' kin.
const (
	ldflags = "set by -ldflags at build time"
	table   = "a fixed table: set here, only read"
	fixture = "a test's fixed input: set here, only read"
	derived = "a fixed table's view, worked out once from it, only read"
)

// devvm is what the repo is held to.
func devvm() rules {
	return rules{
		// It keeps a type to one job, an interface too: one that grows past
		// it splits by what its methods do, as vm's Launch, Lifecycle,
		// Settings and Net, and backend.Backend's roles, which Lima's and the
		// fake's own types each implement.
		maxMethods: 24,
		largeTypes: map[string]large{
			"internal/paths.Dirs": {63, "one method per folder or file devvm uses: a list (a VM's own files are paths.VM's)"},
			"internal/ui.UI":      {47, "the one terminal: messages and where they go, as text or names, the logo, errors with the caller's exit code, prompts and their refusals, whether the picker shows, status line, tables, run log and how many are kept, cleanups, signals and what waits for them"},
		},
		allowedVars: devvmVars(),
		// hostgit runs git without fsmonitor, hooks, filters, GIT_* or prompts, ssh only; tests, stubtest.Git (GitEnv).
		commands:     map[string]string{"git": "internal/hostgit"},
		testCommands: map[string]string{"git": "internal/stubtest"},
		execSites:    devvmExecSites(), goroutineSites: devvmGoroutineSites(), unboundedReads: devvmUnboundedReads(), folderIDOwner: "internal/platform", exactFolderIDs: devvmExactFolderIDs(), statusSites: devvmStatusSites(), statusFree: devvmStatusFree(),
		// The registry's backends are behind its guard: only backend.As
		// finds their other interfaces.
		guardOwner: "internal/backend/guard.go",
		literals:   devvmLiterals(), every: everyOwner{agents.Names(), "internal/agents", "agents.Names()"},
		names: devvmNames(), nameFree: devvmNameFree(), switches: devvmSwitches(), switchFree: devvmSwitchFree(),
		namesFree: []string{"internal/stubtest"}, // the stubs, named after the commands they stand in for
		// An image's name is read once, by internal/image: its layers are
		// split and joined there alone.
		separators: map[string]literalOwner{"+": {"internal/image", "image.Name"}},
		// paths names every file and folder devvm keeps, so the layout,
		// its golden listing and a VM's teardown miss none.
		pathsOwner: "internal/paths",
		// The finder alone reads a folder's VM by its path's hash and looks
		// through its parent folders, so every command finds the same VM
		// and refuses what it needs refused.
		// The inventory alone reads every VM's state, so every listing
		// shares its one failure policy.
		// devvm's agent store alone holds what VMs get of the agents:
		// agentStore its settings, secrets.LoginFile its logins, so
		// nothing reads them from the home folder.
		// The start alone walks a project folder, once per check.
		callers: map[string]string{
			"internal/hostsafety.Walk":          "internal/vm/start.go",
			"internal/state.ReadHash":           "internal/vm/finder.go",
			"internal/project.Parents":          "internal/vm/finder.go",
			"internal/state.ReadAll":            "internal/vm/inventory.go",
			"internal/paths.Dirs.AgentSettings": "internal/vm/agents.go",
			"internal/paths.Dirs.AgentLogins":   "internal/secrets/secrets.go",
		},
		// readOverrides reads a test build's stand-ins, and a release
		// none.
		envOwner: "internal/cli/overrides.go", envSettings: devvmEnvSettings(),
		// paths.Dirs.Home is the home folder, read once: tests set HOME,
		// and e2e gives a run a HOME of its own.
		envOwners: map[string]string{"HOME": "internal/paths/dirs.go"},
		vmNames:   devvmVMNames(), hosts: devvmHosts(), secretOwner: "internal/redact",
		layers: []layer{
			{
				name: "only cmd/devvm and internal/cli hold the UI",
				from: []string{"..."}, exceptFrom: []string{"cmd/devvm", "internal/cli/...", "internal/ui"},
				to: []string{"internal/ui"},
			},
			{
				name: "only internal/cli runs the commands on VMs",
				from: []string{"..."}, exceptFrom: []string{"internal/cli/...", "internal/vm"},
				to: []string{"internal/vm"},
			},
			{
				name: "only cmd/devvm runs the commands",
				from: []string{"..."}, exceptFrom: []string{"cmd/devvm", "internal/cli/..."},
				to: []string{"internal/cli/..."},
			},
			{
				// vmssh's port forwarder and link sessions are devvm's,
				// on every backend: none runs its own.
				name: "backends are below what uses them",
				from: []string{"internal/backend/..."},
				to: []string{
					"internal/vm", "internal/build", "internal/tools", "internal/netpolicy", "internal/secrets",
					"internal/project", "internal/gitprotect", "internal/hostsafety", "internal/sshconfig", "internal/sshagent", "internal/trust",
					"internal/vmssh",
				},
			},
			{
				name: "only the registry and internal/cli name a backend",
				from: []string{"..."}, exceptFrom: []string{
					"internal/cli/...", "internal/backend/registry", "internal/backend/lima",
				},
				to: []string{"internal/backend/lima"},
			},
			{
				name: "backendtest is for tests",
				from: []string{"..."}, exceptFrom: []string{"internal/backend/backendtest"},
				to: []string{"internal/backend/backendtest"},
			},
			{
				name: "upgradetest is for tests",
				from: []string{"..."}, exceptFrom: []string{"internal/upgrade/upgradetest"},
				to: []string{"internal/upgrade/upgradetest"},
			},
			{
				name: "stubtest, leaktest and memnet are for tests",
				from: []string{"..."}, exceptFrom: []string{"internal/stubtest/...", "internal/leaktest", "internal/memnet"},
				to: []string{"internal/stubtest/...", "internal/leaktest", "internal/memnet"},
			},
			{
				name: "internal/stubtest imports only internal/msg and its own",
				from: []string{"internal/stubtest/..."}, exceptTo: []string{"internal/msg", "internal/stubtest/..."},
				to: []string{"..."},
			},
			{
				// The stubs' binary is built apart from devvm: it can't
				// share devvm's code, only stand in for the tools it runs.
				name: "the stubs' binary and its state import nothing of devvm's",
				from: []string{"internal/stubtest/stub", "internal/stubtest/stubstate"}, exceptTo: []string{"internal/stubtest/stubstate"},
				to: []string{"..."},
			},
			{
				name: "internal/proxy is standard library only, with internal/daemon, internal/landlock, internal/redact, internal/safefile and internal/vmname",
				from: []string{"internal/proxy/..."}, exceptTo: []string{"internal/proxy/...", "internal/daemon", "internal/landlock", "internal/redact", "internal/safefile", "internal/vmname"},
				to: []string{"..."},
			},
			{
				name: "leaves import nothing of devvm's",
				from: []string{
					"assets", "locales", "packaging", "internal/platform", "internal/msg/gen", "internal/testbuild", "internal/hostgit",
					"internal/shell", "internal/vmname", "internal/daemon", "internal/fetch", "internal/jsonc",
					"internal/redact", "internal/agents", "internal/safefile", "internal/landlock", "internal/leaktest", "internal/memnet",
				},
				to: []string{"..."},
			},
			{
				// Agents' logins hold their tokens: the package that reads,
				// refreshes and writes them knows nothing of VMs or the UI.
				name: "internal/agentlogin imports only internal/agents, internal/fetch, internal/jsonc, internal/platform, internal/proxy, internal/redact, internal/safefile and internal/state",
				from: []string{"internal/agentlogin"}, exceptTo: []string{"internal/agents", "internal/fetch", "internal/jsonc", "internal/platform", "internal/proxy", "internal/redact", "internal/safefile", "internal/state"},
				to: []string{"..."},
			},
			{
				name: "internal/msg imports only locales and internal/vmname",
				from: []string{"internal/msg"}, exceptTo: []string{"locales", "internal/vmname"},
				to: []string{"..."},
			},
			{
				name: "internal/paths imports only internal/msg and internal/vmname",
				from: []string{"internal/paths"}, exceptTo: []string{"internal/msg", "internal/vmname"},
				to: []string{"..."},
			},
			{
				name: "internal/state imports only internal/msg, internal/paths, internal/platform and internal/vmname",
				from: []string{"internal/state"}, exceptTo: []string{"internal/msg", "internal/paths", "internal/platform", "internal/vmname"},
				to: []string{"..."},
			},
			{
				name: "internal/image imports only assets, internal/agents, internal/msg, internal/paths and internal/shell",
				from: []string{"internal/image"}, exceptTo: []string{"assets", "internal/agents", "internal/msg", "internal/paths", "internal/shell"},
				to: []string{"..."},
			},
			{
				name: "internal/devcontainer imports only internal/image, internal/jsonc, internal/msg and internal/shell",
				from: []string{"internal/devcontainer"}, exceptTo: []string{"internal/image", "internal/jsonc", "internal/msg", "internal/shell"},
				to: []string{"..."},
			},
		},
	}
}

// devvmVMNames is where a VM's name is a vmname.VM, parsed where it's
// read: never a string that a path, an alias or a folder's name could be
// passed for.
func devvmVMNames() vmNameRule {
	return vmNameRule{
		owner: "internal/vmname",
		converts: map[string]string{
			"internal/msg/msg.go": "msg.M.VM reads back the VM a message's function took as a vmname.VM",
		},
		free:     []string{"internal/stubtest"}, // the stubs stand in for limactl and ssh, whose arguments are strings
		notNames: map[string]string{"NewVMs": "backend.Registry's backend for new VMs, by the backend's name"},
	}
}

// devvmHosts is where a host is read in one form: proxy.NormHost, which
// the proxy, its rules and every other judge of a host read it with.
func devvmHosts() hostRule {
	return hostRule{
		owner: "internal/proxy/host.go",
		free: map[string]string{
			"internal/proxy/request.go:trimDots": "drops the trailing dots of a path's segments, as servers read /config.json. as /config.json: no host",
			"internal/vm/cloneseed.go:urlHost":   "a remote's ssh host, which ssh lowercases and keeps its trailing dot of, to find a VM's alias in the ssh config: no host a policy judges",
			// Trims that hold "." in a set of characters a whole string must be made of.
			"internal/backend/lima/start.go:machines.fillPassword": "checks the proxy's URL has only characters no YAML scalar treats specially: no host read",
			"internal/vm/cloneconfig.go:transportHelper":           "checks a transport helper's name has only a scheme's characters: no host read",
			"internal/vm/cloneworktree.go:safeIndexPath":           "drops a path segment's trailing dots and spaces, as Windows reads .git. as .git: no host",
		},
	}
}

// devvmVars are the package-level vars that are constants' kin, each with
// why.
func devvmVars() map[string]string {
	return map[string]string{
		"internal/agentlogin.opencodeOAuth":       table,
		"internal/agentlogin.codexKeep":           table,
		"internal/agentlogin.cursorKeep":          table,
		"internal/proxy.oauthTokenKeys":           table,
		"internal/proxy.cursorMinting":            table,
		"internal/proxy.cursorPersists":           table,
		"internal/proxy.cursorAccount":            table,
		"internal/agentlogin.cursorRoutes":        table,
		"internal/agentlogin.claudeAccountKeep":   table,
		"internal/agentlogin.claudeTopKeys":       table,
		"internal/agentlogin.claudeServerKeys":    table,
		"internal/agentlogin.openaiAuth":          fixture,
		"internal/agentlogin.withheldKeep":        table,
		"internal/agentlogin.piVMDropped":         table,
		"internal/cli.version":                    ldflags,
		"internal/testbuild.testBuild":            ldflags + ", by test builds only",
		"internal/backend.Features":               table,
		"internal/backend.Native":                 "a Support value, as a const would be",
		"internal/backend.None":                   "a Support value, as a const would be",
		"internal/backend/lima.sshLogin":          table,
		"internal/cli.shells":                     table,
		"internal/gitprotect.Modes":               table,
		"internal/gitprotect.reports":             table,
		"internal/image.SharedTools":              table,
		"internal/image.Toolchains":               derived,
		"internal/image.Agents":                   derived,
		"internal/image.Builtins":                 table,
		"internal/backend/lima.conversionScripts": table,
		"internal/msg.docs":                       table,
		"internal/msg/gen.taken":                  table,
		"internal/msg/gen.initialisms":            table,
		"internal/netpolicy.Env":                  table,
		"internal/netpolicy.directArgv":           table,
		"internal/netpolicy.proxyArgv":            table,
		"internal/vm.nixMarkers":                  table,
		"internal/proxy.notPublic":                table,
		"internal/proxy.withIPv4":                 table,
		"internal/proxy.githubMinting":            table,
		"internal/secrets.Known":                  table,
		"internal/agents.Defs":                    table,
		"internal/agents.Providers":               table,
		"internal/sshagent.Modes":                 table,
		"internal/tools.miseSHA256":               table,
		"internal/trust.Env":                      table,
		"internal/vm.opencodeDBs":                 table,
		"internal/vm.defaultKeys":                 table,
		"internal/shell.nasty":                    fixture,
		"internal/build.keyImages":                fixture,
		"internal/tools.gnuT":                     fixture,
		"internal/tools.muslT":                    fixture,
		"internal/structure.update":               "a test flag, set on go test's command line",
		"internal/leaktest.allowed":               table,
		"internal/state.held":                     "the state locks this process's goroutines hold, which test builds check the order of: the locks are the process's",
	}
}

// devvmEnvSettings are the DEVVM_ variables read outside the overrides,
// each with the files that read it and why.
func devvmEnvSettings() map[string]envSetting {
	return map[string]envSetting{
		"DEVVM_NO_TOOL_CHECK":    {[]string{"internal/tools/tools.go"}, "a user setting: no daily check for newer shared tools"},
		"DEVVM_NO_RELEASE_CHECK": {[]string{"internal/settings/release.go"}, "a user setting: no daily check for a newer devvm release"},
		"DEVVM_LIMA_HOME":        {[]string{"internal/paths/dirs.go"}, "a user setting: devvm's Lima home"},
		"DEVVM_SECRET_SERVICE":   {[]string{"internal/secrets/secrets.go"}, "a user setting: the Keychain service secrets are saved under"},
		"DEVVM_FROM":             {[]string{"internal/cli/flags.go"}, "a user setting: --from's default"},
		"DEVVM_MESSAGES":         {[]string{"internal/ui/style.go"}, "names mode, which tests and e2e read messages in: any build has it"},
		"DEVVM_GUESTFWD":         {[]string{"internal/backend/lima/qemu.go", "internal/stubtest/stub/limactl.go"}, "what devvm gives its own QEMU wrapper, devvm _qemu, which the limactl stub records for tests"},
		"DEVVM_QEMU_NET":         {[]string{"internal/backend/lima/qemu.go"}, "what devvm gives its own QEMU wrapper, devvm _qemu"},
		"DEVVM_QEMU_PINS":        {[]string{"internal/backend/lima/qemu.go", "internal/backend/lima/pins.go"}, "what devvm gives its own QEMU wrapper, devvm _qemu, and its namespace's pinned and keeper"},
		"DEVVM_DAEMON_STATUS":    {[]string{"internal/daemon/status.go"}, "what devvm gives its own background processes: where each says it's ready"},
	}
}

// devvmExecSites are the files that make a host command (exec.Command or
// exec.CommandContext), each with why. Elsewhere a command goes through
// one of their runners: the UI's LoggedCmd, which a package takes as a
// field, a backend's Runner, or hostgit.
func devvmExecSites() map[string]string {
	return map[string]string{
		"internal/hostgit/hostgit.go":            "the host git runner: git without the repo's fsmonitor, hooks or filters",
		"internal/backend/lima/limactl.go":       "limactl's command, which Lima's Runner runs into the run log",
		"internal/backend/lima/lima.go":          "qemu-img's command, which Lima's Runner runs",
		"internal/backend/lima/oci.go":           "docker save's command, which Lima's Runner runs",
		"internal/platform/pinrun_linux.go":      "devvm's own _qemu in QEMU's namespace on Linux: QEMU itself, its keeper and the probe, which outlive the run or are read",
		"internal/tools/mise.go":                 "mise's command, in its own folders, which the UI's LoggedCmd runs, or whose output is read",
		"internal/from/from.go":                  "docker's answers (an image's ID, whether it runs): output or exit code, nothing to log",
		"internal/sshagent/sshagent.go":          "OpenSSH's tools on devvm's agent socket, their output read or on the terminal",
		"internal/vm/sshagent.go":                "ssh-add on the terminal, which may ask for a passphrase",
		"internal/vm/desktop.go":                 "ssh -W to the VM's desktop socket: a stream for the viewer's relay",
		"internal/vm/env.go":                     "macOS's open, for the desktop's viewer",
		"internal/platform/folderstat_darwin.go": "diskutil's plist of a volume, whose output is read, which LoggedCmd would send to the run log",
		"internal/vm/agentimport.go":             "this machine's opencode: its version and auth export, whose output holds logins, so it's read, never logged",
		"internal/vmssh/forward.go":              "the port forwarder's ssh master: a stream that lasts as long as the forwarder",
		"internal/vmssh/links.go":                "links' ssh sessions, which last as long as the link, their output to its log",
		"internal/vmssh/relay.go":                "each forwarded or linked connection's ssh -W relay: a stream that lasts as long as the connection",
		"internal/platform/keychain_darwin.go":   "security and tmutil: secrets on stdin and stdout, never in a log",
		"internal/platform/clipboard_tool.go":    "devvm paste's clipboard tools: osascript, wl-paste and xclip, the clipboard's image and a copied file's path on stdout, read and never logged; pbcopy, wl-copy and xclip -i, the path on stdin, which on Linux serve the clipboard after the run ends",
		"internal/platform/folder_darwin.go":     "chflags, for .git protection",
		"internal/daemon/daemon.go":              "devvm's background processes, which outlive the run and its context",
		"internal/upgrade/upgrade.go":            "devvm upgrade: brew upgrade devvm through the UI's LoggedCmd, a new devvm's --version and ps's list, their output read",
		"internal/stubtest/build.go":             "go build of the test stubs, once for a test process, and their names from the binary",
		"internal/stubtest/git.go":               "tests' plain git, on their own repos, which they run and read as they like",
		"internal/stubtest/stub/process.go":      "the stubs' stand-in VM's own commands (git on its files, tar) and the real uname and sw_vers, run in the stub's place",
	}
}

// devvmGoroutineSites are the functions that start goroutines, each with
// what waits for them to end, or detached and why nothing does.
func devvmGoroutineSites() map[string]string {
	return map[string]string{
		"internal/backend/lima/buildproxy.go: buildProxy":   detached + "the build VM's logs' keeper (vmlog.KeepVM), which the build's done or the end of its context stops",
		"internal/vmlog/rotate.go: Keep":                    "Keep's wg.Wait, each log's look ending with ctx",
		"internal/backend/lima/packages.go: sendPackages":   "sendPackages, on sent, once the VM's tar has stopped reading and the pipe is closed",
		"internal/backend/lima/packages.go: takePackages":   "takePackages, on taken, once the VM's tar has ended and the pipe is closed",
		"internal/backend/lima/qemu.go: Connect":            detached + "QEMU's side to the connection, which ends as Connect closes it; devvm _qemu connect exits once Connect returns",
		"internal/cli/release.go: releaseNotice":            detached + "the release check's refresh, which the run waits for up to releaseWait and then leaves: the run's context and the fetch client's timeout bound it",
		"internal/daemon/daemon.go: Spawn":                  "the Handle's Done, closed once it has reaped the process, which ends it",
		"internal/desktop/relay.go: Relay.Serve":            "Serve's wg.Wait, for each connection's handle; the listener's closer ends with Serve's deferred cancel",
		"internal/desktop/relay.go: Relay.dialVM":           detached + "the VM's handshake, which ends once dialVM closes the VM's connection at a timeout or ctx's end, its result in a buffered channel",
		"internal/desktop/relay.go: Relay.carry":            detached + "the closer, which carry's deferred cancel ends, and the copy to the viewer, which ends once the closer closes both connections",
		"internal/gitprotect/repos.go: scanAll":             "scanAll's wg.Wait",
		"internal/memnet/memnet.go: Conn.wake":              detached + "a deadline's AfterFunc, which only wakes the connection's waiting ends, and which a later deadline or its close stops",
		"internal/memnet/memnet.go: Listener.SetDeadline":   detached + "a deadline's AfterFunc, which only wakes a waiting Accept, and which a later deadline or the listener's close stops",
		"internal/platform/pin_linux.go: WaitExit":          "the process's end, or WaitExit's stop, which closes the pidfd its wait in Go's poller is on",
		"internal/platform/pin_linux.go: WatchFolders":      "WatchFolders's stop, which closes the inotify file its read in Go's poller is on",
		"internal/platform/pinrun_linux.go: runChild":       "runChild, on passed, once it has closed the signals' channel",
		"internal/platform/pinrun_linux.go: RunPinned":      "keep, which returns the child's end that it sends",
		"internal/proxy/process.go: runBroker":              detached + "the lifeline's watch, which runs for the broker's life and ends it once the VM's hostagent is gone, the SSH agent relay's accept loop, which ends once runBroker's deferred close closes its listener, and the VM's logs' keeper (IO.KeepLogs), which runBroker's deferred stop ends",
		"internal/proxy/broker.go: broker.serveChild":       detached + "each of the child's requests' answer, which ends within its lookup's or token exchange's lookupTimeout, or its link's dial's, whatever happens to the pair",
		"internal/proxy/broker.go: broker.handshake":        detached + "the read of the child's answer, which ends once the socket does, as a start that fails closes it, its result in a buffered channel",
		"internal/proxy/broker.go: broker.live":             detached + "serveChild's read of the child's requests, which ends once the socket closes, as each of live's ends closes it, its result in a buffered channel",
		"internal/proxy/child.go: runChildOn":               "runChildOn, on ended, once the broker client's read has ended, which closing the socket ends",
		"internal/proxy/confine.go: within":                 detached + "the confinement, which within waits for up to its wait (confineWait): one that takes longer fails the child's start, and the child's process exits",
		"internal/proxy/agent.go: agentRelay.serve":         detached + "each connection's relay, which ends when either side closes, when the client sends no whole request within agentIdle, or with the proxy; nothing waits for one waiting on the agent's answer",
		"internal/proxy/splice.go: splice":                  "splice, on done, once both directions have ended",
		"internal/proxy/proxy.go: whileHeard":               "whileHeard's cancel, which handleHTTP defers, so it ends with the request's handler",
		"internal/proxy/conns.go: conns.close":              detached + "the handlers' wait, which close gives closeWait at most: it ends with the last handler, as the proxy exits",
		"internal/proxy/upstream.go: upstream.connect":      detached + "the AfterFunc that ends the handshake at ctx's end, which connect's deferred stop keeps from starting after it; one already started only sets a deadline",
		"internal/ui/exit.go: UI.main":                      "main, on done; after a signal, interrupt, for returnGrace at most when there are cleanups, then the run ends",
		"internal/ui/status.go: UI.startStatus":             "the status line's stop, on ended",
		"internal/vm/clonelfs.go: vmLFS.bring":              "bring, on done, once the VM's stream has ended and the pipe is closed",
		"internal/vm/cloneseed.go: seeder.sendTar":          detached + "the tar's writer, which ends at its next write once sendTar closes the pipe",
		"internal/vm/desktop.go: Desktop.serve":             detached + "the VM's check, which serve's deferred cancel ends",
		"internal/vm/desktop.go: sshChannel.Close":          "Close, which waits for ssh's Wait, killing it after channelClose",
		"internal/vm/env.go: folderWithin":                  detached + "a look at a folder whose disk may hang, such as an NFS share's, left to finish on its own after folderWait",
		"internal/vm/home.go: remaking.saveHome":            "saveHome, on copied, once the VM's tar has ended and the pipe is closed",
		"internal/vmssh/forward.go: Forwarder.Run":          detached + "the VM's watch and the ports' recheck, which Run's deferred cancel ends",
		"internal/vmssh/forward.go: Forwarder.open":         detached + "each listener's accept loop (serve), which ends as the forward's close closes the listener",
		"internal/vmssh/forward.go: Forwarder.serve":        detached + "each relay, which holds a slot of the forwarder's relays and ends with its context, which the forward's close cancels",
		"internal/vmssh/links.go: Links.Run":                detached + "the VM's watch, which Run's deferred cancel ends",
		"internal/vmssh/links.go: Links.master":             "the master's done, closed once ssh's wait returns, which the session waits on",
		"internal/vmssh/links.go: Links.add":                detached + "the link's accept loop (serve), which ends as its listener closes",
		"internal/vmssh/links.go: Links.serve":              "serve, on taken, once it has closed handoff",
		"internal/vmssh/links.go: Links.take":               detached + "each relay, which holds a slot of the session's conns and ends with its context, which its link's removal, its master's replacement and the session's end cancel",
		"internal/vmssh/links.go: Links.newTargetMaster":    "the master's ready, closed once it's up or has failed",
		"internal/vmssh/loopback.go: SSH.forwardLoopback":   "LoopbackForward.Close, through its wg; the listeners' closer ends with the forward's cancel",
		"internal/vmssh/loopback.go: LoopbackForward.serve": "serve's conns.Wait",
		"internal/vmssh/relay.go: sshRelay.carry":           detached + "the client's copy to the VM, which ends once the relay closes the client's connection; the AfterFunc that closes it at ctx's end, which the relay's deferred stop ends",
		"internal/vmssh/relay.go: newGraceTimer":            detached + "the grace's AfterFunc, which cancels the relay once the grace runs out; stopped as the relay ends, it never runs after it",
	}
}

// devvmUnboundedReads are the functions that read all of a reader with no
// limit, each with why its reader ends.
func devvmUnboundedReads() map[string]string {
	const stub = "the test stubs' standard input, which devvm under test writes and closes, in a stub run only by the CLI tests"
	return map[string]string{
		"internal/ui/status.go: tail":                      "the run's own log, which only this run writes, read from its last size bytes",
		"internal/stubtest/stub/guest.go: guest.logScript": stub,
		"internal/stubtest/stub/stub.go: call.copyIn":      stub,
		"internal/stubtest/stub/control.go: control":       stub,
	}
}

// devvmStatusFree are the folders whose code shows no status line, each
// with why.
func devvmStatusFree() map[string]string {
	return map[string]string{
		"internal/ui":        "the status line's own: it draws it, and its logged commands show their step",
		"internal/proxy":     "the proxy's processes, which run in the background with no terminal",
		"internal/landlock":  "the proxy's child's sandbox, in the background with no terminal",
		"internal/daemon":    "devvm's background processes, with no terminal",
		"internal/hostgit":   "the owner of host git: its callers' functions are the sites, as each of its calls is slow work",
		"internal/fetch":     "the owner of HTTP: its callers' functions are the sites, as each request is slow work",
		"internal/stubtest":  "the test stubs, which stand in for the commands devvm runs",
		"internal/structure": "the structure test",
		"cmd":                "the entry point, which runs the UI",
	}
}

// devvmStatusSites are the functions that do slow work and show no status
// line themselves, each with why: under a caller's status line, named by
// its message, on the terminal, in a process with no UI, or a command
// made for its caller to run.
func devvmStatusSites() map[string]string {
	const (
		made     = "makes the command, which its caller runs as a logged command's step or under its status line"
		launch   = "under a launch's or a stop's status lines (checking_vm), devvm git-protect's and devvm clone on's and off's (git_protecting), and a delete's, an orphan's included (deleting); chflagsCalls fails a lock with none"
		clone    = "a --clone VM's host git, under its caller's status line: a new --clone VM's and devvm clone on's look at its repos (clone_planning), a launch's setup (setting_up_vm), devvm clone add's (clone_reading, clone_adding), clone fetch's (clone_fetching), the check before a delete or a remake (clone_checking) and clone off's (git_protecting)"
		imported = "under devvm agent import's status lines (agent_import_reading, agent_store_saving)"
		saved    = "under devvm agent save's status lines (agent_save_comparing, agent_store_saving)"
		build    = "under a build's status line (checking_image), around its lookups and steps"
		release  = "under devvm upgrade's status lines (upgrade_checking, upgrade_downloading), or the daily release check, which a launch waits for a second at most"
		inVM     = "a command in the VM, under its caller's status line (TestLaunchStatusLines checks a launch's), or on the terminal with the streams its caller gives it"
		keychain = "one call to macOS's security tool, which answers at once; macOS shows its own dialog when it asks"
		noUI     = "a process that runs in the background, with no terminal: the forwarder, a link session or the relay"
		pinned   = "Linux's _qemu helper, in the process that runs QEMU in the background, or doctor's check of it (doctor_checking)"
		// clipboard is the clipboard's tool, for devvm paste and devvm
		// desktop's relay.
		clipboard = "the clipboard's tool (pbcopy, osascript, wl-paste, xclip), which answers at once; one that hangs is stopped after clipWait"
	)
	return map[string]string{
		"internal/backend/lima/limactl.go: Lima.limactl":          made,
		"internal/backend/lima/lima.go: qemuImg":                  made,
		"internal/tools/mise.go: Versions.miseCmd":                made,
		"internal/backend/lima/limactl.go: Lima.list":             "limactl list, under its callers' status lines: every Inspect, List, Images and SSH, which vm's commands call under theirs (finding_vm, checking_vm, reading_vms…; TestLaunchStatusLines checks a launch's)",
		"internal/backend/lima/lima.go: machines.Exec":            inVM,
		"internal/backend/lima/lima.go: Lima.diskInfo":            "qemu-img info, under its caller's status line: a new VM's (creating) and an image's delete (deleting)",
		"internal/backend/lima/remount.go: mountedCache":          "under the start's status line (starting), which runs its check of the cache mode",
		"internal/from/from.go: DockerID":                         build,
		"internal/from/from.go: DockerRunning":                    build,
		"internal/from/registry.go: Registry.token":               build,
		"internal/ubuntu/ubuntu.go: Finder.fetch":                 build,
		"internal/gitprotect/gitprotect.go: git":                  launch,
		"internal/gitprotect/gitprotect.go: hasHooks":             launch,
		"internal/gitprotect/own.go: ownObjects":                  launch,
		"internal/gitprotect/scan.go: tree.delegations":           launch,
		"internal/gitprotect/scan.go: tree.gitDirs":               launch,
		"internal/gitprotect/strict.go: converter.dropIgnore":     launch,
		"internal/gitprotect/watch.go: summer.hooks":              launch,
		"internal/hostsafety/hostsafety.go: Walk":                 launch,
		"internal/hostsafety/hostsafety.go: SSHRemote":            launch,
		"internal/platform/folder_darwin.go: Chflags":             launch,
		"internal/vm/start.go: starter.walk":                      launch,
		"internal/hostsafety/hostsafety.go: BareExplicit":         "under devvm doctor's status line (doctor_checking)",
		"internal/platform/clipboard_tool.go: copyWith":           clipboard,
		"internal/platform/clipboard_tool.go: runClipTool":        clipboard,
		"internal/platform/folderstat_darwin.go: VolumeUUID":      "Env.Volumes, which volumeOf runs under its status line (looking_up_volume)",
		"internal/platform/keychain_darwin.go: KeychainGet":       keychain,
		"internal/platform/keychain_darwin.go: KeychainSet":       keychain,
		"internal/platform/keychain_darwin.go: KeychainDelete":    keychain,
		"internal/ports/ports.go: Allocator.Build":                "locks a build port's record it has just written, a new file that no other process holds, so it never waits",
		"internal/platform/keychain_darwin.go: ExcludeFromBackup": "tmutil, under its caller's status line: an import's or a save's (agent_store_saving) and the proxy's start (checking_vm, starting)",
		"internal/platform/pinrun_linux.go: Probe":                pinned,
		"internal/platform/pinrun_linux.go: RunInNamespace":       pinned,
		"internal/platform/pinrun_linux.go: RunPinned":            pinned,
		"internal/platform/usage.go: DiskUsage":                   "under devvm space's, devvm images' and gc's status lines (reading_space, reading_images); devvm history's list of exports, one file each, is quick",
		"internal/sshagent/sshagent.go: run":                      "OpenSSH's tools for devvm's agent, whose ssh-add asks for a key's passphrase on the terminal: never under a status line",
		"internal/vm/sshagent.go: sshAdd":                         "ssh-add on the user's agent, which asks for a key's passphrase on the terminal: never under a status line",
		"internal/tools/dotnet.go: Versions.dotnetSDK":            "under withSDKDownloads' status line (resolving_tools)",
		"internal/upgrade/source.go: Source.feed":                 release,
		"internal/upgrade/source.go: Source.Fetch":                release,
		"internal/upgrade/source.go: Source.Latest":               release,
		"internal/upgrade/upgrade.go: runs":                       "under the upgrade's status line (upgrade_installing)",
		"internal/upgrade/upgrade.go: Upgrader.prune":             "under the upgrade's status line (upgrade_installing)",
		"internal/vm/agentimport.go: AgentImport.opencode":        imported,
		"internal/vm/agentimport.go: AgentImport.readSettings":    imported,
		"internal/vm/agentimport.go: pruneIn":                     imported + ", or devvm agent save's (agent_store_saving)",
		"internal/vm/agentlinks.go: follower.copy":                imported,
		"internal/vm/agentsave.go: storeFiles":                    saved,
		"internal/vm/clone.go: clones.gitSSH":                     clone,
		"internal/vm/clone.go: clones.saveFetched":                clone,
		"internal/vm/clone.go: hostGit":                           clone,
		"internal/vm/cloneconfig.go: gitBool":                     clone,
		"internal/vm/cloneconfig.go: listConfig":                  clone,
		"internal/vm/cloneconfig.go: listConfigFile":              clone + "; a marketplace's git config, " + imported + " and " + saved,
		"internal/vm/cloneconfig.go: repoHooksPath":               clone,
		"internal/vm/cloneconfig.go: seedConfig.readHooks":        clone,
		"internal/vm/cloneconfig.go: seedConfig.readHooksFolder":  clone,
		"internal/vm/clonelfs.go: lfsHistory":                     clone,
		"internal/vm/clonelfs.go: lfsPointers":                    clone,
		"internal/vm/clonepartial.go: hostPromisors":              clone,
		"internal/vm/clonesave.go: clones.sayUntracked":           clone,
		"internal/vm/clonesave.go: stashWithout":                  clone,
		"internal/vm/cloneseed.go: hostRefs":                      clone,
		"internal/vm/cloneseed.go: packCmd":                       clone,
		"internal/vm/clonestate.go: checkIgnored":                 clone,
		"internal/vm/clonestate.go: haveObjects":                  clone,
		"internal/vm/clonestate.go: holdsIgnored":                 clone,
		"internal/vm/clonestate.go: ignoredThen":                  clone,
		"internal/vm/clonestate.go: missingCommits":               clone,
		"internal/vm/clonesub.go: gitlinks":                       clone,
		"internal/vm/clonesub.go: hostSubmodules":                 clone,
		"internal/vm/cloneworktree.go: seedTree.git":              clone,
		"internal/vm/cloneworktree.go: seedTree.read":             clone,
		"internal/vm/sync.go: gitConfig":                          "under a launch's setup's status line (setting_up_vm)",
		"internal/vm/desktop.go: Desktop.dial":                    "devvm desktop's relay's ssh, which runs while the desktop is open, with no status line to show",
		"internal/vm/env.go: openURL":                             "opens a browser and returns, as the opener starts it apart",
		"internal/vmssh/forward.go: SSH.watch":                    noUI,
		"internal/vmssh/links.go: Links.commandSSH":               noUI,
		"internal/vmssh/links.go: Links.startSSH":                 noUI,
		"internal/vmssh/relay.go: execSSH":                        noUI,
	}
}

// devvmExactFolderIDs are the functions that compare a field named Folder
// or GitID with == or != outside platform.SameFolder, the one comparison
// of two folders' identities, as a boot can number an unchanged folder's
// device anew, each with why: a path that shares the name, or a record
// checked unchanged under its lock.
func devvmExactFolderIDs() map[string]string {
	return map[string]string{
		"internal/agents/agents.go: SettingsFormat": "an agent's folder in its home folder, a path, not a folder's identity",
		"internal/agents/agents.go: Withheld":       "an agent's folder in its home folder, a path, not a folder's identity",
		"internal/vm/exports.go: Exports.Newest":    "an export's project folder, a path, not a folder's identity",
		"internal/vm/move.go: recordMove":           "the state as it was read, unchanged under its lock, byte for byte",
		"internal/vm/move.go: finder.move":          "the folder read twice in one run, which no boot renumbered between",
		"internal/vm/start.go: folderChanged":       "after recordFolder recorded the folder as it is now in this run, the folder the backend mounts by the record, byte for byte",
	}
}

// devvmLiterals are devvm's literals: a second shell quoter can let a value
// run as a command in a VM or on this machine, a second copy of the
// VM-name grammar can read a project VM as an image, which gc deletes, and
// a second copy of an agent's folders below the home folder can leave its
// logins or history where its registry entry no longer looks.
func devvmLiterals() map[string]literalOwner {
	literals := map[string]literalOwner{
		`'\''`:         {"internal/shell", "shell.Quote"},
		`[0-9a-f]{14}`: {"internal/vmname", "vmname.IsVM"},
	}
	for _, p := range agentHomePaths(agents.Defs) {
		literals[p] = literalOwner{"internal/agents", "an agents.Def's paths"}
	}
	return literals
}

// agentHomePaths are the agents' paths below the home folder: each one's
// settings folder, with a slash when it's one folder's name (.claude/, not
// .claude.json), and what of its history is outside it.
func agentHomePaths(defs []agents.Def) []string {
	var paths []string
	for _, d := range defs {
		folder := d.Folder
		if !strings.Contains(folder, "/") {
			folder += "/"
		}
		paths = append(paths, folder)
		for _, h := range d.History {
			if h != d.Folder && !strings.HasPrefix(h, d.Folder+"/") {
				paths = append(paths, h)
			}
		}
	}
	return paths
}

// devvmNames are the coding agents' names and login kinds, which only
// internal/agents' literals are: its registry holds every agent, and code
// of one agent's own names it by its constant there, so a new agent is a
// Def and no copy of a list of them goes stale. The toolchains' names and
// their members' are internal/image's, whose SharedTools holds every
// toolchain, so a new toolchain is an entry there: code elsewhere asks it
// (image.Def, its Installer, its fields) rather than naming one. And git
// push is no command of devvm's: a --clone VM's seed goes through the
// backend's Exec (internal/vm's cloneseed.go), never git's transport,
// whose receive-pack refuses what a user's repo can be (a shallow one, a
// SHA-256 one), so only internal/hostgit, which runs every host git
// command, may name it. Nor may any other package name git diff-files,
// which enters submodules, whose own config can run a filter, but through
// hostgit.DiffFiles, which doesn't.
func devvmNames() map[string]literalOwner {
	names := map[string]literalOwner{
		"push": {"internal/hostgit", "no git push, and copy into a --clone VM through the backend's Exec, as internal/vm's cloneseed.go does"},
		// It reads each submodule's working tree, where that repo's own
		// config can name a filter git runs, unless --ignore-submodules.
		"diff-files": {"internal/hostgit", "hostgit.DiffFiles, which enters no submodule or embedded repo"},
	}
	for _, n := range slices.Concat(agents.Names(), agents.LoginKinds()) {
		names[n] = literalOwner{"internal/agents", "its constant in internal/agents"}
	}
	for _, d := range image.SharedTools {
		if d.Kind != image.Toolchain {
			continue
		}
		for _, m := range d.Members {
			names[m] = literalOwner{"internal/image", "its image.SharedTools entry"}
		}
	}
	return names
}

// devvmNameFree are the constants whose value may be an agent's name, for
// something else of that name, each used only where that something is:
// the Cursor editor's command, which devvm cursor runs and is named
// after, is the editor's, not its CLI agent's.
func devvmNameFree() map[string]freeName {
	return map[string]freeName{
		"internal/vm.CursorCommand": {"the Cursor editor's command, not its CLI agent's", []string{"internal/vm/ssh.go", "internal/cli/cursor.go"}},
	}
}

// devvmSwitches are the coding agents' names and login kinds, which only
// internal/agents, and the files named for the agent, compare with or
// choose by: a login kind's word is its agent's name (claude for
// claude-oauth). Everything else asks the agent's Def, so a new agent
// changes no other code; and no list but agents.Names holds every agent.
func devvmSwitches() map[string]switchOwner {
	out := map[string]switchOwner{}
	for _, d := range agents.Defs {
		for _, n := range slices.Concat([]string{d.Name}, agents.LoginKinds()) {
			if _, a, ok := agents.LoginOf(n); n == d.Name || ok && a.Name == d.Name {
				out[n] = switchOwner{
					"internal/agents", d.Name,
					"give its agents.Def a field the code asks, or put the code in a file named for " + d.Name,
				}
			}
		}
	}
	return out
}

// devvmSwitchFree are the constants named after an agent that a file
// outside the agent's own may compare with, each for a reason that isn't
// the agent's: the claude secret is a secret too, which the secrets'
// registry, devvm secret and a launch's warnings name as they do github;
// and Codex's ChatGPT login's binding is named after its agent, in Codex's
// format, which shares logins.go with opencode's.
func devvmSwitchFree() map[string]string {
	const secret = "the claude secret's own value check, prompt and warning, as github's"
	return map[string]string{
		"internal/secrets/secrets.go: internal/secrets.ClaudeToken": secret,
		"internal/cli/secret.go: internal/secrets.ClaudeToken":      secret,
		"internal/vm/secret.go: internal/secrets.ClaudeToken":       secret,
		"internal/agentlogin/logins.go: internal/agentlogin.codexLogin": "Codex's format's own binding, in the file of " +
			"the Codex and opencode formats",
	}
}

// The CLI tests' scripts list no agents as a message does: they take the
// registry's lists from the runner.
func TestScriptsListNoAgents(t *testing.T) {
	names, err := fs.Glob(os.DirFS(root), "testdata/script/*.txtar")
	if err != nil || len(names) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	for _, n := range names {
		data, err := fs.ReadFile(os.DirFS(root), n)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range scriptLists(n, string(data), agents.Names()) {
			t.Error(v)
		}
	}
}

// The repo keeps to its rules.
func TestStructure(t *testing.T) {
	m, err := load(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.files) == 0 {
		t.Fatal("no Go files found")
	}
	for _, v := range check(m, devvm()) {
		t.Error(v)
	}
}
