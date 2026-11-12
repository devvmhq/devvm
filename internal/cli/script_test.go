package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/stubtest"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/upgrade/upgradetest"
	"github.com/devvmhq/devvm/internal/vmname"
)

// testBuildFlag makes a test build (internal/testbuild), as tests/e2e
// does.
const testBuildFlag = "-X github.com/devvmhq/devvm/internal/testbuild.testBuild=yes"

// TestScript runs testdata/script/*.txtar against a built devvm, in a sandbox.
func TestScript(t *testing.T) {
	bin := buildDevvm(t)
	stubs := stubtest.Bin(t)
	testscript.Run(t, testscript.Params{
		Dir:                 scriptsDir(),
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		Setup: func(env *testscript.Env) error {
			return sandbox(env, bin, stubs)
		},
		Cmds: scriptCmds(),
	})
}

// scriptCmds are the commands devvm's scripts add to testscript's own.
// TestCommandLines leaves their lines out, as they aren't devvm's.
func scriptCmds() map[string]func(ts *testscript.TestScript, neg bool, args []string) {
	return map[string]func(ts *testscript.TestScript, neg bool, args []string){
		"catg":           catGlob,
		"cp_exe":         cpExe,
		"fixture":        fixture,
		"free_port":      freePort,
		"json_set":       jsonSet,
		"json_superset":  jsonSuperset,
		"listen_any":     listenAny,
		"project":        project,
		"release_server": releaseServer,
		"stub_calls":     stubCalls,
		"stub_reset":     stubReset,
		"vm_name":        vmName,
		"vm_state":       vmState,
		"wait_file":      waitFile,
	}
}

// scriptsDir is where the scripts are.
func scriptsDir() string { return filepath.Join("..", "..", "testdata", "script") }

// buildDevvm builds cmd/devvm, its files embedded, into a temporary folder
// and returns that folder. It's a test build: the test overrides count
// (readOverrides).
func buildDevvm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"install", "-ldflags", testBuildFlag}
	if os.Getenv(coverDirVar) != "" {
		args = append(args, coverFlags()...)
	}
	cmd := exec.CommandContext(t.Context(), "go", append(args, "github.com/devvmhq/devvm/cmd/devvm")...) //nolint:gosec // go install, with the test's own flags
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOBIN="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building devvm: %v\n%s", err, out)
	}
	return dir
}

// coverDirVar names the folder the CLI tests' devvm writes its coverage
// in, a folder for each script, when scripts/test.sh asks for it: devvm
// is built with -cover for
// every package of devvm's, so the CLI tests count toward each package's
// coverage, not only cli's.
const coverDirVar = "CLI_GOCOVERDIR"

// scriptCoverDir sets GOCOVERDIR, where a -cover devvm writes its coverage,
// to a folder of the script's own under CLI_GOCOVERDIR, when that's set.
// Scripts run in parallel, and devvm runs that start at once in one folder
// race on the meta-data file: Go names its temporary file by the time, to
// the microsecond on macOS, so one run's rename fails and devvm prints
// "coverage meta-data emit failed" on stderr. scripts/test.sh merges the
// folders.
func scriptCoverDir(env *testscript.Env) error {
	parent := os.Getenv(coverDirVar)
	if parent == "" {
		return nil
	}
	dir, err := os.MkdirTemp(parent, "script.")
	if err != nil {
		return err
	}
	env.Setenv("GOCOVERDIR", dir)
	return nil
}

// coverFlags are go build's flags for that: the test binary's own
// coverage mode (atomic with -race), as go tool covdata merges only
// data of one mode.
func coverFlags() []string {
	mode := testing.CoverMode()
	if mode == "" {
		mode = "atomic"
	}
	return []string{"-cover", "-covermode=" + mode, "-coverpkg=github.com/devvmhq/devvm/..."}
}

// stopLinkSessions stops the link sessions (devvm _links) of the sandbox
// at root: each process a pid file in its run folder names, if it still
// runs as devvm _links.
func stopLinkSessions(root string) {
	pids, _ := filepath.Glob(filepath.Join(root, "data", "devvm", "run", "*.links.pid"))
	for _, f := range pids {
		b, err := os.ReadFile(f) //nolint:gosec // the test's own folder
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			continue
		}
		if args, err := platform.ProcessArgs(pid); err == nil && len(args) > 1 && args[1] == "_links" {
			_ = daemon.Signal(pid, syscall.SIGKILL)
		}
	}
}

