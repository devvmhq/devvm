package backend

import (
	"errors"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// Only a confined VM's guest has its own loopback as its one name server;
// an open VM, with or without devvm's proxy, and a build keep their
// network's own.
func TestResolver(t *testing.T) {
	for _, tc := range []struct {
		c    Confinement
		want string
	}{
		{Confinement{Net: NetProxy, ProxyPort: 3128}, "127.0.0.1"},
		{Confinement{Net: NetDirect}, ""},
		{Confinement{Net: NetDirect, ProxyPort: 3128}, ""},
		{Confinement{Net: NetInternet}, ""},
		{Confinement{Net: NetNone}, ""},
	} {
		if got := tc.c.Resolver(); got != tc.want {
			t.Errorf("%+v: Resolver() = %q, want %q", tc.c.Net, got, tc.want)
		}
	}
}

// A start refuses only a disk smaller than the VM's: zero is the VM's
// own, and the rest changes.
func TestValidate(t *testing.T) {
	in := Instance{Name: "v", Size: Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}}
	for _, tc := range []struct {
		want Want
		err  string
	}{
		{Want{}, ""},
		{Want{Size: Size{CPUs: 1, MemoryGiB: 2, DiskGiB: 100}, AgentForwarding: true}, ""},
		{Want{Size: Size{DiskGiB: 120.5}}, ""},
		{Want{Size: Size{DiskGiB: 99.5}}, "disk_shrink have=100 vm=v want=99.5"},
	} {
		if got := msg.NamesOf(Validate(tc.want, in)); got != tc.err {
			t.Errorf("Validate(%+v) = %q, want %q", tc.want, got, tc.err)
		}
	}
}

// A logged failure is still its message, for errors.As and printing.
func TestLoggedError(t *testing.T) {
	m := msg.StartFailed("v")
	err := Logged(m)
	if le, ok := errors.AsType[*LoggedError](err); !ok || le.Msg.ID() != "start_failed" {
		t.Errorf("as a LoggedError: %v", err)
	}
	if msg.IDOf(err) != m.ID() || err.Error() != m.Error() {
		t.Errorf("as its message: %v", err)
	}
}

// A start's RemountError is still its message, for errors.As and
// printing, where its caller doesn't start the VM again.
func TestRemountError(t *testing.T) {
	m := msg.MountCacheUnmounted("v")
	var err error = &RemountError{Msg: m}
	if msg.IDOf(err) != m.ID() || err.Error() != m.Error() {
		t.Errorf("as its message: %v", err)
	}
}
