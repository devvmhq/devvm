package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
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
