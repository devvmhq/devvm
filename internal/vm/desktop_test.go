package vm

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/desktop"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/state"
)

func desktopVM(t *testing.T) (paths.Dirs, *state.VM) {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	st := vmState(t, d, "devvm-p-00000000000000")
	return d, st
}

// The password is made once and kept; one that isn't a password devvm
// makes is made again.
func TestDesktopPassword(t *testing.T) {
	d, st := desktopVM(t)
	p1, err := desktopPassword(t.Context(), st)
	if err != nil || !desktop.ValidPassword(p1) {
		t.Fatalf("desktopPassword = %q, %v", p1, err)
	}
	again, err := state.ReadVM(d, st.Name())
	if err != nil {
		t.Fatal(err)
	}
	if p2, _ := desktopPassword(t.Context(), again); p2 != p1 {
		t.Errorf("second desktopPassword = %q, want %q", p2, p1)
	}
	for _, bad := range []string{"short", "has spaces!", "ABCDEFGH"} {
		if err := set(t, again, KeyDesktopPassword, bad); err != nil {
			t.Fatal(err)
		}
		if p, _ := desktopPassword(t.Context(), again); p == bad || !desktop.ValidPassword(p) {
			t.Errorf("with %q kept: desktopPassword = %q", bad, p)
		}
	}
}

// The listener is on 127.0.0.1 alone, on the port kept for the VM while
// it's free, else on a new one, which is kept.
func TestDesktopListener(t *testing.T) {
	d, st := desktopVM(t)
	e := Desktop{&Env{Dirs: d, Proxy: &netpolicy.Proxy{Dirs: d, Listening: ports.Listening}}}
	l, err := e.listener(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || get(st, KeyDesktopPort) != strconv.Itoa(addr.Port) {
		t.Fatalf("listening on %v, kept %q", addr, get(st, KeyDesktopPort))
	}
	// Taken (by the first): another one, kept in its place.
	l2, err := e.listener(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	p2 := l2.Addr().(*net.TCPAddr).Port
	if p2 == addr.Port || get(st, KeyDesktopPort) != strconv.Itoa(p2) {
		t.Errorf("second on %d, kept %q", p2, get(st, KeyDesktopPort))
	}
	_ = l.Close()
	_ = l2.Close()
	// Free again: the kept one.
	l3, err := e.listener(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if p := l3.Addr().(*net.TCPAddr).Port; p != p2 {
		t.Errorf("third on %d, want the kept %d", p, p2)
	}
	_ = l3.Close()
	// The kept one is one only VMs' ssh may have: a new one.
	e.Proxy.SSHList = []int{p2}
	l4, err := e.listener(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l4.Close() }()
	if p := l4.Addr().(*net.TCPAddr).Port; p == p2 || get(st, KeyDesktopPort) != strconv.Itoa(p) {
		t.Errorf("fourth on %d (kept %q), not off the ssh port %d", p, get(st, KeyDesktopPort), p2)
	}
}

// Closing a channel reports an ssh that ended on its own with a failure,
// in the log once and as Close's error each time, and nothing for one
// that ended well or that the dial's context ended (killed).
func TestDesktopChannelClose(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		cancel       bool
		want         string // in Close's error and the log's one line; "": neither
	}{
		{name: "failed", script: "cat >/dev/null; exit 3", want: "exit status 3"},
		{name: "ended well", script: "cat >/dev/null"},
		{name: "context ended", script: "exec sleep 30", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var lines []string
			logf := func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) }
			c, err := startChannel(ctx, exec.CommandContext(ctx, "/bin/sh", "-c", tc.script), logf) //nolint:gosec // the test's own scripts
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancel {
				cancel()
			}
			for range 2 {
				err := c.Close()
				if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
					t.Errorf("Close() = %v, want %q", err, tc.want)
				}
			}
			if tc.want == "" && len(lines) != 0 || tc.want != "" && (len(lines) != 1 || !strings.Contains(lines[0], tc.want)) {
				t.Errorf("logged %q, want one line with %q", lines, tc.want)
			}
		})
	}
}

// The ssh channel: devvm's ssh config, a channel to the desktop's socket,
// no session, agent or forwarding.
func TestDesktopSSHArgs(t *testing.T) {
	args := desktopSSHArgs("/cfg/ssh_config", "devvm-p-00000000000000")
	for _, want := range [][]string{
		{"-F", "/cfg/ssh_config"},
		{"-W", desktop.Socket, "devvm-p-00000000000000"},
		{"-o", "IdentityAgent=none"},
		{"-o", "ForwardAgent=no"},
		{"-o", "ForwardX11=no"},
		{"-o", "ClearAllForwardings=yes"},
		{"-o", "ControlMaster=no"},
		{"-T"},
	} {
		found := false
		for i := range args {
			if slices.Equal(args[i:min(len(args), i+len(want))], want) {
				found = true
			}
		}
		if !found {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if args[len(args)-1] != "devvm-p-00000000000000" {
		t.Errorf("the host isn't last: %q", args)
	}
}
