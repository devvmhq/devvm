package vmssh

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Host keys as ssh's wire format has them: each decodes to its own type.
const (
	keyED  = "AAAAC3NzaC1lZDI1NTE5AAAAIGRldnZtLXRlc3QtaG9zdC1rZXktYWFhYWFhYWFhYWFh"
	keyED2 = "AAAAC3NzaC1lZDI1NTE5AAAAIGRldnZtLXRlc3QtaG9zdC1rZXktYmJiYmJiYmJiYmJi"
	keyEC  = "AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAAABBA=="
	keySK  = "AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIGRldnZtLXRlc3QtaG9zdC1rZXktY2NjY2NjY2NjY2NjAAAABHNzaDo="
)

// One parser for every key type: a line is a key only when its key
// decodes and names its own type; certificates, markers, patterns and
// anything else are dropped.
func TestParseHostKeys(t *testing.T) {
	out := strings.Join([]string{
		"ssh-ed25519 " + keyED + " root@vm",
		"ecdsa-sha2-nistp256 " + keyEC,
		"sk-ssh-ed25519@openssh.com " + keySK + " a comment",
		"",
		"ssh-ed25519",
		"ssh-rsa " + keyED,                     // the key is another type's
		"ssh-ed25519 " + keyED + "x",           // not base64
		"ssh-ed25519 AAAA",                     // too short for its type
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",     // cut inside the type
		"@cert-authority ssh-ed25519 " + keyED, // a marker
		"*-evil " + keyED,                      // a pattern
		"ssh-ed25519-cert-v01@openssh.com " + keyED, // a certificate
		"ssh-ed25519\"x " + keyED,
		"ssh-ed25519 " + strings.Repeat("A", 8<<10), // over keyMax
	}, "\n")
	want := []HostKey{{"ssh-ed25519", keyED}, {"ecdsa-sha2-nistp256", keyEC}, {"sk-ssh-ed25519@openssh.com", keySK}}
	if got := ParseHostKeys([]byte(out)); !slices.Equal(got, want) {
		t.Errorf("ParseHostKeys = %q, want %q", got, want)
	}
	w := &capped{max: 4}
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil || w.String() != "abcd" {
		t.Errorf("capped: %d, %v, %q", n, err, w.String())
	}
	// io.Copy, as os/exec copies a command's output, goes through Write.
	w = &capped{max: 4}
	if _, err := io.Copy(w, strings.NewReader(strings.Repeat("x", 1<<20))); err != nil || w.String() != "xxxx" {
		t.Errorf("copied: %q, %v", w.String(), err)
	}
}

// hostKeysFake is a fake with ssh whose running VMs show key as their
// host key.
func hostKeysFake(t *testing.T, vm vmname.VM, key *string) (*backendtest.Fake, paths.Dirs) {
	t.Helper()
	f := backendtest.New("fake", backend.Caps{backend.FeatureSSH: backend.Native})
	f.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusRunning}
	f.ExecFunc = func(_ vmname.VM, argv []string, o backend.ExecOpts) error {
		if !slices.Equal(argv, []string{"sh", "-c", hostKeyScript}) {
			return errors.New("unexpected command")
		}
		_, err := io.WriteString(o.Stdout, *key+"\nnot a key\n")
		return err
	}
	return f, paths.Dirs{Data: t.TempDir()}
}

func execs(f *backendtest.Fake) int {
	n := 0
	for _, m := range f.Methods() {
		if m == "Exec" {
			n++
		}
	}
	return n
}