// sandbox points every folder devvm uses into a new temporary folder, and puts
// devvm and the stubs first on PATH.
func sandbox(env *testscript.Env, bin, stubs string) error {
	// /tmp keeps Lima's socket paths short.
	root, err := os.MkdirTemp("/tmp", "dvt.")
	if err != nil {
		return err
	}
	env.Defer(func() {
		// VMs' proxies follow a stub hostagent that's gone before they
		// look: stop them with the sandbox.
		_ = exec.CommandContext(context.Background(), "pkill", "-f", "_proxy -config "+root+"/").Run() //nolint:gosec,forbidigo // the test's own folder; a cleanup, after the scripts' contexts end
		// And VMs' port forwarders.
		_ = exec.CommandContext(context.Background(), "pkill", "-f", "_ports -vm .* -lifeline "+root+"/").Run() //nolint:gosec,forbidigo // the test's own folder; a cleanup, after the scripts' contexts end
		// And client VMs' link sessions, named only by their VM: by their
		// pid files, then the stub ssh's masters they and the forwarders
		// leave, which outlive them as ssh's do.
		stopLinkSessions(root)
		_ = exec.CommandContext(context.Background(), "pkill", "-f", "[-]M -S "+root+"/").Run() //nolint:gosec,forbidigo // the test's own folder; a cleanup, after the scripts' contexts end
		_ = os.RemoveAll(root)
	})

	dirs := map[string]string{
		"HOME":            "home",
		"XDG_DATA_HOME":   "data",
		"XDG_CONFIG_HOME": "config",
		"DEVVM_LIMA_HOME": "lima",
		"LIMA_HOME":       "lima",
		"STUB_DIR":        "stub",
	}
	for name, dir := range dirs {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0o750); err != nil {
			return err
		}
		env.Setenv(name, path)
	}
	log := filepath.Join(root, "stub", "calls")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		return err
	}
	env.Setenv("STUB_LOG", log)
	// Messages by their names, and devvm's own folders, which scripts read:
	// a script that checks wording sets DEVVM_MESSAGES= itself.
	env.Setenv("DEVVM_MESSAGES", "names")
	for name, dir := range scriptFolders(root) {
		env.Setenv(name, dir)
	}
	// Base's features are chosen (none), as devvm setup saves them, so
	// nothing asks.
	for _, dir := range []string{filepath.Join(root, "config", "devvm"), filepath.Join(root, "data", "devvm")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "data", "devvm", "settings.json"), []byte(`{"features": []}`+"\n"), 0o600); err != nil {
		return err
	}
	// git takes a bare repo only when it's named, as devvm doctor asks.
	if err := os.WriteFile(filepath.Join(root, "home", ".gitconfig"), []byte("[safe]\n\tbareRepository = explicit\n"), 0o600); err != nil {
		return err
	}
	// Ubuntu's cloud images: a dated release in a folder, as Ubuntu
	// published and signed it (internal/ubuntu's fixture), never the
	// network.
	if err := os.CopyFS(filepath.Join(root, "ubuntu"), os.DirFS(filepath.Join("..", "ubuntu", "testdata", ubuntu.Series))); err != nil {
		return err
	}
	env.Setenv("DEVVM_UBUNTU_URL", "file://"+filepath.Join(root, "ubuntu"))
	// Linux's /dev/kvm: a file anyone can open, so the host's doesn't
	// matter.
	kvm := filepath.Join(root, "stub", "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		return err
	}
	env.Setenv("DEVVM_KVM", kvm)
	// Linux hosts that can't pin .git in QEMU's namespace, whatever the
	// host's kernel lets devvm do: a script that tests pins sets on.
	env.Setenv("DEVVM_PINS", "this host's kernel refused")
	env.Setenv("DEVVM_SECRET_SERVICE", "devvm-test")
	// No launch asks GitHub for devvm's newest release: a script that
	// tests the check unsets it and serves its own (release_server).
	env.Setenv("DEVVM_NO_RELEASE_CHECK", "1")
	// A large host, whatever runs the tests: a script that tests a small
	// one sets its own.
	for name, v := range fakeHost() {
		env.Setenv(name, v)
	}
	// The shared tools' versions: the stub mise, which resolves the
	// fixture lock.
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return err
	}
	// The registries' lists, as devvm's messages join them, so a script
	// that checks one names no toolchain or agent itself.
	env.Setenv("TOOLCHAINS", strings.Join(image.Toolchains, " "))
	env.Setenv("AGENTS", strings.Join(image.Agents, " "))
	env.Setenv("DEVVM_HOST_MISE", filepath.Join(stubs, "mise"))
	// devvm paste's clipboard tools: the stubs, in place of the system's
	// (macOS's /usr/bin/osascript), so no script reaches the real clipboard.
	env.Setenv("DEVVM_CLIP_TOOLS", stubs)
	env.Setenv("MISE_STUB_LOCK", filepath.Join(repo, "testdata", "mise.lock"))
	env.Setenv("DEVVM_BIN", bin) // for scripts that set their own PATH
	if err := scriptCoverDir(env); err != nil {
		return err
	}
	env.Setenv("DEVVM_REPO", repo) // for the repo's own scripts (scripts/, assets/)
	env.Setenv("PATH", strings.Join([]string{bin, stubs, env.Getenv("PATH")}, string(os.PathListSeparator)))
	return nil
}

