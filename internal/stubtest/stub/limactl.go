package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/stubtest/stubstate"
)

// limactl stands in for Lima's limactl. It logs every call to $STUB_LOG,
// and the folder and Lima home of the last one to $STUB_DIR/limactl-env.
// Its instances are the state's (name, status, size and config): create,
// clone, start, stop, edit, rename and delete keep them up to date, and
// list --json shows them as Lima does, sorted by name: on a Linux host as
// QEMU's, each mount's 9p cache mode filled in where devvm set none.
//
// create, clone and edit: a clone starts with its source's config, size
// and lima.yaml; --mount-none drops the mounts; then each --set applies,
// in the forms devvm writes (.KEY = JSON, .KEY=JSON, del(.KEY)), the
// environment into lima.yaml as an env: line of its own; create's
// --cpus=N, --memory=GiB and --disk=GiB set its size (else 4 CPUs, 8 GiB
// and 100 GiB), and edit's --cpus, --memory and --disk change it. create
// and clone make the instance's folder, $LIMA_HOME/<name>, and create's
// template on its input (-) goes to $STUB_DIR/create-template. create
// and clone refuse a new instance's name that Lima's grammar refuses
// (limaName), separators in a row included. The
// TestLimactlStubContract test in internal/backend/lima checks all this
// against real Lima, but for create's refusal of a name, which it checks
// for clone alone.
//
// start fails on a broken instance whose driver runs (its pids, list
// --json's hostAgentPID and driverPID, are the state's), as Lima's QEMU
// can't open a disk the one left running holds. Otherwise it writes the
// VM's ha.pid (devvm's pid, which runs and is soon gone,
// or $STUB_DIR/hostagent's, a pid that lasts), which stop removes, its ssh
// config ($LIMA_HOME/<name>/ssh.config, Lima's own options, host keys
// unchecked) unless it exists, and the SSH agent it was given
// ($SSH_AUTH_SOCK, or "unset") to $STUB_DIR/start-agent-<name>, and the
// command QEMU would run for the guest's connections to the proxy
// ($DEVVM_GUESTFWD, empty when unset) to $STUB_DIR/start-guestfwd-<name>. shell
// runs in the guest (guest.go).
//
// Its switches, in $STUB_DIR:
//
//	list-fails, list-hangs        list --json fails, or hangs
//	start-fails                   start fails, leaving the VM running (a degraded start)
//	start-hangs                   start hangs (touching hanging first)
//	start-hangs-running           for a VM whose name holds its text, start hangs once the VM runs
//	                              and has its ha.pid, as Lima's waits for a VM's ssh
//	clone-fails                   clone fails, leaving its instance (a partial clone)
//	delete-fails                  delete fails, after deleting
//	rename-fails                  rename fails, changing nothing
//	interrupt-on                  a call whose arguments hold its text is Ctrl+C: it interrupts
//	                              devvm, once
//	start-links, stop-links       its text is DIR TARGET: once, as start begins or once stop ends,
//	                              DIR moves to DIR.real and a link to TARGET takes its place
//
// Everything else succeeds.
func limactl(c *call) int {
	c.log()
	cwd, _ := os.Getwd()
	if p, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = p
	}
	_ = c.put("limactl-env", []byte("cwd="+cwd+" LIMA_HOME="+c.getenv("LIMA_HOME")+"\n"))
	if c.mentions("interrupt-on") {
		_ = os.Remove(c.path("interrupt-on"))
		c.interrupt()
		return 130
	}
	l := &lima{call: c, names: vmNames(c.args)}
	if c.arg(0) == "clone" && c.has("devvm-tools-build") {
		if err := l.toolsOut(); err != nil {
			return c.status(err)
		}
	}
	if c.arg(0) == "shell" && c.has("devvm-tools-build") && hasRun(c.args, "devvm-tools-build", "bash", "-s", "--") {
		if err := l.toolsBuild(); err != nil {
			return c.status(err)
		}
	}
	if c.arg(0) == "shell" && strings.Contains(" "+c.joined()+" ", "/oci/build.sh ") {
		return c.status(l.ociBuild())
	}
	if code, done := l.command(); done {
		return code
	}
	switch {
	case c.arg(0) == "--version":
		c.print("limactl version 2.2.1\n")
	case c.arg(0) == "list" && c.arg(1) == "--json":
		return l.list()
	}
	return 0
}