// A VM's host keys are read once for each start, into its one record:
// another start's record is read again, as is one devvm forgot, and one
// for this start is used as it is. Links' sessions use it, never reading.
func TestPinHostKeys(t *testing.T) {
	const vm = "devvm-p-0123456789abcd"
	key := "ssh-ed25519 " + keyED + " root@vm"
	f, d := hostKeysFake(t, vm, &key)
	ctx := t.Context()
	path := d.VM(vm).KnownHosts()
	if _, err := KnownHostsFor(ctx, d, f, vm); !errors.Is(err, ErrNoHostKeys) {
		t.Errorf("before any pin: %v", err)
	}
	got, err := PinHostKeys(ctx, d, f, vm)
	want := "# devvm: host keys of start start-0\n" + vm + " ssh-ed25519 " + keyED + "\n"
	if err != nil || got != path || readFile(t, path) != want {
		t.Fatalf("pinned %q, %v: %q", got, err, readFile(t, path))
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("its mode: %v, %v", fi, err)
	}
	if got, err := KnownHostsFor(ctx, d, f, vm); err != nil || got != path {
		t.Errorf("KnownHostsFor = %q, %v", got, err)
	}
	n := execs(f)
	if _, err := PinHostKeys(ctx, d, f, vm); err != nil || execs(f) != n {
		t.Errorf("pinned again: %v, %d more reads", err, execs(f)-n)
	}

	// Another start: read again, and links' sessions refuse the old record.
	key = "ssh-ed25519 " + keyED2
	f.Restart(vm)
	if _, err := KnownHostsFor(ctx, d, f, vm); !errors.Is(err, ErrNoHostKeys) {
		t.Errorf("another start's record: %v", err)
	}
	if _, err := PinHostKeys(ctx, d, f, vm); err != nil || readFile(t, path) != "# devvm: host keys of start start-1\n"+vm+" ssh-ed25519 "+keyED2+"\n" {
		t.Errorf("after a start: %v, %q", err, readFile(t, path))
	}

	// Forgotten: read again. A file that isn't a record is read again too.
	if err := ForgetHostKeys(d, vm); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("forgotten: %v", err)
	}
	if err := ForgetHostKeys(d, vm); err != nil {
		t.Errorf("forgetting none: %v", err)
	}
	if err := os.WriteFile(path, []byte(vm+" ssh-ed25519 "+keyED+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n = execs(f)
	if _, err := PinHostKeys(ctx, d, f, vm); err != nil || execs(f) != n+1 || !strings.HasPrefix(readFile(t, path), startLine+"start-1\n") {
		t.Errorf("not a record: %v, %d reads, %q", err, execs(f)-n, readFile(t, path))
	}
}

// restartsOnExec is a fake whose VM starts again while it shows its keys.
type restartsOnExec struct{ *backendtest.Fake }

func (b restartsOnExec) Exec(ctx context.Context, vm vmname.VM, argv []string, o backend.ExecOpts) error {
	err := b.Fake.Exec(ctx, vm, argv, o)
	b.Restart(vm)
	return err
}

// None is pinned for a VM that doesn't run, whose start isn't known, that
// shows no keys or can't be asked, or that starts again while it's asked;
// and a record that can't be read fails.
func TestPinHostKeysNone(t *testing.T) {
	const vm = "devvm-p-0123456789abcd"
	key := "ssh-ed25519 " + keyED
	f, d := hostKeysFake(t, vm, &key)
	ctx := t.Context()
	pin := func(b backend.Backend, name string) {
		t.Helper()
		if got, err := PinHostKeys(ctx, d, b, vm); !errors.Is(err, ErrNoHostKeys) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
		if _, err := os.Stat(d.VM(vm).KnownHosts()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: recorded: %v", name, err)
		}
	}
	pin(restartsOnExec{f}, "started again")
	key = "nothing here"
	pin(f, "no keys")
	key = "ssh-ed25519 " + keyED
	f.Errs = map[string]error{"Exec": errors.New("no answer")}
	pin(f, "no answer")
	f.Errs = map[string]error{"Running": errors.New("no backend")}
	pin(f, "Running fails")
	f.Errs = nil
	f.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusStopped}
	pin(f, "stopped")
	f.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusRunning}

	if err := os.MkdirAll(d.VM(vm).KnownHosts(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PinHostKeys(ctx, d, f, vm); err == nil || errors.Is(err, ErrNoHostKeys) {
		t.Errorf("an unreadable record: %v", err)
	}
	if _, err := KnownHostsFor(ctx, d, f, vm); err == nil || errors.Is(err, ErrNoHostKeys) {
		t.Errorf("an unreadable record, for links: %v", err)
	}
	if err := os.Remove(d.VM(vm).KnownHosts()); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := PinHostKeys(cancelled, d, f, vm); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// bothRead holds each Exec until two have started, so both devvms have
// found no record before either writes one.
type bothRead struct {
	*backendtest.Fake
	both *sync.WaitGroup
}

func (b bothRead) Exec(ctx context.Context, vm vmname.VM, argv []string, o backend.ExecOpts) error {
	b.both.Done()
	b.both.Wait()
	return b.Fake.Exec(ctx, vm, argv, o)
}

// Two devvms pinning one VM's keys at once write one record, whole.
func TestPinHostKeysTogether(t *testing.T) {
	const vm = "devvm-p-0123456789abcd"
	key := "ssh-ed25519 " + keyED
	f, d := hostKeysFake(t, vm, &key)
	var both, wg sync.WaitGroup
	both.Add(2)
	for range 2 {
		wg.Go(func() {
			if _, err := PinHostKeys(t.Context(), d, bothRead{f, &both}, vm); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := readFile(t, d.VM(vm).KnownHosts()); got != startLine+"start-0\n"+vm+" ssh-ed25519 "+keyED+"\n" {
		t.Errorf("record: %q", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}
