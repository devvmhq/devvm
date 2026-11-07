//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/tools"
)

// toolsSection: what a user gets, upstream's newest versions of the shared
// tools, each download with its checksum (rust: rustup checks), resolved
// by devvm's pinned mise, downloaded and checked on the first run. This
// section alone uses them: the pinned lock goes back at its end. A node
// VM mounts them read-only from the share, at boot too; a tool missing
// from the share stops the launch; devvm tools and devvm update; a .NET
// project's VM.
func toolsSection(s *S) {
	s.Section("shared tool versions")
	s.Check("the shared tools resolve to upstream's newest, with checksums", func() error {
		if _, err := s.Devvm("", "_refresh-tools"); err != nil {
			return err
		}
		return newestLock(s)
	})
	s.Section("shared tools")
	vm, t := toolsNode(s)
	toolsShare(s, vm, t)
	toolsMissing(s, vm, t)
	toolsCommands(s)
	toolsDotnet(s)
	if err := s.sb.pinLock(); err != nil {
		s.r.out.line("  e2e: couldn't put the pinned tool lock back")
	}
}

// newestLock fails unless the lock has every shared tool, each with a
// SHA-256 for this machine's Linux but rust, which rustup checks, and
// dotnet, which has the SHA-512 devvm added from Microsoft's releases.
func newestLock(s *S) error {
	toml, err := os.ReadFile(filepath.Join(s.sb.root, "assets", "shared-tools", "mise.toml"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.Paths.ToolLock)
	if err != nil {
		return err
	}
	// devvm writes the lock again as TOML of its own when it adds dotnet's
	// downloads, so it's read as TOML.
	lock, err := tools.ParseLock(data)
	if err != nil {
		return err
	}
	var names []string
	in := false
	for line := range strings.Lines(string(toml)) {
		line = strings.TrimSpace(line)
		switch {
		case line == "[tools]":
			in = true
		case strings.HasPrefix(line, "["):
			in = false
		case in && strings.Count(line, "=") == 1:
			name, _, _ := strings.Cut(line, "=")
			names = append(names, strings.TrimSpace(name))
		}
	}
	platform := "linux-" + tools.MiseArch(runtime.GOARCH)
	var bad []string
	for _, n := range names {
		_, sum := lock.Download(n, platform)
		want := `^sha256:[0-9a-f]{64}$`
		switch n {
		case "rust":
			want = `^$`
		case "dotnet":
			want = `^sha512:[0-9a-f]{128}$`
		}
		if !lock.Has(n) || !Matches(sum, want) {
			bad = append(bad, n)
		}
	}
	if len(names) == 0 || len(bad) > 0 {
		return fmt.Errorf("the lock lacks %q, or their checksums for %s; mise.toml has %q", bad, platform, names)
	}
	return nil
}

// toolsNode launches a node VM, a package.json's, and checks its tools: in
// its state, the share, the VM. It's the VM and its folder.
func toolsNode(s *S) (string, string) {
	// A package.json: node, detected. Its tool images, of the newest versions, stay cached in the share
	// until upstream moves or a later run's gc finds them unused.
	var vm string
	t, err := s.NewProject("tools")
	if err == nil {
		err = os.WriteFile(filepath.Join(t, "package.json"), []byte("{}\n"), 0o600)
	}
	if err == nil {
		vm, _, _ = s.Launch(t, "exit", "--network", "open", "--no-git-protect")
	}
	lockv := s.Paths.ToolVersion
	st := func() map[string]string { return s.Paths.State(vm) }
	files := func() []string { return strings.Fields(st()["tools.made.files"]) }
	s.Check("a node VM, with node's shared tools in its state and the share", func() error {
		switch {
		case vm == "" || s.Status(vm) != "Running":
			return errors.New("the node VM isn't running")
		case st()["image"] != "base" || st()["tools.names"] != "node" || st()["tools.made.libc"] != "gnu":
			return errors.New("its state has another image, tools or C library")
		}
		for _, f := range files() {
			b, err := os.ReadFile(filepath.Join(s.Paths.Share, f+".json"))
			if !Exists(filepath.Join(s.Paths.Share, f)) || err != nil || !HasLine(string(b), `  "backend": "lima",`) {
				return errors.New(f + " isn't in the share, a Lima tool image")
			}
		}
		if !Matches("tools.made.files="+st()["tools.made.files"], `^tools\.made\.files=node-`+regexp.QuoteMeta(lockv("node"))+`-.* pnpm-`+regexp.QuoteMeta(lockv("pnpm"))+`-.* bun-`+regexp.QuoteMeta(lockv("bun"))+`-`) {
			return errors.New("its tools aren't node's, pnpm's and bun's at the lock's versions")
		}
		return nil
	})
	s.Check("Ubuntu's base has no glibc version to check, and each tool image records the newest glibc it needs", func() error {
		if _, ok := st()["tools.made.glibc"]; ok {
			return errors.New("its state has a glibc version")
		}
		for _, f := range files() {
			b, _ := os.ReadFile(filepath.Join(s.Paths.Share, f+".json"))
			if !Matches(string(b), `^  "glibc": "[0-9]+\.[0-9]+",?$`) {
				return errors.New(f + " records no glibc")
			}
		}
		return nil
	})
	for _, f := range files() {
		b, _ := os.ReadFile(filepath.Join(s.Paths.Share, f+".json"))
		g := CaptureLine(string(b), `^  "glibc": "(.*)",?$`)
		if g == "" {
			g = "none"
		}
		s.r.out.line("        glibc: %s needs %s", f, g)
	}
	inVM := func(script string) func() error {
		return func() error { _, err := s.InVM(vm, script); return err }
	}
	s.Check("node, npm, pnpm, bun and yarn work, node at the locked version", inVM(
		"[[ $(node -v) == v"+lockv("node")+" ]] && npm -v && pnpm -v && bun -v && yarn -v"))
	s.Check("each tool is a read-only image from the share", inVM(
		"[[ $(findmnt -no FSTYPE /opt/devvm/tools/node/"+lockv("node")+") == erofs ]] && ! touch /opt/devvm/tools/node/"+lockv("node")+"/x 2>/dev/null"))
	s.Check("the share is read-only even for root", func() error {
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "sudo", "touch", "/mnt/devvm-tools/x")
		if !strings.Contains(r.Out, "Read-only file system") {
			return errors.New("root's touch in the share didn't fail as read-only")
		}
		return nil
	})
	s.Check("npm i -g installs into the VM's home", inVM(
		"npm i -g cowsay@1.6.0 >/dev/null 2>&1 && [[ $(command -v cowsay) == $HOME/.npm-global/bin/cowsay ]]"))
	return vm, t
}