// scriptFolders are the variables naming devvm's folders in the sandbox
// at root: DATA its data folder, VMS its VMs' state files, RUN its run
// folder and CFG its config folder.
func scriptFolders(root string) map[string]string {
	data := filepath.Join(root, "data", "devvm")
	return map[string]string{
		"DATA": data,
		"VMS":  filepath.Join(data, "vms"),
		"RUN":  filepath.Join(data, "run"),
		"CFG":  filepath.Join(root, "config", "devvm"),
	}
}

// fakeHost is the host the test build reads in every script
// (platform.Host's overrides): twice the CPUs and memory base asks for,
// and more free space than any build checks for.
func fakeHost() map[string]string {
	return map[string]string{
		platform.TestCPUs:      "8",
		platform.TestMemoryGiB: "16",
		platform.TestFreeGiB:   "500",
		// Backends' listings wait long, as the stubs are slow under load:
		// a script that tests the timeout sets its own.
		platform.TestListWait: "10m",
	}
}

// waitFileTimeout is how long wait_file waits. A background devvm takes
// a dozen stub calls to get to the one that writes the file: about a
// second alone, but over 10 under the full suite's load, when each stub
// call (bash and its own commands) can take a second.
const waitFileTimeout = 60 * time.Second

// cpExe copies an executable, a new one mode 0755, for the script to run. It writes
// it under syscall.ForkLock, as testscript's cp doesn't: on Linux a child
// another script forks while the copy's write descriptor is open holds it
// until that child execs, and running the copy then fails with "text file
// busy" (ETXTBSY).
func cpExe(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: cp_exe SRC DST")
	}
	data, err := os.ReadFile(ts.MkAbs(args[0]))
	ts.Check(err)
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	f, err := os.OpenFile(ts.MkAbs(args[1]), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // a file in the script's own folder, written in place under the lock
	ts.Check(err)
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	ts.Check(err)
}

