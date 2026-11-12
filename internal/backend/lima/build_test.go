package lima

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/settings"
)

const (
	baseKey  = "0123456789ab"
	layerKey = "ba9876543210"
)

// buildLima is testLima with devvm's files in a temporary folder, a fixed
// Ubuntu image and a 16 GiB host. Each create or clone adds its instance,
// stopped, to what limactl list reports; parent is listed from the start
// with parentStatus, unless that's "".
func buildLima(t *testing.T, os string, parentStatus string) (*Lima, *fakeRunner) {
	t.Helper()
	l, f, _ := testLima(t, os)
	l.Assets = fstest.MapFS{"base.yaml": {Data: []byte("containerd:\n  user: false\n")}}
	l.Memory = func() (uint64, error) { return 16 * gib, nil }
	l.CPUs = func() int { return 8 }
	list := ""
	if parentStatus != "" {
		list = `{"name":"devvm-img-` + baseKey + `","status":"` + parentStatus + `"}` + "\n"
	}
	f.out["limactl list --json"] = list
	f.do = func(c call) {
		line := c.line()
		switch {
		case strings.HasPrefix(line, "limactl create"), strings.HasPrefix(line, "limactl clone"):
			name := c.argv[len(c.argv)-1]
			if strings.HasPrefix(line, "limactl create") {
				name = strings.TrimPrefix(c.argv[4], "--name=")
			}
			f.out["limactl list --json"] += `{"name":"` + name + `","status":"Stopped"}` + "\n"
		case strings.HasPrefix(line, "limactl stop devvm-img-"+baseKey):
			f.out["limactl list --json"] = strings.Replace(f.out["limactl list --json"], parentStatus, "Stopped", 1)
		}
	}
	return l, f
}

// baseSpec is base's build from Ubuntu on l, its origin l's own and its
// one step base.sh.
func baseSpec(t *testing.T, l *Lima) backend.ImageSpec {
	t.Helper()
	origin, err := l.Origin(nil)
	if err != nil {
		t.Fatal(err)
	}
	return backend.ImageSpec{
		Image: "base", Key: baseKey, Net: backend.NetInternet,
		Recipe: image.Recipe{Origin: origin, Steps: []image.Step{{Name: "base.sh", Data: []byte("#!/bin/bash\n")}}},
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("call %d: %q, want it to start %q", i, got[i], want[i])
		}
	}
}

// base: a VM from Ubuntu's image with base.yaml on limactl create's
// input, from devvm's files (none written out), nothing mounted, started
// in its build sandbox, its script run, flushed and stopped; then what
// its run left is deleted.
func TestBuildImageBase(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	inst := "devvm-img-" + baseKey
	var profile []byte
	do := f.do
	f.do = func(c call) {
		if c.argv[0] == "sandbox-exec" {
			profile, _ = os.ReadFile(l.Dirs.VM(inst).Profile())
		}
		do(c)
	}
	built, err := l.BuildImage(t.Context(), baseSpec(t, l))
	if err != nil {
		t.Fatal(err)
	}
	img := testUbuntuURL + "/release-20260101/ubuntu-26.04-server-cloudimg-arm64.img"
	if want := (backend.Built{Origin: img + " sha256:" + testUbuntuSum, Libc: "gnu"}); built != want {
		t.Errorf("Built = %+v, want %+v", built, want)
	}
	checkLines(t, f.lines(), []string{
		"limactl create --tty=false --mount-none --name=" + inst +
			` --set .images = [{"location":"` + img + `","arch":"aarch64","digest":"sha256:` + testUbuntuSum + `"}]` +
			" --cpus=4 --memory=8 --disk=100 -",
		"limactl edit --tty=false " + inst + " --set .ssh.localPort=",
		"sandbox-exec -f " + l.Dirs.VM(inst).Profile() + " ",
		"limactl shell " + inst + " bash -s",
		"limactl shell --workdir / " + inst + " sync",
		"limactl stop " + inst,
	})
	for _, c := range f.calls {
		if strings.HasPrefix(c.line(), "limactl create") && c.stdin != "containerd:\n  user: false\n" {
			t.Errorf("limactl create's input: %q, want base.yaml", c.stdin)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Dirs.Data, "assets")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("devvm's files written out: %v", err)
	}
	if !strings.Contains(string(profile), `(deny network-outbound (remote ip "localhost:*"))`) {
		t.Errorf("the build's profile: %s", profile)
	}
	if _, err := os.Stat(l.Dirs.VM(inst).Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the build VM's folder after its build: %v", err)
	}
}