// A lima is one call of limactl's, with the VM names among its
// arguments.
type lima struct {
	*call
	names []string
}

// vmNames are the VM names among limactl's arguments: neither options
// nor their values.
func vmNames(args []string) []string {
	var names []string
	skip := false
	for _, a := range args[min(1, len(args)):] {
		if skip {
			skip = false
			continue
		}
		switch {
		case a == "--set", a == "--mount-only", a == "--name", a == "--workdir", a == "--cpus", a == "--memory", a == "--disk":
			skip = true
		case strings.HasPrefix(a, "--name="):
			names = append(names, strings.TrimPrefix(a, "--name="))
		case strings.HasPrefix(a, "-"):
		default:
			names = append(names, a)
		}
	}
	return names
}

// hasRun reports whether args hold words, one after another.
func hasRun(args []string, words ...string) bool {
	for i := range args {
		if i+len(words) <= len(args) && slices.Equal(args[i:i+len(words)], words) {
			return true
		}
	}
	return false
}

// name is the i-th VM name, "" past the last.
func (l *lima) name(i int) string {
	if i < len(l.names) {
		return l.names[i]
	}
	return ""
}

// vmDir is a VM's folder in Lima's home.
func (l *lima) vmDir(name string) string { return filepath.Join(l.getenv("LIMA_HOME"), name) }

// command runs the commands that change instances, and shell; done is
// false for the rest.
func (l *lima) command() (code int, done bool) {
	switch l.arg(0) {
	case "create", "clone":
		if err := l.checkNew(); err != nil {
			return l.status(err), true
		}
	}
	switch l.arg(0) {
	case "create", "clone", "edit":
		if err := l.update(l.configure); err != nil {
			return l.status(err), true
		}
	}
	switch l.arg(0) {
	case "create":
		return l.status(l.create()), true
	case "clone":
		return l.status(l.clone()), true
	case "start":
		return l.status(l.start()), true
	case "stop":
		return l.status(l.stop()), true
	case "edit":
		return l.status(l.update(l.resize)), true
	case "delete":
		return l.status(l.delete()), true
	case "rename":
		return l.status(l.rename()), true
	case "shell":
		return l.shell(), true
	}
	return 0, false
}

// gib is GiB in bytes, rounded as printf's %.0f does; a size that isn't
// one, or too big for bytes in an int64, fails, as limactl refuses it.
func gib(s string) (int64, error) {
	g, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	n, err := strconv.ParseInt(strconv.FormatFloat(g*(1<<30), 'f', 0, 64), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n, nil
}

// cpuCount is a --cpus value, which fails unless it's a number, as
// limactl refuses it.
func cpuCount(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --cpus %q: %w", s, err)
	}
	return n, nil
}

// configure is create's, clone's and edit's part in the config: a
// clone's copy of its source's, --mount-none, each --set, and create's
// size.
func (l *lima) configure(st *stubstate.State) error {
	target := l.name(0)
	if l.arg(0) == "clone" {
		target = l.name(1)
		if err := l.cloneConfig(st, l.name(0), target); err != nil {
			return err
		}
	}
	in := st.Instances[target]
	if in == nil {
		// Its config and size, until create or clone lists it.
		in = stubstate.NewInstance("")
		st.Instances[target] = in
	}
	if l.has("--mount-none") {
		delete(in.Config, "mounts")
	}
	cpus, mem, disk := stubstate.DefaultCPUs, int64(stubstate.DefaultMemory), int64(stubstate.DefaultDisk)
	var err error
	for i, a := range l.args {
		switch {
		case a == "--set":
			err = l.set(in, target, l.arg(i+1))
		case strings.HasPrefix(a, "--cpus="):
			cpus, err = cpuCount(strings.TrimPrefix(a, "--cpus="))
		case strings.HasPrefix(a, "--memory="):
			mem, err = gib(strings.TrimPrefix(a, "--memory="))
		case strings.HasPrefix(a, "--disk="):
			disk, err = gib(strings.TrimPrefix(a, "--disk="))
		}
		if err != nil {
			return err
		}
	}
	if l.arg(0) == "create" {
		in.CPUs, in.Memory, in.Disk = cpus, mem, disk
	}
	if in.Status == "" && l.arg(0) == "edit" {
		delete(st.Instances, target)
	}
	return nil
}