// waitFile waits for a file to exist, for a background command to get
// somewhere. It fails at once when every background command has ended
// (nothing is left to write it), else after waitFileTimeout, for one that
// hangs on the way.
func waitFile(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: wait_file FILE")
	}
	deadline := time.Now().Add(waitFileTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ts.MkAbs(args[0])); err == nil {
			return
		}
		if bg := ts.BackgroundCmds(); len(bg) > 0 && !slices.ContainsFunc(bg, running) {
			ts.Fatalf("%s didn't appear: the background command ended", args[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
	ts.Fatalf("%s didn't appear in %s", args[0], waitFileTimeout)
}

// running reports whether a background command's process hasn't been
// waited for yet. Signal 0 asks without reading the state testscript's
// own goroutine writes when it waits.
func running(cmd *exec.Cmd) bool {
	return cmd.Process != nil && !errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// releaseServer serves devvm's releases until the script ends, as GitHub
// does (upgradetest): a release of each VERSION for this machine, the
// first the newest, each devvm in it a script that says its version, and
// SHA256SUMS signed with a release key of its own, or with -badsig with
// another key. It points devvm upgrade at it: DEVVM_REPO_URL, the key's
// public half in DEVVM_RELEASE_KEY, and its certificate in
// $WORK/release-ca.pem, DEVVM_RELEASE_CA.
func releaseServer(ts *testscript.TestScript, neg bool, args []string) {
	signer := upgradetest.NewKey()
	key := signer
	if len(args) > 0 && args[0] == "-badsig" {
		signer, args = upgradetest.NewKey(), args[1:]
	}
	if neg || len(args) == 0 {
		ts.Fatalf("usage: release_server [-badsig] LATEST [VERSION...]")
	}
	s := upgradetest.Start()
	ts.Defer(s.Close)
	for i, v := range args {
		s.Add(v, upgradetest.NewRelease(signer, v, nil), i == 0)
	}
	ca := ts.MkAbs("release-ca.pem")
	ts.Check(os.WriteFile(ca, s.CA(), 0o600))
	ts.Setenv("DEVVM_REPO_URL", s.URL)
	ts.Setenv("DEVVM_RELEASE_KEY", key.Public())
	ts.Setenv("DEVVM_RELEASE_CA", ca)
}

// listenAny listens on a free port on every address, as a program on the
// host does, until the script ends, and sets VAR to the port.
func listenAny(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: listen_any VAR")
	}
	l, err := net.Listen("tcp", ":0") //nolint:noctx,gosec // every address, as the program it stands for; the script's whole life
	ts.Check(err)
	ts.Defer(func() { _ = l.Close() })
	ts.Setenv(args[0], strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
}

// freePort sets each VAR to a different port nothing on the host listens
// on now, for a script that lists ports a start or a change finds free: a
// fixed one (5173) may be a program's here. -range FIRST LAST sets the
// two to a range of two such ports, LAST the one after FIRST.
func freePort(ts *testscript.TestScript, neg bool, args []string) {
	usage := "usage: free_port VAR... | free_port -range FIRST LAST"
	if neg || len(args) == 0 {
		ts.Fatalf("%s", usage)
	}
	var held []net.Listener
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	listen := func(port int) (int, bool) {
		l, err := net.Listen("tcp", ":"+strconv.Itoa(port)) //nolint:noctx // every address, so the port is free on each; closed at once
		if err != nil {
			return 0, false
		}
		held = append(held, l)
		return l.Addr().(*net.TCPAddr).Port, true
	}
	if args[0] == "-range" {
		if len(args) != 3 {
			ts.Fatalf("%s", usage)
		}
		for range 100 {
			first, ok := listen(0)
			if !ok || first == 65535 {
				continue
			}
			if _, ok := listen(first + 1); ok {
				ts.Setenv(args[1], strconv.Itoa(first))
				ts.Setenv(args[2], strconv.Itoa(first+1))
				return
			}
		}
		ts.Fatalf("free_port: no two free ports in a row")
	}
	for _, v := range args {
		port, ok := listen(0)
		if !ok {
			ts.Fatalf("free_port: no free port")
		}
		ts.Setenv(v, strconv.Itoa(port))
	}
}

// stubCalls writes the calls the stubs logged to stdout, one per line.
// Negated, it fails if any were logged.
func stubCalls(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) > 0 {
		ts.Fatalf("usage: stub_calls")
	}
	calls := ts.ReadFile(ts.Getenv("STUB_LOG"))
	if neg {
		if calls != "" {
			ts.Fatalf("unexpected stub calls:\n%s", calls)
		}
		return
	}
	_, err := ts.Stdout().Write([]byte(calls))
	ts.Check(err)
}

// stubReset forgets the calls the stubs logged so far. It empties stdout
// and stderr too, as an exec does.
func stubReset(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) > 0 {
		ts.Fatalf("usage: stub_reset")
	}
	ts.Check(os.WriteFile(ts.Getenv("STUB_LOG"), nil, 0o600))
	ts.Stdout()
	ts.Stderr()
}

// catGlob writes the one file a glob matches to stdout, and fails when it
// matches none or more than one. Negated, it fails when the glob matches
// exactly one.
func catGlob(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: catg GLOB")
	}
	files, err := filepath.Glob(ts.MkAbs(args[0]))
	ts.Check(err)
	switch {
	case neg && len(files) == 1:
		ts.Fatalf("%s matches one file: %s", args[0], files[0])
	case neg:
	case len(files) != 1:
		ts.Fatalf("%s matches %d files, not one: %q", args[0], len(files), files)
	default:
		_, err = ts.Stdout().Write([]byte(ts.ReadFile(files[0])))
		ts.Check(err)
	}
}

// jsonSet sets fields in each JSON file GLOB matches, keeping its other
// fields; a GLOB without a wildcard that matches nothing is made, an
// object with only these fields. KEY=VALUE sets a string, KEY:=JSON any
// value; a dotted KEY (net.on) is a field of an object, made when it's
// missing. It's how scripts write devvm's JSON records, a tool image's
// built time or a VM's state say, which they can't touch.
func jsonSet(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: json_set GLOB KEY=VALUE...")
	}
	glob := ts.MkAbs(args[0])
	files, err := filepath.Glob(glob)
	ts.Check(err)
	if len(files) == 0 && !strings.ContainsAny(args[0], "*?[") {
		files = []string{glob}
	}
	if len(files) == 0 {
		ts.Fatalf("%s matches no file", args[0])
	}
	for _, f := range files {
		obj := map[string]any{}
		if data, err := os.ReadFile(f); err == nil { //nolint:gosec // the script's own file
			ts.Check(json.Unmarshal(data, &obj))
		} else if !errors.Is(err, os.ErrNotExist) {
			ts.Fatalf("%v", err)
		}
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				ts.Fatalf("usage: json_set GLOB KEY=VALUE...")
			}
			var val any = v
			if raw, isRaw := strings.CutSuffix(k, ":"); isRaw {
				k = raw
				ts.Check(json.Unmarshal([]byte(v), &val))
			}
			// A dotted key is a field of an object, made when it's missing.
			at := obj
			parts := strings.Split(k, ".")
			for _, p := range parts[:len(parts)-1] {
				next, _ := at[p].(map[string]any)
				if next == nil {
					next = map[string]any{}
					at[p] = next
				}
				at = next
			}
			at[parts[len(parts)-1]] = val
		}
		data, err := json.MarshalIndent(obj, "", "  ")
		ts.Check(err)
		ts.Check(os.MkdirAll(filepath.Dir(f), 0o700))
		ts.Check(os.WriteFile(f, append(data, '\n'), 0o600))
	}
}