// toolsShare: node trusts devvm's CAs, a non-login shell finds the tools,
// each runs from the share, an .nvmrc's version is its own, and the boot
// mounts them.
func toolsShare(s *S, vm, t string) {
	lockv := s.Paths.ToolVersion
	inVM := func(script string) func() error {
		return func() error { _, err := s.InVM(vm, script); return err }
	}
	// Node reads its own CA list, and NODE_EXTRA_CA_CERTS: a CA in devvm's CA folder, from the VM's
	// next launch, makes an HTTPS server here it signed trusted.
	port := strconv.Itoa(s.Port(47195))
	ca, err := NewCA(s)
	if err == nil {
		err = ca.Serve(s.Port(47195))
	}
	if err == nil {
		err = ca.Trust()
	}
	caErr := err
	if caErr == nil {
		_, _, _ = s.Launch(t, "exit")
	}
	s.Check("node trusts a CA in devvm's CA folder", func() error {
		if caErr != nil {
			return caErr
		}
		return inVM(`node -e "fetch(\"https://host.lima.internal:` + port + `/\").then(r => process.exit(r.ok ? 0 : 1), e => { console.error(e); process.exit(1) })"`)()
	})
	_ = os.RemoveAll(s.Paths.CAs)
	if ca != nil {
		ca.StopServing()
	}
	// ssh with a command runs a non-login, non-interactive shell, as editors and agents' tools do.
	s.Check("a non-login shell finds the tools too", func() error {
		_, err := s.Run(Cmd{Args: []string{"ssh", "-F", filepath.Join(s.sb.lima, vm, "ssh.config"), "lima-" + vm, "node -v"}})
		return err
	})
	// The checks above pass with a download of the locked version too: these tell the share's from one.
	// Before the .nvmrc check, which installs into ~/.local/share/mise/installs on purpose.
	for _, tool := range []string{"node", "pnpm", "bun"} {
		s.Check(tool+" runs from the share, not a download", inVM(
			`p=$(readlink -f "$(mise which `+tool+`)") && [[ $p == /opt/devvm/tools/`+tool+`/`+lockv(tool)+`/* ]] || { echo "$p"; false; }`))
	}
	s.Check("mise installed none of node, pnpm and bun into the VM", inVM(
		"[[ ! -e ~/.local/share/mise/installs/node && ! -e ~/.local/share/mise/installs/pnpm && ! -e ~/.local/share/mise/installs/bun ]]"))
	s.Check("an .nvmrc picks its own version, installed into the VM", inVM(
		"cd "+t+" && echo 22.20.0 > .nvmrc && [[ $(node -v) == v22.20.0 ]]; r=$?; rm .nvmrc; exit $r"))
	s.Check("with the .nvmrc gone, node runs from the share again", inVM(
		`for d in `+t+` ~; do p=$(cd "$d" && readlink -f "$(mise which node)") && [[ $p == /opt/devvm/tools/node/`+lockv("node")+`/* ]] || { echo "$d: $p"; exit 1; }; done`))
	// Started without devvm (so no setup): the boot itself mounts them.
	s.Check("the tools are mounted at boot", func() error {
		if _, err := s.Limactl("stop", vm); err != nil {
			return err
		}
		if _, err := s.Limactl("start", "--tty=false", vm); err != nil {
			return err
		}
		r, err := s.InVM(vm, "node -v")
		if err != nil {
			return err
		}
		if !HasLine(r.Stdout, "v"+lockv("node")) {
			return errors.New("node isn't at the lock's version after a boot")
		}
		return nil
	})
	_, _ = s.Limactl("stop", vm) // two VMs at once starve a nested Linux host
}

