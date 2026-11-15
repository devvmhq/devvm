package sshconfig

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

const (
	web    = "devvm-web-aaaaaaaaaaaaaa"
	web2   = "devvm-web-bbbbbbbbbbbbbb"
	api    = "devvm-api-cccccccccccccc"
	imgish = "devvm-img-foo-abc123abc123ab" // a project folder named img-foo
)

// made makes each VM's state, with the alias ChooseAlias gives it then,
// as up does (under the claims lock), in order.
func made(t *testing.T, d paths.Dirs, vms ...vmname.VM) {
	t.Helper()
	for _, vm := range vms {
		create(t, d, vm)
	}
}

// create makes vm's state as up does: its alias chosen under the claims
// lock.
func create(t *testing.T, d paths.Dirs, vm vmname.VM) {
	t.Helper()
	st, err := state.New(d, vm)
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend = "base", "/p", "lima"
	err = st.Create(t.Context(), func(st *state.VM) error {
		var err error
		st.Alias, err = ChooseAlias(d, vm)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Two VMs of one folder's name made at once never take one alias: each
// chooses under the claims lock.
func TestChooseAliasTogether(t *testing.T) {
	c, _ := testConfig(t)
	d := c.Dirs
	vms := []vmname.VM{"devvm-web-aaaaaaaaaaaaa1", "devvm-web-aaaaaaaaaaaaa2", "devvm-web-aaaaaaaaaaaaa3", "devvm-web-aaaaaaaaaaaaa4"}
	var wg sync.WaitGroup
	for _, vm := range vms {
		wg.Go(func() { create(t, d, vm) })
	}
	wg.Wait()
	short := 0
	for _, vm := range vms {
		st, err := state.ReadVM(d, vm)
		if err != nil {
			t.Fatal(err)
		}
		switch st.Alias {
		case "devvm-web":
			short++
		case vm.String():
		default:
			t.Errorf("%s: alias %q", vm, st.Alias)
		}
	}
	if short != 1 {
		t.Errorf("%d VMs took devvm-web, want 1", short)
	}
}

// A VM's alias is devvm-<folder> unless another VM holds that; it's
// chosen once, so the first VM of a folder's name keeps it when a second
// comes, and the second keeps its full name when the first goes.
func TestChooseAlias(t *testing.T) {
	c, _ := testConfig(t)
	d := c.Dirs
	const dashed = "devvm-web-app-dddddddddddddd" // a folder whose name has a dash is another folder
	made(t, d, web, api, imgish, dashed)
	for vm, want := range map[vmname.VM]string{web: "devvm-web", api: "devvm-api", imgish: "devvm-img-foo", dashed: "devvm-web-app"} {
		if got := c.Host(vm); got != want {
			t.Errorf("Host(%s) = %s, want %s", vm, got, want)
		}
	}
	// Another folder with the same name: the hash tells them apart.
	made(t, d, web2)
	if c.Host(web) != "devvm-web" || c.Host(web2) != web2 {
		t.Errorf("with two web folders: %s, %s", c.Host(web), c.Host(web2))
	}
	st, err := state.ReadVM(d, web)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.Host(web2) != web2 {
		t.Errorf("once the first is gone: %s", c.Host(web2))
	}
	// Made again, the VM of a folder takes the alias no other VM holds.
	if got, err := ChooseAlias(d, web); err != nil || got != "devvm-web" {
		t.Errorf("ChooseAlias(%s) again = %s, %v", web, got, err)
	}
	// A VM without state, or one recording none, is reached by its name.
	if got := c.Host("devvm-none-eeeeeeeeeeeeee"); got != "devvm-none-eeeeeeeeeeeeee" {
		t.Errorf("Host without state = %s", got)
	}
	if got, err := ChooseAlias(d, "devvm-tools-build"); err != nil || got != "devvm-tools-build" {
		t.Errorf("ChooseAlias(devvm-tools-build) = %s, %v", got, err)
	}
	// So is one whose state records an alias ChooseAlias never gives it:
	// another folder's, a pattern, or more lines for its ssh config.
	for _, bad := range []string{"devvm-web-app", "*", "devvm-api x", "devvm-api\n  ProxyCommand x", "api", web2} {
		setAlias(t, d, api, bad)
		if got := c.Host(api); got != api {
			t.Errorf("Host(%s) with alias %q = %q", api, bad, got)
		}
	}
	// Nor does such an alias keep a new VM from its own.
	setAlias(t, d, api, "devvm-new")
	if got, err := ChooseAlias(d, "devvm-new-ffffffffffffff"); err != nil || got != "devvm-new" {
		t.Errorf("ChooseAlias with api's alias devvm-new = %s, %v", got, err)
	}
}

// setAlias records alias in vm's state, as a hand-edited vm.json might.
func setAlias(t *testing.T, d paths.Dirs, vm vmname.VM, alias string) {
	t.Helper()
	st, err := state.ReadVM(d, vm)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(t.Context(), func(st *state.VM) error { st.Alias = alias; return nil }); err != nil {
		t.Fatal(err)
	}
}

// pinned is what every entry turns off, whatever a Host * says.
const pinned = `  ForwardX11 no
  ForwardX11Trusted no
  Tunnel no
  PermitRemoteOpen none
  AddKeysToAgent no
  ProxyJump none
  ProxyCommand none
  UpdateHostKeys no
  HashKnownHosts no
`

// keyOf is a made-up ed25519 host key of vm's, in ssh's wire format.
func keyOf(vm vmname.VM) string {
	b := []byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20")
	return base64.StdEncoding.EncodeToString(append(b, (vm.String() + strings.Repeat(".", 32))[:32]...))
}

// testConfig is a config on a fake backend whose VMs answer with a
// made-up host key each (keyOf), and lines that aren't keys.
func testConfig(t *testing.T) (*Config, *backendtest.Fake) {
	t.Helper()
	root := t.TempDir()
	f := backendtest.New("fake", backend.Caps{backend.FeatureSSH: backend.Native})
	f.Target = backend.SSHTarget{Host: "127.0.0.1", Port: 60022, User: "u", Key: "/x/key", Options: []backend.SSHOption{
		{Key: "IdentitiesOnly", Value: "yes"},
	}}
	f.ExecFunc = func(vm vmname.VM, argv []string, o backend.ExecOpts) error {
		if len(argv) != 3 || !strings.Contains(argv[2], "/etc/ssh/ssh_host_") {
			return errors.New("unexpected command")
		}
		_, err := io.WriteString(o.Stdout, "ssh-ed25519 "+keyOf(vm)+" root@x\n"+
			"garbage\nssh-rsa not;base64\n*-evil AAAA\n")
		return err
	}
	return &Config{
		Dirs:     paths.Dirs{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data")},
		Reporter: &msg.Fake{},
	}, f
}

// up is every VM f has, as Write is given them: Write leaves out those
// that aren't running project VMs.
func up(f *backendtest.Fake) []Running {
	var out []Running
	for _, in := range f.Instances {
		out = append(out, Running{Backend: f, Instance: in})
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func setAgent(t *testing.T, c *Config, vm vmname.VM, mode string) {
	t.Helper()
	st, err := state.ReadVM(c.Dirs, vm)
	if err == nil && !st.Exists() {
		create(t, c.Dirs, vm)
		st, err = state.ReadVM(c.Dirs, vm)
	}
	if err == nil {
		err = st.Update(t.Context(), func(st *state.VM) error { return sshagent.SetMode(st, mode) })
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Agent forwarding is what the VM's state says, whatever its backend
// config has: devvm's own agent by its socket, as the agent too, so it's
// forwarded however the editor was started.
func TestWriteAgent(t *testing.T) {
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning, AgentForwarding: true}}
	sock := `"` + c.Dirs.AgentSock() + `"`
	for _, tc := range []struct {
		mode  string
		want  string
		agent bool
	}{
		{sshagent.Devvm, "  ForwardAgent " + sock + "\n", true},
		{sshagent.On, "  ForwardAgent yes\n", false},
		{sshagent.Off, "  ForwardAgent no\n", false},
		{"", "  ForwardAgent no\n", false},
	} {
		setAgent(t, c, web, tc.mode)
		if err := c.Write(t.Context(), up(f)); err != nil {
			t.Fatal(err)
		}
		got := read(t, c.Dirs.SSHConfig())
		if !strings.Contains(got, tc.want) || strings.Count(got, "ForwardAgent") != 1 {
			t.Errorf("mode %q:\n%s", tc.mode, got)
		}
		if has := strings.Contains(got, "  ControlPath none\n  IdentityAgent "+sock+"\n"+pinned+"  Hostname "); has != tc.agent {
			t.Errorf("mode %q: IdentityAgent %v, want %v:\n%s", tc.mode, has, tc.agent, got)
		}
	}
}

// A VM devvm started forwards the agent its start was made with: a change
// waits for its next start. One started some other way since forwards the
// one its state says.
func TestWriteAgentStarted(t *testing.T) {
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning, AgentForwarding: true}}
	setAgent(t, c, web, sshagent.Off)
	st, err := state.ReadVM(c.Dirs, web)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.Running(t.Context(), web)
	if err == nil {
		err = sshagent.RecordStart(t.Context(), st, sshagent.Off, run.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	setAgent(t, c, web, sshagent.On)
	for _, tc := range []struct {
		name, want string
		do         func()
	}{
		{"changed while running", "  ForwardAgent no\n", func() {}},
		{"started outside devvm", "  ForwardAgent yes\n", func() { f.Restart(web) }},
	} {
		tc.do()
		if err := c.Write(t.Context(), up(f)); err != nil {
			t.Fatal(err)
		}
		if got := read(t, c.Dirs.SSHConfig()); !strings.Contains(got, tc.want) {
			t.Errorf("%s:\n%s", tc.name, got)
		}
	}
}

func TestWrite(t *testing.T) {
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{
		web:                      {Name: web, Status: backend.StatusRunning},
		api:                      {Name: api, Status: backend.StatusStopped},
		imgish:                   {Name: imgish, Status: backend.StatusRunning},
		"devvm-img-0123456789ab": {Name: "devvm-img-0123456789ab", Status: backend.StatusRunning},
		"devvm-tools-build":      {Name: "devvm-tools-build", Status: backend.StatusRunning},
	}
	made(t, c.Dirs, web, api, imgish)
	setAgent(t, c, imgish, sshagent.On)
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	khImg, khWeb := c.Dirs.VM(imgish).KnownHosts(), c.Dirs.VM(web).KnownHosts()
	want := header + `
Host devvm-img-foo ` + imgish + `
  HostKeyAlias ` + imgish + `
  UserKnownHostsFile "` + khImg + `"
  StrictHostKeyChecking yes
  NoHostAuthenticationForLocalhost no
  ForwardAgent yes
  ControlMaster no
  ControlPath none
` + pinned + `  Hostname 127.0.0.1
  Port 60022
  User u
  IdentityFile "/x/key"
  IdentitiesOnly yes

Host devvm-web ` + web + `
  HostKeyAlias ` + web + `
  UserKnownHostsFile "` + khWeb + `"
  StrictHostKeyChecking yes
  NoHostAuthenticationForLocalhost no
  ForwardAgent no
  ControlMaster no
  ControlPath none
` + pinned + `  Hostname 127.0.0.1
  Port 60022
  User u
  IdentityFile "/x/key"
  IdentitiesOnly yes
`
	if got := read(t, c.Dirs.SSHConfig()); got != want {
		t.Errorf("ssh config:\n%s\nwant:\n%s", got, want)
	}
	// Only the running project VMs' keys, each in its own record for its
	// start, under its name, nothing but host keys.
	record := func(vm vmname.VM) string {
		return "# devvm: host keys of start start-0\n" + vm.String() + " ssh-ed25519 " + keyOf(vm) + "\n"
	}
	for vm, kh := range map[vmname.VM]string{imgish: khImg, web: khWeb} {
		if got := read(t, kh); got != record(vm) {
			t.Errorf("%s's known hosts = %q, want %q", vm, got, record(vm))
		}
	}
	if got := read(t, c.Dirs.VM(api).KnownHosts()); got != "" {
		t.Errorf("a stopped VM's keys: %q", got)
	}
	if fi, err := os.Stat(c.Dirs.SSHConfig()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("ssh config's mode: %v, %v", fi, err)
	}

	// Keys are read once for each start; a stopped VM drops out, its keys kept.
	f.Instances[imgish] = backend.Instance{Name: imgish, Status: backend.StatusStopped}
	execs := 0
	for _, m := range f.Methods() {
		if m == "Exec" {
			execs++
		}
	}
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.Methods() {
		if m == "Exec" {
			execs--
		}
	}
	if execs != 0 {
		t.Errorf("keys read again: %d more Execs", -execs)
	}
	got := read(t, c.Dirs.SSHConfig())
	if strings.Contains(got, imgish) || !strings.Contains(got, "Host devvm-web "+web+"\n") {
		t.Errorf("ssh config with %s stopped:\n%s", imgish, got)
	}
	if read(t, khImg) != record(imgish) {
		t.Errorf("known hosts changed: %q", read(t, khImg))
	}

	// Started again: its keys are read again, for the new start.
	f.Restart(web)
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, khWeb); !strings.HasPrefix(got, "# devvm: host keys of start start-1\n") {
		t.Errorf("after a start: %q", got)
	}

	// Two web folders: the first keeps its alias, the second has its full
	// name only. Two that record the same alias (made at once) give it to
	// the first entry alone.
	made(t, c.Dirs, web2)
	f.Instances[web2] = backend.Instance{Name: web2, Status: backend.StatusRunning}
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); !strings.Contains(got, "\nHost devvm-web "+web+"\n") || !strings.Contains(got, "\nHost "+web2+"\n") {
		t.Errorf("ssh config with two web folders:\n%s", got)
	}
	if st, err := state.ReadVM(c.Dirs, web2); err != nil || st.Update(t.Context(), func(st *state.VM) error { st.Alias = "devvm-web"; return nil }) != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); strings.Count(got, "Host devvm-web ") != 1 || !strings.Contains(got, "\nHost "+web2+"\n") {
		t.Errorf("ssh config with one alias twice:\n%s", got)
	}

	// An alias that isn't the VM's own goes nowhere near the config.
	setAlias(t, c.Dirs, web2, "devvm-web\n  ProxyCommand touch /tmp/x\nHost *")
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); strings.Contains(got, "touch") || strings.Contains(got, "Host *") || !strings.Contains(got, "\nHost "+web2+"\n") {
		t.Errorf("ssh config with an alias of lines:\n%s", got)
	}
}

// New's config checks VMs' ssh against the registry's review, and writes
// the running project VMs it's given alone: a stopped one, or one that
// isn't a project VM, gets no entry. A backend that can't say how a VM
// runs gives no entries; a run cancelled writes nothing.
func TestWriteFailures(t *testing.T) {
	t.Parallel()
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{
		web:                 {Name: web, Status: backend.StatusRunning},
		api:                 {Name: api, Status: backend.StatusStopped},
		"devvm-tools-build": {Name: "devvm-tools-build", Status: backend.StatusRunning},
	}
	made(t, c.Dirs, web)
	made(t, c.Dirs, api)
	if st, err := state.ReadVM(c.Dirs, web); err != nil || st.Update(t.Context(), func(st *state.VM) error { st.Backend = "fake"; return nil }) != nil {
		t.Fatal(err)
	}
	reg, err := backend.NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(c.Dirs, reg).Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	got := read(t, c.Dirs.SSHConfig())
	if !strings.Contains(got, "\nHost devvm-web "+web+"\n") || strings.Contains(got, api) || strings.Contains(got, "tools-build") {
		t.Errorf("New's config:\n%s", got)
	}
	f.Errs = map[string]error{"Running": errors.New("no")}
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); got != header {
		t.Errorf("Running fails: %q", got)
	}
	f.Errs = nil
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Write(cancelled, up(f)); err == nil {
		t.Error("written once cancelled")
	}
}

// A record devvm can't read fails the ssh config: it's devvm's own file.
func TestWriteUnreadableRecord(t *testing.T) {
	t.Parallel()
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	made(t, c.Dirs, web)
	if err := os.MkdirAll(c.Dirs.VM(web).KnownHosts(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), up(f)); err == nil {
		t.Error("written past an unreadable record")
	}
}

// A running VM whose state can't be read fails the ssh config, which
// stays as it was; so does choosing an alias while any state can't be
// read. A VM choosing again skips its own state.
func TestUnreadableState(t *testing.T) {
	t.Parallel()
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	made(t, c.Dirs, web, api)
	if got, err := ChooseAlias(c.Dirs, web); err != nil || got != "devvm-web" {
		t.Errorf("ChooseAlias(%s) with its own state = %s, %v", web, got, err)
	}
	if err := state.WriteFile(c.Dirs.VM(web).State(), []byte("[]")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), up(f)); err == nil {
		t.Error("written past a state it can't read")
	}
	if _, err := os.Stat(c.Dirs.SSHConfig()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ssh config written: %v", err)
	}
	if _, err := ChooseAlias(c.Dirs, api); err == nil {
		t.Error("ChooseAlias past a state it can't read")
	}
}

