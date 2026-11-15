package registry

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// TestE2EExecQuoting checks, on a real running VM, that what its
// backend's Exec runs reaches the guest as given: arguments with spaces,
// quotes, $, newlines and globs, a folder with a space, and input. The
// backend is the one the VM's state records, so one test covers every
// backend. tests/e2e runs it on a project VM it made, named by
// DEVVM_E2E_VM, in its sandbox; otherwise it's skipped.
func TestE2EExecQuoting(t *testing.T) {
	name := vmname.VM(os.Getenv("DEVVM_E2E_VM"))
	if name == "" {
		t.Skip("tests/e2e runs it, on a VM of its own")
	}
	d, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(d, assets.FS, &msg.Fake{}, Overrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := state.ReadVM(d, name)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Exists() {
		t.Fatalf("%s has no state", name)
	}
	c, err := r.For(v)
	if err != nil {
		t.Fatal(err)
	}
	b := c.Backend

	args := []string{
		"a b", "  two  spaces ", `"double"`, "'single'", `it's`, "$HOME", "${HOME}", "$(id -u)", "`id -u`",
		"a\nb", "tab\there", "*", "?", "[ab]", "/*", "~", "~root", `back\slash`, `\n`, "", "-n", "--",
		";", "&&", "|", ">x", "<x", "#c", "!", "%s", "é ünï",
	}
	var out, errOut bytes.Buffer
	argv := append([]string{"printf", `%s\0`}, args...)
	if err := b.Exec(t.Context(), name, argv, backend.ExecOpts{Stdout: &out, Stderr: &errOut}); err != nil {
		t.Fatalf("%s's Exec: %v\n%s", b.Name(), err, errOut.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\x00"), "\x00")
	if !slices.Equal(got, args) {
		t.Errorf("%s: the guest got %q,\nwant %q", b.Name(), got, args)
	}

	// A folder with a space, quote and $ in its name: made there, then
	// run in.
	dir := "/tmp/devvm e2e 'q' $x"
	if err := b.Exec(t.Context(), name, []string{"mkdir", "-p", dir}, backend.ExecOpts{Stderr: &errOut}); err != nil {
		t.Fatalf("mkdir: %v\n%s", err, errOut.String())
	}
	out.Reset()
	if err := b.Exec(t.Context(), name, []string{"pwd"}, backend.ExecOpts{Dir: dir, Stdout: &out, Stderr: &errOut}); err != nil {
		t.Fatalf("pwd: %v\n%s", err, errOut.String())
	}
	if got := strings.TrimSuffix(out.String(), "\n"); got != dir {
		t.Errorf("%s: pwd in %q = %q", b.Name(), dir, got)
	}
	// Input reaches it too.
	out.Reset()
	if err := b.Exec(t.Context(), name, []string{"cat"}, backend.ExecOpts{Stdin: strings.NewReader("in\n$x\n"), Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "in\n$x\n" {
		t.Errorf("%s: cat = %q", b.Name(), out.String())
	}
}