// toolsMissing: a tool image gone from the share stops the launch, named,
// and mise downloads nothing in its place; back in the share, the next
// launch mounts it.
func toolsMissing(s *S, vm, t string) {
	nodev := s.Paths.ToolVersion("node")
	var nodef string
	for _, f := range strings.Fields(s.Paths.State(vm)["tools.made.files"]) {
		if strings.HasPrefix(f, "node-") {
			nodef = f
		}
	}
	inShare, aside := filepath.Join(s.Paths.Share, nodef), filepath.Join(s.sb.work, nodef)
	moved := nodef != "" && os.Rename(inShare, aside) == nil
	s.Check("a tool missing from the share stops the launch, named, and isn't downloaded", func() error {
		if !moved {
			return errNo
		}
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "--network", "open", "--no-git-protect"}, Dir: t, Stdin: "node -v\n"})
		switch {
		case err == nil:
			return errors.New("the launch didn't fail")
		case !strings.Contains(r.Out, FirstLine(s.Msg("tools_unmounted", "tools=node "+nodev, "vm="+vm))):
			return errors.New("the launch didn't name node as unmounted")
		case HasLine(r.Out, "v"+nodev):
			return errors.New("node ran")
		}
		_, err = s.InVM(vm, "! mountpoint -q /opt/devvm/tools/node/"+nodev+" && [[ ! -e ~/.local/share/mise/installs/node/"+nodev+" ]]")
		return err
	})
	if moved {
		_ = os.Rename(aside, inShare)
	}
	s.Check("back in the share, the next launch mounts it", func() error {
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "--network", "open", "--no-git-protect"}, Dir: t, Stdin: "node -v\n"})
		if err != nil {
			return err
		}
		if !HasLine(r.Out, "v"+nodev) {
			return errors.New("node didn't run at the lock's version")
		}
		_, err = s.InVM(vm, "[[ $(findmnt -no FSTYPE /opt/devvm/tools/node/"+nodev+") == erofs ]]")
		return err
	})
	_, _ = s.Limactl("stop", vm)
}