// A VM that doesn't show its host keys, or has no ssh target, gets no
// entry: ssh couldn't check it.
func TestWriteUnchecked(t *testing.T) {
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	f.ExecFunc = func(vmname.VM, []string, backend.ExecOpts) error { return errors.New("no answer") }
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); got != header {
		t.Errorf("ssh config = %q", got)
	}
	if got := read(t, c.Dirs.VM(web).KnownHosts()); got != "" {
		t.Errorf("known hosts = %q", got)
	}
	f.ExecFunc = func(_ vmname.VM, _ []string, o backend.ExecOpts) error {
		_, err := io.WriteString(o.Stdout, "nothing here\n")
		return err
	}
	f.Errs = map[string]error{"SSH": errors.New("no ssh")}
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	f.Errs = nil
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c.Dirs.SSHConfig()); got != header {
		t.Errorf("ssh config = %q", got)
	}
}

// A backend whose ssh is an emulation not yet reviewed gets no entries,
// unless a test build lets it through (DEVVM_UNREVIEWED); none whose
// backend lacks it.
func TestWriteUnreviewed(t *testing.T) {
	t.Parallel()
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	made(t, c.Dirs, web)
	for _, tc := range []struct {
		caps backend.Caps
		let  map[string]bool
		want bool
	}{
		{backend.Caps{backend.FeatureSSH: backend.Emulated(false)}, nil, false},
		{backend.Caps{backend.FeatureSSH: backend.Emulated(false)}, map[string]bool{"fake:ssh": true}, true},
		{backend.Caps{backend.FeatureSSH: backend.Emulated(true)}, nil, true},
		{nil, map[string]bool{"fake:ssh": true}, false},
	} {
		f.Capabilities, c.Review.Unreviewed = tc.caps, tc.let
		if err := c.Write(t.Context(), up(f)); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(read(t, c.Dirs.SSHConfig()), "Host devvm-web "); got != tc.want {
			t.Errorf("%v, let through %v: entry %v, want %v", tc.caps, tc.let, got, tc.want)
		}
	}
}