// cloneConfig gives target src's config, size and lima.yaml.
func (l *lima) cloneConfig(st *stubstate.State, src, target string) error {
	delete(st.Instances, target)
	if s := st.Instances[src]; s != nil {
		c := *s
		c.Config = map[string]string{}
		for k, v := range s.Config {
			c.Config[k] = v
		}
		c.Status = ""
		st.Instances[target] = &c
	}
	y, err := readFile(filepath.Join(l.vmDir(src), "lima.yaml"))
	if err != nil {
		return nil //nolint:nilerr // no lima.yaml to copy
	}
	return writeFile(filepath.Join(l.vmDir(target), "lima.yaml"), y)
}

// set applies a --set expression to an instance's config. The
// environment goes into its lima.yaml instead, as an env: line.
func (l *lima) set(in *stubstate.Instance, name, expr string) error {
	if in.Config == nil {
		in.Config = map[string]string{}
	}
	switch {
	case strings.HasPrefix(expr, ".env = ") || expr == "del(.env)":
		return l.setEnv(name, expr)
	case strings.HasPrefix(expr, "del(.") && strings.HasSuffix(expr, ")"):
		k := strings.TrimSuffix(strings.TrimPrefix(expr, "del(."), ")")
		for key := range in.Config {
			if key == k || strings.HasPrefix(key, k+".") {
				delete(in.Config, key)
			}
		}
	case strings.HasPrefix(expr, ".") && strings.Contains(expr, "="):
		k, v, _ := strings.Cut(expr, "=")
		in.Config[strings.TrimPrefix(strings.TrimSuffix(k, " "), ".")] = strings.TrimPrefix(v, " ")
	}
	return nil
}

// setEnv sets or deletes the env: line of a VM's lima.yaml.
func (l *lima) setEnv(name, expr string) error {
	y := filepath.Join(l.vmDir(name), "lima.yaml")
	old, _ := readFile(y)
	var lines []string
	for line := range strings.Lines(string(old)) {
		if !strings.HasPrefix(line, "env:") {
			lines = append(lines, line)
		}
	}
	if expr != "del(.env)" {
		lines = append(lines, "env: "+strings.TrimPrefix(expr, ".env = ")+"\n")
	}
	return writeFile(y, []byte(strings.Join(lines, "")))
}

