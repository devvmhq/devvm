package vm

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/sshagent"
)

// underStatus records each call to f made with no status line of fake's
// showing, but those the backend makes without one on purpose, each with
// why: what takes the terminal, and what takes no time.
func underStatus(f *backendtest.Fake, fake *msg.Fake) *[]string {
	offStatus := map[string]string{
		"Shell":   "the VM's shell takes the terminal",
		"Running": "it reads the files of the VM's process, which takes no time (Lima's hostagent's pid file)",
	}
	var off []string
	f.OnCall = func(method string) {
		if _, on := fake.Showing(); !on && offStatus[method] == "" {
			off = append(off, method)
		}
	}
	return &off
}

// .git protection's waits for devvm's git lock, held as by another devvm,
// show git_lock_waiting, and a Ctrl-C ends the wait with nothing run.
func TestGitLockWaits(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	if err := os.MkdirAll(d.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := platform.LockFile(t.Context(), d.GitLock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // a wait that shows nothing fails, never hangs
	defer cancel()
	var showed msg.Recorder
	f := &msg.Fake{OnStatus: func(m msg.M) { showed.Add(m); cancel() }}
	ran := false
	err = gitSerial(d, f)(ctx, func() error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || ran || !slices.Equal(showed.Names(), []string{"git_lock_waiting"}) {
		t.Errorf("held: %v, ran %v, showed %q", err, ran, showed.Names())
	}
}

// ssh-add, on the user's agent or devvm's own, never runs under a status
// line, as it may ask for a key's passphrase on the terminal: in a
// stopped VM's launch and a running VM's, with either agent.
func TestSSHAddOffStatusLine(t *testing.T) {
	for _, mode := range []string{sshagent.On, sshagent.Devvm} {
		t.Run(mode, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			ran := 0
			under := func(what string) {
				ran++
				if m, on := fake(e).Showing(); on {
					t.Errorf("%s under %s", what, m.Names())
				}
			}
			sshAdd, agentRun := e.SSHAdd, e.Agent.Run
			e.SSHAdd = func(ctx context.Context, args ...string) int {
				under("ssh-add " + strings.Join(args, " "))
				return sshAdd(ctx, args...)
			}
			e.Agent.Run = func(ctx context.Context, terminal bool, argv ...string) (string, int) {
				under(strings.Join(argv, " "))
				return agentRun(ctx, terminal, argv...)
			}
			st := existingVM(t, e, Options{Network: "open", SSHAgent: true})
			if err := set(t, st, keyAgent, mode); err != nil {
				t.Fatal(err)
			}
			if err := f.Stop(t.Context(), st.Name()); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"stopped", "running"} {
				if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
					t.Fatalf("a %s VM's launch: %v", state, msg.NamesOf(err))
				}
			}
			if ran == 0 {
				t.Error("no ssh-add ran")
			}
		})
	}
}