// erofsCount is how many tool images the share has.
func erofsCount(s *S) int {
	n := 0
	_ = filepath.WalkDir(s.Paths.Share, func(_ string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), ".erofs") {
			n++
		}
		return nil
	})
	return n
}

// lockSum is the tools lock's bytes, to compare with later.
func lockSum(s *S) string { return readText(s.Paths.ToolLock) }

// toolsCommands: a second node VM shares the tool images; devvm tools,
// tools update, update, tools rm and add, and devvm node refused.
func toolsCommands(s *S) {
	nodev := s.Paths.ToolVersion("node")
	before := erofsCount(s)
	var vm string
	var out Res
	t2, err := s.NewProject("tools2")
	if err == nil {
		err = os.WriteFile(filepath.Join(t2, "package.json"), []byte("{}\n"), 0o600)
	}
	if err == nil {
		vm, out, _ = s.Launch(t2, "node -v", "--network", "open", "--no-git-protect")
	}
	s.Check("a second node VM uses the same tool images: none built", func() error {
		switch {
		case vm == "":
			return errNo
		case erofsCount(s) != before:
			return errors.New("a tool image was built")
		case !HasLine(out.Out, "v"+nodev):
			return errors.New("node didn't run at the lock's version")
		}
		return nil
	})
	s.Check("the tools build left no VM behind", func() error {
		if s.Status("devvm-tools-build") != "" {
			return errors.New("devvm-tools-build is still there")
		}
		return nil
	})
	// Upstream can publish mid-run (node's musl builds come days after a release): with the lock
	// unchanged, the tools are the newest; changed, the VM may move (if one of its tools did).
	s.Check("devvm tools lists node, pnpm and bun, each at the lock's version", func() error {
		r, err := s.Devvm(t2, "tools")
		if err != nil {
			return err
		}
		for _, tool := range []string{"node", "pnpm", "bun"} {
			if !Matches(r.Out, `^`+tool+` +`+regexp.QuoteMeta(s.Paths.ToolVersion(tool))+` +- +-$`) {
				return errors.New("devvm tools doesn't list " + tool + " at the lock's version\n" + r.Out)
			}
		}
		return nil
	})
	s.Check("devvm tools update: checks upstream; the newest unless the lock changed", func() error {
		lock := lockSum(s)
		r, _ := s.Run(Cmd{Args: []string{s.sb.devvm, "tools", "update"}, Dir: t2, Stdin: "exit\n"})
		if strings.Contains(r.Out, FirstLine(s.Msg("tools_newest", "vm="+vm))) ||
			(lockSum(s) != lock && strings.Contains(r.Out, FirstLine(s.Msg("tools_switch", "vm="+vm)))) {
			return nil
		}
		return errors.New("devvm tools update said neither newest nor that the VM moves")
	})
	s.Check("devvm update on a current image never makes the VM again: current, or moved to newer tools", func() error {
		lock := lockSum(s)
		r, _ := s.Run(Cmd{Args: []string{s.sb.devvm, "update"}, Dir: t2, Stdin: "exit\n"})
		switch {
		case strings.Contains(r.Out, s.Msg("vm_changed", "changed=base", "vm="+vm)):
			return errors.New("devvm update made the VM again")
		case len(vm) >= 14 && s.Status("devvm-"+vm[len(vm)-14:]+"-old") != "":
			return errors.New("devvm update left the VM's old one aside")
		case strings.Contains(r.Out, s.Msg("vm_current", "image=base", "vm="+vm)):
			return nil
		case lockSum(s) != lock && strings.Contains(r.Out, FirstLine(s.Msg("tools_switch", "vm="+vm))):
			return nil
		}
		return errors.New("devvm update said neither current nor that the VM moves")
	})
	toolsRmAdd(s, vm, t2)
}