// vmName sets VAR to the name of the one VM made for the folder NAME, the
// one whose state in $VMS records the name devvm-NAME-ID, and VAR_dir to
// its folder there ($VMS/ID). Negated, it fails when there's exactly one.
func vmName(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 {
		ts.Fatalf("usage: vm_name VAR NAME")
	}
	found := vmsFor(ts, args[1])
	switch {
	case neg && len(found) == 1:
		ts.Fatalf("one VM for the folder %s: %s", args[1], found[0])
	case neg:
	case len(found) != 1:
		ts.Fatalf("%d VMs for the folder %s, not one: %q", len(found), args[1], found)
	default:
		ts.Setenv(args[0], found[0])
		ts.Setenv(args[0]+"_dir", filepath.Join(ts.Getenv("VMS"), found[0][len(found[0])-vmname.HashLen:]))
	}
}

// vmsFor returns the names of the VMs whose states in $VMS record a name
// made for the folder name.
func vmsFor(ts *testscript.TestScript, name string) []string {
	states, err := filepath.Glob(filepath.Join(ts.Getenv("VMS"), "*", "vm.json"))
	if err != nil {
		ts.Fatalf("%v", err)
	}
	re := regexp.MustCompile(`^devvm-` + regexp.QuoteMeta(name) + `-[0-9a-f]+$`)
	var found []string
	for _, s := range states {
		var st struct{ Name string }
		if json.Unmarshal([]byte(ts.ReadFile(s)), &st) == nil && re.MatchString(st.Name) {
			found = append(found, st.Name)
		}
	}
	return found
}

// vmState prints the one VM state (vm.json) GLOB matches as KEY=VALUE
// lines, sorted, for scripts to match: a section's fields dotted
// (net.on=true, ports.host.ssh=23456), a list of strings space-separated,
// a list of objects by index (links.links.0.name=db), strings as they
// are and anything else as JSON.
func vmState(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: vm_state GLOB")
	}
	files, err := filepath.Glob(ts.MkAbs(args[0]))
	ts.Check(err)
	if len(files) != 1 {
		ts.Fatalf("%s matches %d files, not one: %q", args[0], len(files), files)
	}
	var v any
	ts.Check(json.Unmarshal([]byte(ts.ReadFile(files[0])), &v))
	var lines []string
	var flat func(prefix string, v any)
	flat = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, f := range v {
				flat(strings.TrimPrefix(prefix+"."+k, "."), f)
			}
		case []any:
			var words []string
			for i, e := range v {
				if s, ok := e.(string); ok {
					words = append(words, s)
				} else {
					flat(prefix+"."+strconv.Itoa(i), e)
				}
			}
			if len(words) > 0 || len(v) == 0 {
				lines = append(lines, prefix+"="+strings.Join(words, " "))
			}
		case string:
			lines = append(lines, prefix+"="+v)
		default:
			b, _ := json.Marshal(v)
			lines = append(lines, prefix+"="+string(b))
		}
	}
	flat("", v)
	slices.Sort(lines)
	_, err = ts.Stdout().Write([]byte(strings.Join(lines, "\n") + "\n"))
	ts.Check(err)
}

// project makes the folder $WORK/NAME and goes to it, as a project's
// folder; with git, it's a repo with nothing committed.
func project(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) == 0 || len(args) > 2 || len(args) == 2 && args[1] != "git" || strings.ContainsAny(args[0], `/\`) {
		ts.Fatalf("usage: project NAME [git]")
	}
	dir := filepath.Join(ts.Getenv("WORK"), args[0])
	ts.Check(os.MkdirAll(dir, 0o750))
	ts.Check(ts.Chdir(dir))
	if len(args) == 2 {
		ts.Check(ts.Exec("git", "init", "-q"))
	}
}