// A layer: its parent stopped first, then a clone of it. On Linux hosts
// there's no sandbox for a build with the internet, and the image's disk is
// read-only once built.
func TestBuildImageLayer(t *testing.T) {
	l, f := buildLima(t, "linux", "Running")
	inst := "devvm-img-" + layerKey
	writeTestFile(t, l.disk(inst), "qcow2")
	built, err := l.BuildImage(t.Context(), backend.ImageSpec{
		Image: "foo", Key: layerKey, Parent: baseKey, Net: backend.NetInternet,
		Recipe: image.Recipe{Steps: []image.Step{
			{Name: "foo.sh", Data: []byte("#!/bin/bash\necho foo\n")},
			{Name: "args.sh", Data: []byte("echo \"$1\"\n"), Args: []string{"a", "b c"}},
			{Name: "root.sh", Data: []byte("id -u\n"), Root: true},
		}},
		Trust: backend.Trust{Add: backend.Script{Name: "ca.sh", Data: []byte("add\n")}, Remove: backend.Script{Name: "ca.sh", Data: []byte("rm\n")}, Env: []byte("export CA=1\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if built != (backend.Built{}) {
		t.Errorf("Built = %+v, want nothing above base", built)
	}
	checkLines(t, f.lines(), []string{
		"limactl stop devvm-img-" + baseKey,
		"limactl clone --tty=false --mount-none devvm-img-" + baseKey + " " + inst,
		"limactl edit --tty=false " + inst + " --set .ssh.localPort=",
		"limactl start --tty=false " + inst,
		"limactl shell --workdir / " + inst + " sudo sh -s",
		"limactl shell " + inst + " bash -s",
		"limactl shell " + inst + " bash -s -- a b c",
		"limactl shell --workdir / " + inst + " sudo bash -s",
		"limactl shell --workdir / " + inst + " sudo sh -s",
		"limactl shell --workdir / " + inst + " sync",
		"limactl stop " + inst,
	})
	// Each step as it is, after the CAs' variables; the CAs' own scripts
	// without them.
	var inputs []string
	for _, c := range f.calls {
		if slices.Contains(c.argv, "-s") {
			inputs = append(inputs, c.stdin)
		}
	}
	if want := []string{"add\n", "export CA=1\n#!/bin/bash\necho foo\n", "export CA=1\necho \"$1\"\n", "export CA=1\nid -u\n", "rm\n"}; !slices.Equal(inputs, want) {
		t.Errorf("inputs %q, want %q", inputs, want)
	}
	for _, c := range f.calls {
		if i := slices.Index(c.argv, "--"); i >= 0 && !slices.Equal(c.argv[i+1:], []string{"a", "b c"}) {
			t.Errorf("args.sh's arguments: %q", c.argv)
		}
	}
	if fi, err := os.Stat(l.disk(inst)); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("the image's disk: %v, %v", fi.Mode(), err)
	}
}

// Behind the host's proxy, a build's scripts get its variables on their
// input, a loopback proxy as the guest reaches the host, and sudo keeps
// them while the scripts run: the proxy is in no argument, and the build
// VM gets none of the host's settings itself.
// fakeProxy makes l's build proxies sleeps that say they listen on port
// 40000, recording each one's arguments in spawned; stopped gets each one's
// end.
func fakeProxy(t *testing.T, l *Lima) (spawned *[][]string, stopped chan struct{}) {
	t.Helper()
	spawned, stopped = &[][]string{}, make(chan struct{}, 4)
	l.Spawn = func(c daemon.Cmd) (*daemon.Handle, error) {
		// Its arguments, then its config as it starts: its folder goes with
		// a build that succeeds.
		args := append([]string{c.Path}, c.Args...)
		if len(c.Args) == 3 && c.Args[1] == "-config" {
			b, _ := os.ReadFile(c.Args[2])
			args = append(args, string(b))
		}
		*spawned = append(*spawned, args)
		h, err := daemon.Spawn(daemon.Cmd{Path: "sleep", Args: []string{"60"}, Out: c.Out})
		if err != nil {
			return nil, err
		}
		go func() { <-h.Done(); stopped <- struct{}{} }()
		t.Cleanup(func() { h.Stop(time.Second) })
		return h, daemon.WriteStatus(c.Status, h.PID(), "127.0.0.1:40000")
	}
	return spawned, stopped
}

// Behind a company proxy, a build goes through a proxy of devvm's, which
// alone has the company proxy's address and password (its -upstream
// file): the build's scripts get that proxy's address on Lima's host
// address with the build's own password, sudo keeps it, and on macOS the
// build VM's sandbox allows its port. The proxy stops once the build is
// done, and its folder goes.
func TestBuildImageHostProxy(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			l, f := buildLima(t, goos, "Running")
			env := map[string]string{"HTTPS_PROXY": "http://corp:pa55@127.0.0.1:3128", "no_proxy": ".corp"} //nolint:gosec // a fake company proxy's fake password
			l.Getenv = func(k string) string { return env[k] }
			// limactl runs without it too: Lima logs what it reads.
			t.Setenv("HTTPS_PROXY", env["HTTPS_PROXY"])
			spawned, stopped := fakeProxy(t, l)
			inst := "devvm-img-" + layerKey
			writeTestFile(t, l.disk(inst), "qcow2")
			var profile []byte
			do := f.do
			f.do = func(c call) {
				if c.argv[0] == "sandbox-exec" {
					profile, _ = os.ReadFile(l.Dirs.VM(inst).Profile())
				}
				do(c)
			}
			if _, err := l.BuildImage(t.Context(), backend.ImageSpec{
				Image: "foo", Key: layerKey, Parent: baseKey, Net: backend.NetInternet,
				Recipe: image.Recipe{Steps: []image.Step{{Name: "foo.sh", Data: []byte("#!/bin/bash\necho foo\n")}}},
			}); err != nil {
				t.Fatal(err)
			}
			if len(*spawned) != 1 {
				t.Fatalf("proxies: %v", *spawned)
			}
			args := (*spawned)[0]
			dir := l.Dirs.VM(inst).BuildProxy()
			var c proxy.Config
			if len(args) != 5 || args[3] != filepath.Join(dir, "proxy.json") || json.Unmarshal([]byte(args[4]), &c) != nil ||
				c.Lifeline != l.haPIDFile(inst) || !c.UpstreamUnresolved || c.Upstream == nil || c.Upstream.HTTPS != env["HTTPS_PROXY"] ||
				c.Upstream.NoProxy != ".corp" || strings.Contains(strings.Join(args[:4], " "), "pa55") {
				t.Errorf("the build's proxy: %q", args)
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Error("the build's proxy wasn't stopped")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("its folder stayed: %v", err)
			}
			var scripts []string
			for _, c := range f.calls {
				if strings.Contains(c.line(), "pa55") || strings.Contains(strings.Join(c.env, " "), "pa55") || strings.Contains(c.stdin, "pa55") {
					t.Errorf("the company proxy's password reaches the build VM: %s", c.line())
				}
				if strings.HasPrefix(c.line(), "limactl shell") && c.stdin != "" {
					scripts = append(scripts, c.line()+"\n"+c.stdin)
				}
			}
			if len(scripts) != 3 || !strings.HasPrefix(scripts[1], "limactl shell "+inst+" bash -s\nexport HTTPS_PROXY='http://devvm:") ||
				!strings.Contains(scripts[1], "@192.168.5.2:40000' https_proxy='http://devvm:") ||
				!strings.HasSuffix(scripts[1], "@192.168.5.2:40000' NO_PROXY='.corp' no_proxy='.corp'; #!/bin/bash\necho foo\n") {
				t.Errorf("scripts:\n%s", strings.Join(scripts, "\n--\n"))
			}
			if want := "limactl shell --workdir / " + inst + " sudo sh -s\nprintf '%s\\n' '" + sudoKeepProxy +
				"' > /etc/sudoers.d/devvm-build-proxy && chmod 440 /etc/sudoers.d/devvm-build-proxy\n"; len(scripts) == 3 && scripts[0] != want {
				t.Errorf("sudo: %s", scripts[0])
			}
			if !slices.ContainsFunc(f.lines(), func(l string) bool { return strings.Contains(l, ".propagateProxyEnv=false") }) {
				t.Error("the build VM gets the host's proxy settings from Lima")
			}
			if goos == "darwin" {
				if !strings.Contains(string(profile), `(deny network-outbound (remote ip "localhost:*"))`) ||
					!strings.HasSuffix(strings.TrimSpace(string(profile)), `(allow network-outbound (remote ip "localhost:40000"))`) {
					t.Errorf("profile: %s", profile)
				}
			}
		})
	}
}