// toolsRmAdd: devvm tools rm and add, and devvm node and --tools on an
// existing VM refused.
func toolsRmAdd(s *S, vm, t2 string) {
	nodev := s.Paths.ToolVersion("node")
	st := func() map[string]string { return s.Paths.State(vm) }
	s.Check("devvm tools rm takes node out of the VM", func() error {
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "tools", "rm", "node"}, Dir: t2, Stdin: "exit\n"}); err != nil {
			return err
		}
		switch {
		case st()["image"] != "base":
			return errors.New("the VM's image isn't base")
		case strings.Contains(st()["tools.made.files"], "node") || strings.Contains(st()["tools.names"], "node"):
			return errors.New("the VM's state still has node")
		}
		_, err := s.InVM(vm, "! command -v node")
		return err
	})
	s.Check("devvm node is an unknown command: a usage error in one line, nothing changed", func() error {
		r, err := s.DevvmNames(t2, "node")
		switch {
		case err == nil:
			return errors.New("devvm node succeeded")
		case strings.Count("\n"+r.Out, "\ndevvm: ") != 1 || !Matches(r.Out, `^devvm: no_command`):
			return errors.New("devvm node didn't fail with one no_command\n" + r.Out)
		case !HasLine(r.Out, "usage_help command=devvm"):
			return errors.New("devvm node didn't name devvm's help")
		case strings.Contains(st()["tools.names"], "node"):
			return errors.New("the VM's tools have node")
		}
		return nil
	})
	s.Check("--tools on it adds nothing: warned, naming devvm tools, and opened as it is", func() error {
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "--tools", "node"}, Dir: t2, Stdin: "exit\n", Env: []string{"DEVVM_MESSAGES=names"}})
		switch {
		case err != nil:
			return err
		case !HasLine(r.Out, "devvm: new_vm_flags_ignored_run command=devvm tools flags=--tools vm="+vm):
			return errors.New("the launch didn't warn that --tools is ignored")
		case strings.Contains(st()["tools.names"], "node"):
			return errors.New("the VM's tools have node")
		}
		return nil
	})
	s.Check("devvm tools add node mounts it again, nothing built", func() error {
		n := erofsCount(s)
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "tools", "add", "node"}, Dir: t2, Stdin: "node -v\nexit\n"})
		switch {
		case err != nil:
			return err
		case !HasLine(r.Out, "v"+nodev):
			return errors.New("node didn't run at the lock's version")
		case st()["image"] != "base" || st()["tools.names"] != "node":
			return errors.New("the VM's state doesn't have base with node")
		case erofsCount(s) != n:
			return errors.New("a tool image was built")
		}
		return nil
	})
	_, _ = s.Limactl("stop", vm) // before the dotnet VM: two VMs at once starve a nested Linux host
}

// dotnetOther is the SDK a global.json in toolsDotnet asks for, which
// isn't the shared one: mise installs it into the VM.
const dotnetOther = "8.0.427"

