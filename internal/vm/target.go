package vm

import (
	"context"
	"errors"
	"io"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// newTarget is the VM named f.Name, for folder f, whose state is st, on b,
// not yet read from its backend (refresh): the finder's, and a new VM's.
func (e *Env) newTarget(f project.Folder, st *state.VM, b backend.Backend) *target {
	return &target{e: e, f: f, st: st, b: b}
}

// refresh reads the VM from its backend again: what it is (t.in) and
// whether it has it (t.exists). The finder reads it first; a start and a
// stop of it then set what they made it (started, stop), as its backend
// would answer, without asking again.
func (t *target) refresh(ctx context.Context) error {
	in, err := t.b.Inspect(ctx, t.f.Name)
	switch {
	case err == nil:
		t.in, t.exists = in, true
	case errors.Is(err, backend.ErrNotFound):
		t.in, t.exists = backend.Instance{}, false
	default:
		return err
	}
	return nil
}

// running is the VM's current start, as its backend tells it apart
// (backend.Run).
func (t *target) running(ctx context.Context) (backend.Run, error) {
	return t.b.Running(ctx, t.f.Name)
}

// stop stops the VM: it's stopped from then on (t.in).
func (t *target) stop(ctx context.Context) error {
	if err := t.b.Stop(ctx, t.f.Name); err != nil {
		return err
	}
	t.in.Status = backend.StatusStopped
	return nil
}

// stopToStart stops the VM to start it again (a restart), once what
// refuses any VM's proxy start, known before (netpolicy.Proxy.Refusal),
// doesn't: a refused restart leaves the VM running as it was, and says
// so when it runs (net_proxy_kept), as a refused launch does.
func (t *target) stopToStart(ctx context.Context) error {
	if err := t.e.Proxy.Refusal(ctx); err != nil {
		if t.in.Status == backend.StatusRunning {
			t.e.Warn(msg.NetProxyKept(t.f.Name))
		}
		return err
	}
	return t.stop(ctx)
}

// started sets what a start of the VM that asked for w made it (t.in):
// running, with what w asks where it differs, as a backend's Start
// applies it (backend.Apply).
func (t *target) started(w backend.Want) {
	backend.Apply(&t.in, w)
	t.in.Status, t.exists = backend.StatusRunning, true
}

// ensureRunning makes sure the VM runs, for a command that runs in it
// without a shell (an export, rm's check of a --clone VM, a remake's home
// folder): it goes through what every start does (starter.bringUp), .git
// checked, the network tier's self-check and the refusals included.
func (t *target) ensureRunning(ctx context.Context) error {
	_, err := t.e.starter.bringUp(ctx, t, upOpts{})
	return err
}

// exec runs argv in the running VM, its connection carrying the VM's
// agent.
func (t *target) exec(ctx context.Context, argv []string, o backend.ExecOpts) error {
	o.Agent = t.agent
	return t.b.Exec(ctx, t.f.Name, argv, o)
}

// shell is the command that opens a shell in the running VM, in the
// folder it opens in (shellDir), its connection carrying the VM's agent.
func (t *target) shell(ctx context.Context) (backend.Command, error) {
	return t.b.Shell(ctx, t.f.Name, backend.ShellOpts{Dir: t.shellDir(ctx, t.e.vmLog()), Agent: t.agent})
}

// shellDir is the folder in a running VM that its shell and editors open
// in: its project folder, else, for a parent folder's VM, the folder devvm
// runs in (t.sub), at the same path in the VM. A --clone VM's clone is at
// the parent's path: it's the subfolder when the clone has it, else the
// clone's top. The check's errors go to log.
func (t *target) shellDir(ctx context.Context, log io.Writer) string {
	if t.sub == "" {
		return t.f.Dir
	}
	if isClone(t.st) {
		err := t.exec(ctx, []string{"test", "-d", t.sub}, backend.ExecOpts{Stderr: log})
		if err != nil {
			return t.f.Dir
		}
	}
	return t.sub
}

// ownSSH is how devvm's own ssh reaches the running VM (Env.ownSSH).
func (t *target) ownSSH(ctx context.Context) (vmssh.SSH, error) {
	return t.e.ownSSH(ctx, t.b, t.f.Name)
}

// sshHost is the name ssh reaches the VM by: its entry's in devvm's ssh
// config.
func (t *target) sshHost() string {
	return t.e.sshConfig().Host(t.f.Name)
}

// widen is asked before a command that gives the VM more access (cmd, as
// devvm network open) changes it: true for the folder's own VM. A parent
// folder's VM, found from a subfolder, changes only after asking, [y/N],
// which no flag answers.
func (t *target) widen(cmd string) (bool, error) {
	if t.sub == "" {
		return true, nil
	}
	dir, vm := t.f.Dir, t.f.Name
	return t.e.Ask(msg.WidenParentVM(cmd, dir, vm).RefusedWith(msg.WidenParentVMRefused(cmd, dir, vm)))
}