// Only a VM on Lima's own network starts with the host's proxy settings in
// limactl's environment, for propagateProxyEnv.
func TestConfineProxyEnv(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		l, _, _ := testLima(t, goos)
		l.Getenv = func(k string) string {
			return map[string]string{"HTTPS_PROXY": "http://u:p@proxy.corp:8080", "no_proxy": ".corp"}[k] //nolint:gosec // a fake proxy's fake password
		}
		_, env, err := l.confine("vm", backend.Confinement{Net: backend.NetDirect}, 0, false)
		if err != nil || !slices.Contains(env, "HTTPS_PROXY=http://u:p@proxy.corp:8080") || !slices.Contains(env, "no_proxy=.corp") {
			t.Errorf("%s NetDirect: %v %v", goos, env, err)
		}
		_, env, err = l.confine("vm", backend.Confinement{Net: backend.NetInternet}, 2222, false)
		if err != nil || slices.ContainsFunc(env, func(e string) bool { return strings.Contains(strings.ToLower(e), "_proxy=") }) {
			t.Errorf("%s NetInternet: %v %v", goos, env, err)
		}
	}
	t.Setenv("HTTPS_PROXY", "http://u:p@proxy.corp:8080")
	t.Setenv("http_proxy", "http://u:p@proxy.corp:8080")
	l, _, _ := testLima(t, "darwin")
	if cmd := l.limactl(t.Context(), "list"); slices.ContainsFunc(cmd.Env, func(e string) bool { return strings.Contains(e, "proxy.corp") }) {
		t.Errorf("limactl's environment keeps the host's proxy: %v", cmd.Env)
	}
}