// The real ssh reads devvm's entries under a user's config whose Host *
// has an IgnoreUnknown of its own (as macOS users' UseKeychain needs):
// devvm's entries set no IgnoreUnknown, which would win and replace it.
func TestEntriesWithRealSSH(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	made(t, c.Dirs, web)
	if err := c.Write(t.Context(), up(f)); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(t.TempDir(), "config")
	conf := "Include " + c.Dirs.SSHConfig() + "\nHost *\n  IgnoreUnknown FooBarOpt\n  FooBarOpt yes\n  ForwardX11 yes\n  AddKeysToAgent yes\n"
	if err := os.WriteFile(user, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), sshPath, "-G", "-F", user, "devvm-web").CombinedOutput() //nolint:gosec // the test's own config
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, out)
	}
	for _, want := range []string{"forwardx11 no", "addkeystoagent false", "permitremoteopen none", "port 60022", "hostkeyalias " + web} {
		if !slices.Contains(strings.Split(string(out), "\n"), want) {
			t.Errorf("ssh -G lacks %q:\n%s", want, out)
		}
	}
}

// A running VM whose backend gives no host or no port, or a host other
// than this machine's loopback, gets no entry: ssh would connect
// somewhere else.
func TestWriteNoAddress(t *testing.T) {
	t.Parallel()
	c, f := testConfig(t)
	f.Instances = map[vmname.VM]backend.Instance{web: {Name: web, Status: backend.StatusRunning}}
	made(t, c.Dirs, web)
	for _, target := range []backend.SSHTarget{
		{Host: "127.0.0.1", User: "u", Key: "/x/key"},
		{Port: 60022, User: "u", Key: "/x/key"},
		{Host: "10.0.0.2", Port: 22, User: "u", Key: "/x/key"},
		{Host: "localhost", Port: 60022, User: "u", Key: "/x/key"},
		{Host: "evil.example", Port: 60022, User: "u", Key: "/x/key"},
		{Host: "0.0.0.0", Port: 60022, User: "u", Key: "/x/key"},
		{Host: "127.0.0.1", Port: 60022, Key: "/x/key"},
		{Host: "127.0.0.1", Port: 60022, User: "u"},
	} {
		f.Target = target
		if err := c.Write(t.Context(), up(f)); err != nil {
			t.Fatal(err)
		}
		if got := read(t, c.Dirs.SSHConfig()); got != header {
			t.Errorf("%+v: %q", target, got)
		}
	}
}