// configJSON is an instance's config as list --json shows it: its
// top-level keys, ssh's and hostResolver's, and lima.yaml's env.
func (l *lima) configJSON(name string, in *stubstate.Instance) string {
	var parts []string
	keys := sortedKeys(in.Config)
	for _, k := range keys {
		switch {
		case k == "mounts" && runtime.GOOS == "linux":
			parts = append(parts, strconv.Quote(k)+":"+filledMounts(in.Config[k]))
		case !strings.Contains(k, "."):
			parts = append(parts, strconv.Quote(k)+":"+in.Config[k])
		}
	}
	for _, p := range []string{"ssh", "hostResolver"} {
		var sub []string
		for _, k := range keys {
			if f, ok := strings.CutPrefix(k, p+"."); ok {
				sub = append(sub, strconv.Quote(f)+":"+in.Config[k])
			}
		}
		if len(sub) > 0 {
			parts = append(parts, strconv.Quote(p)+":{"+strings.Join(sub, ",")+"}")
		}
	}
	y, _ := readFile(filepath.Join(l.vmDir(name), "lima.yaml"))
	var env []string
	for line := range strings.Lines(string(y)) {
		if v, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "env: "); ok {
			env = append(env, v)
		}
	}
	if len(env) > 0 {
		parts = append(parts, `"env":`+strings.Join(env, "\n"))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// filledMounts is mounts, the JSON devvm set, with each mount's 9p cache
// mode filled in where it sets none, as list --json has QEMU's: mmap for
// a writable mount, fscache for a read-only one. Anything it can't read
// is shown as it is.
func filledMounts(mounts string) string {
	var ms []map[string]any
	if err := json.Unmarshal([]byte(mounts), &ms); err != nil {
		return mounts
	}
	for _, m := range ms {
		p9, _ := m["9p"].(map[string]any)
		if p9 == nil {
			p9 = map[string]any{}
		}
		if _, ok := p9["cache"]; !ok {
			p9["cache"] = "fscache"
			if w, _ := m["writable"].(bool); w {
				p9["cache"] = "mmap"
			}
		}
		m["9p"] = p9
	}
	out, err := json.Marshal(ms)
	if err != nil {
		return mounts
	}
	return string(out)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setStatus lists name in status, keeping its size and config; its pids
// are a broken instance's alone.
func setStatus(st *stubstate.State, name, status string) {
	if in := st.Instances[name]; in != nil {
		in.Status = status
		in.HostAgentPID, in.DriverPID = 0, 0
		return
	}
	st.Instances[name] = stubstate.NewInstance(status)
}

// limaName is Lima's grammar of an instance's name (its identifiers
// package): letters and digits, with one of . _ - between them, never
// two in a row.
var limaName = regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-](?:[A-Za-z0-9]+))*$`)

// checkNew fails create or clone, changing nothing, when the instance it
// makes has a name Lima refuses (limaName).
func (l *lima) checkNew() error {
	names := l.names[min(1, len(l.names)):] // clone's target
	if l.arg(0) == "create" {
		names = l.names
	}
	for _, n := range names {
		if n != "-" && !isRegular(n) && !limaName.MatchString(n) {
			return fmt.Errorf("instance name %q is invalid: identifier must match %s", n, limaName)
		}
	}
	return nil
}

// create lists each name that isn't a file (create's template) as
// stopped, and keeps a template on its input.
func (l *lima) create() error {
	err := l.update(func(st *stubstate.State) error {
		for _, n := range l.names {
			if isRegular(n) {
				continue
			}
			setStatus(st, n, "Stopped")
			if err := mkdir(l.vmDir(n)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || l.last() != "-" {
		return err
	}
	return l.copyIn("create-template")
}

// clone lists the clone as stopped, then fails with clone-fails.
func (l *lima) clone() error {
	err := l.update(func(st *stubstate.State) error {
		setStatus(st, l.name(1), "Stopped")
		return mkdir(l.vmDir(l.name(1)))
	})
	if err == nil && l.on("clone-fails") {
		return errStatus(1)
	}
	return err
}

// start runs the VM: running, its ha.pid and ssh config written.
func (l *lima) start() error {
	if l.on("start-hangs") {
		l.hang()
		return nil
	}
	vm := l.name(0)
	if err := l.replaceWithLink("start-links"); err != nil {
		return err
	}
	if err := l.update(func(st *stubstate.State) error {
		// A driver left running holds the VM's disk: Lima's new one
		// can't open it.
		if in := st.Instances[vm]; in != nil && in.Status == "Broken" && in.DriverPID > 0 {
			return errStatus(1)
		}
		setStatus(st, vm, "Running")
		return nil
	}); err != nil {
		return err
	}
	// Its boot mounts the project folder a failed remount left unmounted
	// (guest.go's remount-fails).
	_ = os.Remove(l.path("unmounted-" + vm))
	ha := strconv.Itoa(l.ppid)
	if t, ok := l.text("hostagent"); ok {
		ha = t
	}
	if err := writeFile(filepath.Join(l.vmDir(vm), "ha.pid"), []byte(ha+"\n")); err != nil {
		return err
	}
	if t, ok := l.text("start-hangs-running"); ok && strings.Contains(vm, t) {
		l.hang()
		return nil
	}
	if err := l.put("start-agent-"+vm, []byte(l.agent()+"\n")); err != nil {
		return err
	}
	// The command QEMU would run for each of the guest's connections to the proxy, on Linux.
	if err := l.put("start-guestfwd-"+vm, []byte(l.getenv("DEVVM_GUESTFWD")+"\n")); err != nil {
		return err
	}
	if cfg := filepath.Join(l.vmDir(vm), "ssh.config"); !exists(cfg) {
		if err := writeFile(cfg, []byte(sshConfig(vm, l.getenv("LIMA_HOME")))); err != nil {
			return err
		}
	}
	if l.on("start-fails") {
		return errStatus(1)
	}
	return nil
}

// sshConfig is the ssh config Lima writes for a VM.
func sshConfig(vm, home string) string {
	return "Host lima-" + vm + "\n" +
		"  IdentityFile \"" + home + "/_config/user\"\n" +
		"  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  NoHostAuthenticationForLocalhost yes\n" +
		"  PreferredAuthentications publickey\n  BatchMode yes\n  IdentitiesOnly yes\n  User stub\n" +
		"  ControlMaster auto\n  ControlPath \"" + home + "/" + vm + "/ssh.sock\"\n  ControlPersist yes\n" +
		"  PermitLocalCommand yes\n  LocalCommand touch /x/ran\n  Hostname 127.0.0.1\n  Port 60022\n"
}

// stop stops the VM and removes its ha.pid.
func (l *lima) stop() error {
	vm := l.name(0)
	if err := l.update(func(st *stubstate.State) error { setStatus(st, vm, "Stopped"); return nil }); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(l.vmDir(vm), "ha.pid"))
	return l.replaceWithLink("stop-links")
}

// replaceWithLink is the switch name's (start-links, stop-links), once:
// its text, DIR TARGET, has DIR moved aside to DIR.real and a link to
// TARGET put in its place, as a parent folder's VM could through its
// mount.
func (l *lima) replaceWithLink(name string) error {
	t, ok := l.text(name)
	if !ok {
		return nil
	}
	_ = os.Remove(l.path(name))
	dir, target, _ := strings.Cut(strings.TrimSpace(t), " ")
	if err := os.Rename(dir, dir+".real"); err != nil {
		return err
	}
	return os.Symlink(target, dir)
}

// resize is edit's --cpus N, --memory GiB and --disk GiB.
func (l *lima) resize(st *stubstate.State) error {
	in := st.Instances[l.name(0)]
	if in == nil {
		return nil
	}
	for i := 0; i+1 < len(l.args); i++ {
		v := l.args[i+1]
		var err error
		switch l.args[i] {
		case "--cpus":
			in.CPUs, err = cpuCount(v)
		case "--memory":
			in.Memory, err = gib(v)
		case "--disk":
			in.Disk, err = gib(v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// delete forgets each VM and its folder, then fails with delete-fails.
func (l *lima) delete() error {
	err := l.update(func(st *stubstate.State) error {
		for _, n := range l.names {
			delete(st.Instances, n)
			if err := os.RemoveAll(l.vmDir(n)); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && l.on("delete-fails") {
		return errStatus(1)
	}
	return err
}

// rename moves a stopped instance (status, config, size and folder) to
// its new name.
func (l *lima) rename() error {
	if l.on("rename-fails") {
		return errStatus(1)
	}
	old, name := l.name(0), l.name(1)
	return l.update(func(st *stubstate.State) error {
		in := st.Instances[old]
		if in == nil || in.Status != "Stopped" {
			l.say("limactl stub: " + old + " isn't there, stopped")
			return errStatus(1)
		}
		if st.Instances[name] != nil {
			l.say("limactl stub: " + name + " exists")
			return errStatus(1)
		}
		delete(st.Instances, old)
		st.Instances[name] = in
		if exists(l.vmDir(old)) {
			return os.Rename(l.vmDir(old), l.vmDir(name))
		}
		return nil
	})
}

// listed is an instance as list --json shows it.
type listed struct {
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Dir           string          `json:"dir"`
	CPUs          int             `json:"cpus"`
	Memory        int64           `json:"memory"`
	Disk          int64           `json:"disk"`
	SSHLocalPort  int             `json:"sshLocalPort"`
	SSHConfigFile string          `json:"sshConfigFile"`
	HostAgentPID  int             `json:"hostAgentPID,omitempty"`
	DriverPID     int             `json:"driverPID,omitempty"`
	VMType        string          `json:"vmType,omitempty"`
	Config        json.RawMessage `json:"config"`
}

// vmType is the driver list --json shows: QEMU's on a Linux host, and
// none on macOS, where devvm's tests mount project folders by their path.
func vmType() string {
	if runtime.GOOS == "linux" {
		return "qemu"
	}
	return ""
}

// list prints the instances, one JSON object a line, sorted by name.
func (l *lima) list() int {
	if l.on("list-fails") {
		return 1
	}
	if l.on("list-hangs") {
		l.sleep(30 * time.Second)
		return 0
	}
	st, err := l.state()
	if err != nil {
		return l.status(err)
	}
	names := make([]string, 0, len(st.Instances))
	for n, in := range st.Instances {
		if in.Status != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	enc := json.NewEncoder(l.stdout)
	enc.SetEscapeHTML(false)
	for _, n := range names {
		in := st.Instances[n]
		dir := l.vmDir(n)
		err := enc.Encode(listed{
			Name: n, Status: in.Status, Dir: dir, CPUs: in.CPUs, Memory: in.Memory, Disk: in.Disk,
			SSHLocalPort: 60022, SSHConfigFile: filepath.Join(dir, "ssh.config"),
			HostAgentPID: in.HostAgentPID, DriverPID: in.DriverPID, VMType: vmType(),
			Config: json.RawMessage(l.configJSON(n, in)),
		})
		if err != nil {
			return l.status(err)
		}
	}
	return 0
}

// locationRE finds the last location in a clone's arguments: the folder
// the tools build VM mounts.
var locationRE = regexp.MustCompile(`(?s).*"location": *"([^"]*)"`)