// Without the proxy settings in devvm's environment, a build goes through
// config.toml's company proxy, its password the secret the build's proxy
// looks up itself, which must be saved (the backend's company proxy);
// a config.toml devvm can't read fails the build.
func TestBuildUpstreamConfig(t *testing.T) {
	l, _, _ := testLima(t, "linux")
	none := func(string) string { return "" }
	writeTestFile(t, l.Dirs.ConfigFile(), "[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\nno-proxy = \"corp.internal\"\n")
	if _, _, err := buildUpstream(t.Context(), l.Company, backend.NetInternet); msg.IDOf(err) != "proxy_password_missing" {
		t.Errorf("no password saved: %v", err)
	}
	var asked []string
	l.Company = settings.NewCompanyProxy(l.Dirs, none, func(_ context.Context, name string) (string, error) {
		asked = append(asked, name)
		return "fake-password", nil
	})
	host, up, err := buildUpstream(t.Context(), l.Company, backend.NetInternet)
	if err != nil || host.HTTPS != "http://alice@proxy.corp:3128" || host.NoProxy != "corp.internal" || up == nil || up.Secret != proxy.UpstreamSecret ||
		strings.Contains(fmt.Sprint(host, *up), "fake-password") || !slices.Equal(asked, []string{proxy.UpstreamSecret}) {
		t.Errorf("config.toml's: %+v, %+v, %v, asked %v", host, up, err, asked)
	}
	if _, up, err := buildUpstream(t.Context(), l.Company, backend.NetNone); up != nil || err != nil {
		t.Errorf("no internet: %+v, %v", up, err)
	}
	writeTestFile(t, l.Dirs.ConfigFile(), "")
	if host, up, err := buildUpstream(t.Context(), l.Company, backend.NetInternet); up != nil || err != nil || !host.None() {
		t.Errorf("no company proxy: %+v, %+v, %v", host, up, err)
	}
	writeTestFile(t, l.Dirs.ConfigFile(), "[proxy]\nhttps = 1\n")
	if _, _, err := buildUpstream(t.Context(), l.Company, backend.NetInternet); err == nil {
		t.Error("a broken config.toml was read")
	}
}