// In a terminal, the Include line is added once said yes to, at the top
// of the user's config, before its Host blocks, and its ~/.ssh made the
// user's alone; no leaves the file as it was. --ssh-config adds it
// unasked, and a linked config stays linked, its file written.
func TestEnsureIncludeAsked(t *testing.T) {
	t.Parallel()
	c, _ := testConfig(t)
	ui := &msg.Fake{Terminal: true}
	c.Reporter = ui
	home := t.TempDir()
	cfg := filepath.Join(home, ".ssh", "config")
	asked, answer := 0, false
	ui.OnConfirm = func(msg.M) bool { asked++; return answer }
	if ok, err := c.EnsureInclude(home); ok || err != nil || asked != 1 {
		t.Errorf("no: %v, %v, asked %d", ok, err, asked)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no wrote the config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o750); err != nil {
		t.Fatal(err)
	}
	mine := "Host *\n  ForwardAgent no\n"
	if err := os.WriteFile(cfg, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	answer = true
	if ok, err := c.EnsureInclude(home); !ok || err != nil || asked != 2 {
		t.Errorf("yes: %v, %v, asked %d", ok, err, asked)
	}
	if got := read(t, cfg); got != c.IncludeLine()+"\n"+mine {
		t.Errorf("~/.ssh/config: %q", got)
	}
	if fi, err := os.Stat(filepath.Dir(cfg)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("~/.ssh: %v, %v", fi, err)
	}

	home2 := t.TempDir()
	target := filepath.Join(t.TempDir(), "dotfiles-config")
	if err := os.WriteFile(target, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home2, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home2, ".ssh", "config")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c.AddInclude, ui.Terminal = true, false
	if ok, err := c.EnsureInclude(home2); !ok || err != nil || asked != 2 {
		t.Errorf("--ssh-config: %v, %v, asked %d", ok, err, asked)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link: %v, %v", fi, err)
	}
	if got := read(t, target); got != c.IncludeLine()+"\n"+mine {
		t.Errorf("the linked file: %q", got)
	}
}

// Without a terminal, the Include line isn't added: it says to. Once
// there, anywhere, nothing is asked.
func TestEnsureInclude(t *testing.T) {
	t.Parallel()
	c, _ := testConfig(t)
	home := t.TempDir()
	cfg := filepath.Join(home, ".ssh", "config")
	ok, err := c.EnsureInclude(home)
	if ok || err != nil {
		t.Errorf("EnsureInclude without a terminal = %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("~/.ssh made: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := "Host *\n  ForwardAgent no\n" + c.IncludeLine() + "\n"
	if err := os.WriteFile(cfg, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.EnsureInclude(home); !ok || err != nil {
		t.Errorf("EnsureInclude with the line = %v, %v", ok, err)
	}
	if got := read(t, cfg); got != mine {
		t.Errorf("~/.ssh/config changed: %q", got)
	}
	if want := "Include " + filepath.Join(c.Dirs.Data, "ssh_config"); c.IncludeLine() != want {
		t.Errorf("IncludeLine = %q, want %q", c.IncludeLine(), want)
	}
}