// The commands other than a launch run the backend's every call under a
// status line too, on a running VM and on a stopped one: each shows,
// lists, changes or stops the folder's VM, and devvm rm deletes it last.
func TestCommandStatusLines(t *testing.T) {
	for _, running := range []bool{true, false} {
		e, f, _ := upEnv(t, allNative())
		f.ExecFunc = noDirect
		st := existingVM(t, e, Options{Network: netpolicy.TierAllowlist})
		if !running {
			if err := f.Stop(t.Context(), st.Name()); err != nil {
				t.Fatal(err)
			}
		}
		off := underStatus(f, fake(e))
		for _, c := range []struct {
			name string
			run  func(context.Context) error
		}{
			{"ls", func(ctx context.Context) error { _, _, err := ListAll(ctx, e); return err }},
			{"ports", func(ctx context.Context) error { _, err := (Settings{e}).Ports(ctx); return err }},
			{"size", func(ctx context.Context) error { _, err := (Settings{e}).ShowSize(ctx, false); return err }},
			{"network", func(ctx context.Context) error { _, err := (Net{e}).Network(ctx); return err }},
			{"tools", func(ctx context.Context) error { _, err := (Tools{e}).List(ctx); return err }},
			{"git-protect", func(ctx context.Context) error { _, err := (Settings{e}).GitProtect(ctx); return err }},
			{"ports 3000", func(ctx context.Context) error { return (Settings{e}).SetPorts(ctx, PortsChange{List: "3000"}) }},
			{"ports add", func(ctx context.Context) error {
				return (Settings{e}).SetPorts(ctx, PortsChange{Op: "add", List: "4000"})
			}},
			{"size --cpus 2", func(ctx context.Context) error { return (Settings{e}).SetSize(ctx, Size{CPUs: "2"}, false) }},
			{"network open", func(ctx context.Context) error {
				return (Net{e}).SetTier(ctx, netpolicy.TierOpen, "devvm network open")
			}},
			{"network allowlist", func(ctx context.Context) error {
				return (Net{e}).SetTier(ctx, netpolicy.TierAllowlist, "devvm network allowlist")
			}},
			{"network transparent off", func(ctx context.Context) error {
				return (Settings{e}).SetTransparent(ctx, "off", "devvm network transparent off")
			}},
			{"network transparent on", func(ctx context.Context) error {
				return (Settings{e}).SetTransparent(ctx, "on", "devvm network transparent on")
			}},
			{"network learn", func(ctx context.Context) error { return (Net{e}).Learn(ctx, "devvm network learn") }},
			{"network allow", func(ctx context.Context) error {
				return (Net{e}).PolicyChange(ctx, []string{"example.com"}, RuleChange{})
			}},
			{"git-protect off", func(ctx context.Context) error { return (Settings{e}).SetGitProtect(ctx, gitprotect.Off) }},
			{"git-protect on", func(ctx context.Context) error { return (Settings{e}).SetGitProtect(ctx, gitprotect.On) }},
			{"ssh-agent off", func(ctx context.Context) error { return (Settings{e}).SetSSHAgent(ctx, sshagent.Off) }},
			{"history export", func(ctx context.Context) error { return (Lifecycle{e}).Export(ctx, "") }},
			{"stop", func(ctx context.Context) error { return (Lifecycle{e}).Stop(ctx) }},
			{"rm", func(ctx context.Context) error { return (Lifecycle{e}).Rm(ctx, RmOptions{Force: true}) }},
		} {
			*off = nil
			calls := len(f.Calls())
			if err := c.run(t.Context()); err != nil {
				t.Fatalf("running %v, %s: %v", running, c.name, msg.NamesOf(err))
			}
			if len(f.Calls()) == calls { // the control: the check sees the backend's calls
				t.Errorf("running %v, %s: no backend call", running, c.name)
			}
			if *off != nil {
				t.Errorf("running %v, %s: %q without a status line", running, c.name, *off)
			}
		}
		if m, on := fake(e).Showing(); on {
			t.Errorf("running %v: still showing %s", running, m.Names())
		}
	}
}

// A launch runs each slow step under a status line, the backend's every
// call included: a new VM's, a stopped VM's and a running VM's, on a
// confined tier and on the open one, so the user sees what devvm waits
// for. Only the shell, which takes the terminal, runs without one.
func TestLaunchStatusLines(t *testing.T) {
	for _, network := range []netpolicy.Tier{netpolicy.TierAllowlist, netpolicy.TierOpen} {
		t.Run(string(network), func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			f.ExecFunc = noDirect
			off := underStatus(f, fake(e))
			st := existingVM(t, e, Options{Network: network})
			if *off != nil {
				t.Errorf("a new VM's launch: %q without a status line", *off)
			}
			*off = nil
			if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			if *off != nil {
				t.Errorf("a running VM's launch: %q without a status line", *off)
			}
			if err := f.Stop(t.Context(), st.Name()); err != nil {
				t.Fatal(err)
			}
			*off = nil
			if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			if *off != nil {
				t.Errorf("a stopped VM's launch: %q without a status line", *off)
			}
			if m, on := fake(e).Showing(); on {
				t.Errorf("still showing %s", m.Names())
			}
		})
	}
}