// A company proxy the build's proxy can't go through is refused before
// anything is made; without one, a build starts no proxy.
func TestBuildImageHostProxyRefused(t *testing.T) {
	l, f := buildLima(t, "linux", "Running")
	l.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "socks5://proxy.corp:1080"}[k] }
	_, err := l.BuildImage(t.Context(), backend.ImageSpec{Image: "foo", Key: layerKey, Parent: baseKey, Net: backend.NetInternet})
	if msg.NamesOf(err) != "upstream_proxy_scheme scheme=socks5:// setting=HTTPS_PROXY" {
		t.Errorf("BuildImage = %v", err)
	}
	if slices.ContainsFunc(f.lines(), func(l string) bool { return strings.HasPrefix(l, "limactl clone") }) {
		t.Errorf("made something: %v", f.lines())
	}
	// A build without the internet starts none either.
	l, _ = buildLima(t, "linux", "Running")
	l.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "http://proxy.corp:8080"}[k] }
	spawned, _ := fakeProxy(t, l)
	writeTestFile(t, l.disk("devvm-img-"+layerKey), "qcow2")
	if _, err := l.BuildImage(t.Context(), backend.ImageSpec{Image: "foo", Key: layerKey, Parent: baseKey, Net: backend.NetNone}); err != nil || len(*spawned) != 0 {
		t.Errorf("NetNone: %v, %v", err, *spawned)
	}
}

// A failed script is a logged failure naming it, with devvm's Lima home
// to look inside; the VM is stopped and kept, and nothing else runs.
func TestBuildImageScriptFails(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	inst := "devvm-img-" + baseKey
	f.fail = []string{"limactl shell " + inst + " bash -s"}
	_, err := l.BuildImage(t.Context(), baseSpec(t, l))
	e, ok := errors.AsType[*backend.LoggedError](err)
	if !ok || e.Msg.ID() != "script_failed" {
		t.Fatalf("BuildImage = %v, want a logged script_failed", err)
	}
	if want := msg.ScriptFailed("base.sh", inst).Names(); e.Msg.Names() != want {
		t.Errorf("BuildImage = %s, want %s", e.Msg.Names(), want)
	}
	lines := f.lines()
	if last := lines[len(lines)-1]; last != "limactl stop "+inst {
		t.Errorf("last call %q, want the VM stopped", last)
	}
	for _, line := range lines {
		if strings.Contains(line, "sync") || strings.Contains(line, "delete") {
			t.Errorf("carried on after the script failed: %s", line)
		}
	}
}