// toolsDotnet: a .NET project's VM on the allowlist tier, its file calling
// for dotnet: the shared SDK runs from the share; a console project
// restores a package from NuGet, builds and runs; a global tool installs
// into ~/.dotnet/tools and runs from a non-login shell; a workload
// installs into ~/.dotnet, the SDK's folder being read-only; nothing it
// reached was blocked; and a global.json's SDK installs into the VM.
func toolsDotnet(s *S) {
	var vm string
	t, err := s.NewProject("dotnet")
	if err == nil {
		err = os.WriteFile(filepath.Join(t, "Shop.slnx"), []byte("<Solution />\n"), 0o600)
	}
	if err == nil {
		vm, _, _ = s.Launch(t, "exit", "--network", "allowlist", "--no-git-protect")
	}
	v := s.Paths.ToolVersion("dotnet")
	st := func() map[string]string { return s.Paths.State(vm) }
	inVM := func(script string) func() error {
		return func() error { _, err := s.InVM(vm, script); return err }
	}
	s.Check("a .NET project's VM: dotnet's tool image, at the lock's version, in its state", func() error {
		switch {
		case vm == "" || s.Status(vm) != "Running":
			return errors.New("the dotnet VM isn't running")
		case st()["tools.names"] != "dotnet" || !Matches("files="+st()["tools.made.files"], `^files=dotnet-`+regexp.QuoteMeta(v)+`-[0-9a-f]{12}\.erofs$`):
			return errors.New("its tools aren't dotnet's at the lock's version")
		}
		return nil
	})
	for _, f := range strings.Fields(st()["tools.made.files"]) {
		b, _ := os.ReadFile(filepath.Join(s.Paths.Share, f+".json"))
		s.r.out.line("        glibc: %s needs %s", f, CaptureLine(string(b), `^  "glibc": "(.*)",?$`))
	}
	s.Check("dotnet runs the shared SDK from the share, read-only, with telemetry off", inVM(
		`[[ $(dotnet --version) == `+v+` && $DOTNET_CLI_TELEMETRY_OPTOUT == 1 ]] && p=$(readlink -f "$(mise which dotnet)") &&
		[[ $p == /opt/devvm/tools/dotnet/`+v+`/dotnet ]] && ! touch /opt/devvm/tools/dotnet/`+v+`/x 2>/dev/null`))
	s.Check("a console project restores a package from NuGet, builds and runs", inVM(
		"cd "+t+" && dotnet new console -o hello >/dev/null && cd hello && dotnet add package Newtonsoft.Json --version 13.0.3 >/dev/null &&"+
			" dotnet restore >/dev/null && dotnet build >/dev/null && [[ $(dotnet run) == 'Hello, World!' ]]"))
	s.Check("dotnet tool install -g installs into ~/.dotnet/tools, on the PATH", inVM(
		"dotnet tool install -g dotnetsay --version 3.0.3 >/dev/null && [[ $(command -v dotnetsay) == $HOME/.dotnet/tools/dotnetsay ]]"))
	s.Check("the global tool runs from a non-login shell", func() error {
		_, err := s.Run(Cmd{Args: []string{"ssh", "-F", filepath.Join(s.sb.lima, vm, "ssh.config"), "lima-" + vm, "dotnetsay hi"}})
		return err
	})
	s.Check("a workload installs into ~/.dotnet", inVM(
		"dotnet workload install wasi-experimental >/dev/null && dotnet workload list | grep -q '^wasi-experimental ' && [[ -d ~/.dotnet/packs ]]"))
	s.Check("the project's restore, tool and workload reached nothing the allowlist blocks", func() error {
		r, err := s.Devvm(t, "network", "log", "-n", "100000")
		switch {
		case err != nil:
			return err
		case !Matches(r.Out, `  allowed  api\.nuget\.org:443  `):
			return errors.New("devvm network log doesn't show NuGet\n" + r.Out)
		case strings.Contains(r.Out, "  BLOCKED  "):
			return errors.New("the proxy blocked a host\n" + r.Out)
		}
		return nil
	})
	s.Check("a global.json's SDK installs into the VM, and the shared one runs again without it", inVM(
		"cd "+t+` && echo '{ "sdk": { "version": "`+dotnetOther+`" } }' > global.json && [[ $(dotnet --version 2>/dev/null) == `+dotnetOther+` ]] &&
		[[ $(readlink -f "$(mise which dotnet)") == $HOME/.local/share/mise/installs/dotnet/`+dotnetOther+`/dotnet ]]; r=$?; rm global.json
		[[ $r == 0 && $(dotnet --version) == `+v+` ]]`))
	_, _ = s.Limactl("stop", vm) // two VMs at once starve a nested Linux host
}