// toolsOut remembers the folder the tools build VM mounts, where its
// build.sh writes the images.
func (l *lima) toolsOut() error {
	out := ""
	if m := locationRE.FindStringSubmatch(l.joined()); m != nil {
		out = m[1]
	}
	return l.update(func(st *stubstate.State) error { st.ToolsOut = out; return nil })
}

// toolsBuild is the tools build VM's build.sh: for each TOOL VERSION FILE
// after --, it writes FILE, an image, and FILE.glibc, the glibc version
// the tool needs ($STUB_DIR/tools-glibc-<tool>, else none). A hostile
// build: with $STUB_DIR/tools-symlink, FILE is a symlink to it instead,
// and with tools-hash-symlink, FILE.json, a tool image's record, is one
// to that.
func (l *lima) toolsBuild() error {
	st, err := l.state()
	if err != nil {
		return err
	}
	out := st.ToolsOut
	i := 0
	for i < len(l.args) && l.args[i] != "--" {
		i++
	}
	for i += 3; i < len(l.args); i += 3 {
		tool, file := l.args[i-2], filepath.Join(out, l.args[i])
		glibc, _ := readFile(l.path("tools-glibc-" + tool))
		if err := writeFile(file+".glibc", glibc); err != nil {
			return err
		}
		if l.on("tools-symlink") {
			err = os.Symlink(l.path("tools-symlink"), file)
		} else {
			// A page, as a real image is never shorter than its
			// superblock's end (the shared tools count one cut short).
			img := make([]byte, 4096)
			copy(img, "image "+l.args[i]+"\n")
			err = writeFile(file, img)
		}
		if err != nil {
			return err
		}
		if l.on("tools-hash-symlink") {
			if err := os.Symlink(l.path("tools-hash-symlink"), file+".json"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ociBuild is the --from build VM's assets/oci/build.sh: it writes what
// it would in its OUT folder, the argument after the next. Its switches:
//
//	build-fails     it fails
//	build-libc      the libc, else gnu
//	build-glibc     a gnu image's glibc, else 2.36
//	build-cmdline   the kernel's command line
//	build-qcow2     disk.raw is a qcow2 header, not ext4's superblock magic (0xEF53 at byte 1080)
//	build-symlinks  a hostile image: "FILE TARGET" lines replace FILE with a symlink
func (l *lima) ociBuild() error {
	if l.on("build-fails") {
		return errStatus(1)
	}
	out := ""
	for i, a := range l.args {
		if strings.HasSuffix(a, "/oci/build.sh") {
			out = l.arg(i + 2)
		}
	}
	at := func(name string) string { return filepath.Join(out, name) }
	libc := l.raw("build-libc", "gnu\n")
	files := map[string][]byte{
		"libc":    libc,
		"cmdline": l.raw("build-cmdline", "root=/dev/vda rw console=hvc0\n"),
		"kernel":  nil,
		"initrd":  nil,
	}
	if strings.TrimRight(string(libc), "\n") == "gnu" {
		files["glibc"] = l.raw("build-glibc", "2.36\n")
	}
	disk := append(make([]byte, 1080), 0x53, 0xef)
	if l.on("build-qcow2") {
		disk = []byte("QFI\xfb")
	}
	files["disk.raw"] = disk
	for name, data := range files {
		if err := writeFile(at(name), data); err != nil {
			return err
		}
	}
	links, ok := l.text("build-symlinks")
	if !ok {
		return nil
	}
	for line := range strings.Lines(links) {
		f, t, _ := strings.Cut(strings.TrimSpace(line), " ")
		if f == "" {
			continue
		}
		_ = os.Remove(at(f))
		if err := os.Symlink(strings.TrimSpace(t), at(f)); err != nil {
			return err
		}
	}
	return nil
}
