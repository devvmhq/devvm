package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
)

// The features table has a row per feature with the backend's support; a
// feature the backend doesn't list is none.
func TestWriteFeatures(t *testing.T) {
	b := backendtest.New("lima", backend.Caps{
		backend.FeatureSSH: backend.Native, backend.FeatureDisk: backend.Emulated(false), backend.FeaturePorts: backend.Emulated(true),
	})
	out, colored := tables(t, func(tab *ui.Table) error { return writeFeatures(tab, b) })
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(backend.Features)+1 {
		t.Fatalf("%d lines:\n%s", len(lines), out)
	}
	for _, want := range []string{"FEATURE SUPPORT", "ssh native", "disk unreviewed", "ports emulated", "cpus none"} {
		found := false
		for _, l := range lines {
			found = found || strings.Join(strings.Fields(l), " ") == want
		}
		if !found {
			t.Errorf("no line %q in\n%s", want, out)
		}
	}
	wantCodes(t, colored, "\033[1mFEATURE\033[0m", "\033[32mnative\033[0m", "\033[33memulated\033[0m",
		"\033[31munreviewed\033[0m", "\033[2mnone\033[0m")
}

// doctor warns when the devvm first on PATH isn't this one, links
// resolved; not when it is, or when there's none on PATH.
func TestOnPathOther(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "lib", "devvm")
	brew := filepath.Join(dir, "brew", "devvm")
	link := filepath.Join(dir, "bin", "devvm")
	for _, f := range []string{self, brew} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	found := func(p string) func(string) (string, error) {
		return func(string) (string, error) { return p, nil }
	}
	notFound := func(string) (string, error) { return "", exec.ErrNotFound }
	for _, c := range []struct {
		name     string
		lookPath func(string) (string, error)
		self     string
		want     string
	}{
		{"this one, through its link", found(link), self, ""},
		{"this one, run through its link", found(self), link, ""},
		{"another", found(brew), link, "devvm_other_first first=" + brew + " self=" + self},
		{"none on PATH", notFound, self, ""},
		{"a broken link on PATH", found(filepath.Join(dir, "gone")), self, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, ok := onPath{LookPath: c.lookPath, Executable: func() (string, error) { return c.self, nil }}.other()
			got := ""
			if ok {
				got = m.Names()
			}
			if got != c.want {
				t.Errorf("other: %q, want %q", got, c.want)
			}
		})
	}
}

// --json has each problem's key, fix (null without one) and message, the
// warnings, and every backend's features; it prints nothing else.
func TestDoctorReport(t *testing.T) {
	t.Parallel()
	b := backendtest.New("lima", backend.Caps{backend.FeatureSSH: backend.Native})
	b.Missing = []backend.Problem{
		{Key: "lima", Fix: "brew install lima", Msg: msg.ProgramMissingFix("brew install lima", "Lima", "limactl")},
		{Key: "kvm", Msg: msg.NoKVM()},
	}
	d := doctor{
		Path:     onPath{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }, Executable: os.Executable},
		Free:     func(string) (uint64, error) { return 1 << 40, nil },
		Host:     func() (vm.Host, error) { return vm.Host{CPUs: 2, MemoryGiB: 16}, nil },
		Policies: func() []msg.M { return []msg.M{msg.PolicyNotRule("/p", "2", "a b")} },
		Folders:  func() []msg.M { return []msg.M{msg.VMFolderStateless("/vms/0123456789abcd")} },
		GitBare:  func() bool { return false },
		Proxies:  func() []msg.M { return []msg.M{msg.ProxyUnconfinedNoSandbox("devvm-web-0123456789abcd")} },
	}
	r := d.report(t.Context(), msg.M.Names, []backend.Backend{b}, b)
	fix := "brew install lima"
	want := []problemJSON{{Key: "lima", Fix: &fix, Message: "program_missing_fix fix=brew install lima name=Lima program=limactl"}, {Key: "kvm", Message: "no_kvm"}}
	if len(r.Problems) != 2 || r.Problems[0].Fix == nil || *r.Problems[0].Fix != fix || r.Problems[1].Fix != nil ||
		r.Problems[0].Message != want[0].Message || r.Problems[1].Key != "kvm" {
		t.Errorf("problems %+v, want %+v", r.Problems, want)
	}
	if !slices.Equal(r.Warnings, []string{
		"host_cpus_few have=2 want=4", "policy_not_rule file=/p line=2 rule=a b", "vm_folder_stateless path=/vms/0123456789abcd",
		"proxy_unconfined_no_sandbox vm=devvm-web-0123456789abcd",
	}) || r.Backend != "lima" ||
		!slices.Equal(r.Tips, []string{"doctor_git_bare"}) {
		t.Errorf("warnings %q, backend %q", r.Warnings, r.Backend)
	}
	if r.Features["lima"]["ssh"] != "native" || r.Features["lima"]["disk"] != "none" {
		t.Errorf("features %v", r.Features)
	}
	u, out, errOut := testUI()
	if err := printJSON(u, r); err != nil || errOut.Len() > 0 {
		t.Fatalf("printJSON: %v, stderr %q", err, errOut)
	}
	// install.sh reads this layout: a field a line, the problems' indented
	// six spaces.
	layout := "\n  \"problems\": [\n    {\n      \"key\": \"lima\",\n      \"fix\": \"brew install lima\",\n" +
		"      \"message\": \"program_missing_fix fix=brew install lima name=Lima program=limactl\"\n    },\n    {\n      \"key\": \"kvm\",\n      \"fix\": null,\n"
	if !strings.Contains(out.String(), layout) || !strings.HasPrefix(out.String(), "{\n  \"version\": 1,\n") {
		t.Errorf("printed %s", out)
	}
}