func TestBuildImageRefuses(t *testing.T) {
	base := backend.ImageSpec{Image: "base", Key: baseKey, Net: backend.NetInternet}
	for name, change := range map[string]func(*backend.ImageSpec){
		"--from":             func(s *backend.ImageSpec) { s.From = &backend.From{Ref: "node:24"} },
		"a missing parent":   func(s *backend.ImageSpec) { s.Parent = layerKey },
		"an existing image":  func(s *backend.ImageSpec) { s.Key = baseKey; s.Parent = "" },
		"no Ubuntu pinned":   func(*backend.ImageSpec) {},
		"another Ubuntu":     func(*backend.ImageSpec) {},
		"no host memory":     func(*backend.ImageSpec) {},
		"a missing base.yml": func(*backend.ImageSpec) {},
	} {
		t.Run(name, func(t *testing.T) {
			status := ""
			if name == "an existing image" {
				status = "Stopped"
			}
			l, f := buildLima(t, "darwin", status)
			switch name {
			case "no Ubuntu pinned":
				if err := os.Remove(l.Dirs.UbuntuPin()); err != nil {
					t.Fatal(err)
				}
			case "no host memory":
				l.Memory = func() (uint64, error) { return 0, errors.ErrUnsupported }
			case "a missing base.yml":
				l.Assets = fstest.MapFS{}
			}
			spec := base
			spec.Recipe.Origin, _ = l.Origin(nil) // none without base.yaml
			if name == "another Ubuntu" {
				// Pinned again since the recipe was made.
				pinUbuntu(t, l, testUbuntuURL, strings.Repeat("2", 64))
			}
			change(&spec)
			if _, err := l.BuildImage(t.Context(), spec); err == nil {
				t.Error("BuildImage succeeded")
			}
			for _, line := range f.lines() {
				if strings.HasPrefix(line, "limactl start") || strings.Contains(line, "limactl start") {
					t.Errorf("started a VM: %s", line)
				}
			}
		})
	}
}

// On a host of 2 CPUs and 8 GiB, base gets 2 CPUs and 4 GiB, and the
// build reports them as fitted; on a 16 GiB host of 8 CPUs, nothing is.
func TestBuildImageSmallHost(t *testing.T) {
	l, _ := buildLima(t, "darwin", "")
	built, err := l.BuildImage(t.Context(), baseSpec(t, l))
	if err != nil || built.Fitted != (backend.Size{}) {
		t.Errorf("on a big host: fitted %+v, %v; want none", built.Fitted, err)
	}
	l, f := buildLima(t, "darwin", "")
	l.CPUs = func() int { return 2 }
	l.Memory = func() (uint64, error) { return 8 * gib, nil }
	built, err = l.BuildImage(t.Context(), baseSpec(t, l))
	if err != nil {
		t.Fatal(err)
	}
	if want := (backend.Size{CPUs: 2, MemoryGiB: 4}); built.Fitted != want {
		t.Errorf("fitted %+v, want %+v", built.Fitted, want)
	}
	lines := f.lines()
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "limactl create ") || !strings.Contains(lines[0], " --cpus=2 --memory=4 ") {
		t.Errorf("calls:\n%s\nwant base created with --cpus=2 --memory=4", strings.Join(lines, "\n"))
	}
}

// devvm's own base, on a host with room for it, is created with 4 CPUs,
// 8 GiB of memory and a 100 GiB disk, all limactl create's flags: the
// base.yaml on its input sets none of them.
func TestBuildImageBaseSize(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	l.Assets = assets.FS
	if _, err := l.BuildImage(t.Context(), baseSpec(t, l)); err != nil {
		t.Fatal(err)
	}
	var create call
	for _, c := range f.calls {
		if strings.HasPrefix(c.line(), "limactl create") {
			create = c
		}
	}
	flag := func(name string) string {
		for _, a := range create.argv {
			if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
				return v
			}
		}
		return ""
	}
	for name, want := range map[string]string{"cpus": "4", "memory": "8", "disk": "100"} {
		if got := flag(name); got != want {
			t.Errorf("base's %s: %q, want %q (%s)", name, got, want, create.line())
		}
		for line := range strings.Lines(create.stdin) {
			if strings.HasPrefix(line, name+":") {
				t.Errorf("base.yaml sets %s: %q", name, line)
			}
		}
	}
}

func TestLimaArch(t *testing.T) {
	for goarch, want := range map[string]string{"arm64": "aarch64", "amd64": "x86_64", "riscv64": "riscv64"} {
		if got := limaArch(goarch); got != want {
			t.Errorf("limaArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}