// A machine below the recommended host gets a warning for each way it
// is: less memory, fewer CPUs. None when it isn't, or can't say.
func TestSmallHost(t *testing.T) {
	for _, c := range []struct {
		host vm.Host
		err  error
		want []string
	}{
		{vm.Host{CPUs: 8, MemoryGiB: 16}, nil, nil},
		{vm.Host{CPUs: 4, MemoryGiB: 8}, nil, nil},
		{vm.Host{CPUs: 4, MemoryGiB: 7}, nil, []string{"host_memory_low have=7 want=8"}},
		{vm.Host{CPUs: 2, MemoryGiB: 8}, nil, []string{"host_cpus_few have=2 want=4"}},
		{vm.Host{CPUs: 1, MemoryGiB: 4}, nil, []string{"host_memory_low have=4 want=8", "host_cpus_few have=1 want=4"}},
		{vm.Host{}, errors.ErrUnsupported, nil},
	} {
		var got []string
		for _, m := range smallHost(func() (vm.Host, error) { return c.host, c.err }) {
			got = append(got, m.Names())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("host %+v, %v: %q, want %q", c.host, c.err, got, c.want)
		}
	}
}

// doctor's warnings about a small host go to stderr and never change
// what it returns.
func TestDoctorSmallHostWarnsOnly(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	notFound := func(string) (string, error) { return "", exec.ErrNotFound }
	for _, c := range []struct {
		name     string
		host     vm.Host
		problems []backend.Problem
		wantErr  string // the message doctor returns; "" for none
		warned   bool   // whether stderr has the small host's warnings
	}{
		{"big, nothing missing", vm.Host{CPUs: 8, MemoryGiB: 16}, nil, "", false},
		{"small, nothing missing", vm.Host{CPUs: 2, MemoryGiB: 4}, nil, "", true},
		{"small, Lima missing", vm.Host{CPUs: 2, MemoryGiB: 4}, []backend.Problem{{Key: "lima", Msg: msg.ProgramMissing("Lima", "limactl")}}, "program_missing name=Lima program=limactl", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := backendtest.New("lima", backend.Caps{})
			b.Missing = c.problems
			u, _, errOut := testUI()
			d := doctor{
				Path: onPath{LookPath: notFound, Executable: os.Executable},
				Free: func(string) (uint64, error) { return 1 << 40, nil },
				Host: func() (vm.Host, error) { return c.host, nil },
				Dirs: paths.FromEnv,
			}
			err := d.run(t.Context(), u, b)
			if err != nil && msg.IDOf(err) == "" {
				t.Fatal(err)
			}
			if gotErr := msg.NamesOf(err); gotErr != c.wantErr {
				t.Errorf("returned %q, want %q", gotErr, c.wantErr)
			}
			if warned := strings.Contains(errOut.String(), "devvm: host_cpus_few have=2 want=4\n") &&
				strings.Contains(errOut.String(), "devvm: host_memory_low have=4 want=8\n"); warned != c.warned {
				t.Errorf("stderr %q: warned %v, want %v", errOut.String(), warned, c.warned)
			}
			if !c.warned && c.problems == nil && errOut.Len() > 0 {
				t.Errorf("stderr %q, want nothing", errOut.String())
			}
		})
	}
}
