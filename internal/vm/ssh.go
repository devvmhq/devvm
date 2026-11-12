package vm

import (
	"context"
	"os/exec"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// ownSSH is how devvm's own ssh reaches a running VM, for links and a
// --clone VM's fetch: its backend's ssh (reviewed), with the
// VM's own key and the host keys devvm pinned for its current start, read
// as they are (vmssh.Checked, vmssh.ReadKeys).
func (e *Env) ownSSH(ctx context.Context, b backend.Backend, vm string) (vmssh.SSH, error) {
	r, err := vmssh.Checked(ctx, e.Dirs, e.Registry.Review, b, vm, vmssh.ReadKeys)
	return r.SSH, err
}

// ShowHost gets the folder's VM ready for ssh and editors (reach), then
// says the name they reach it by and the folder to open (shellDir).
func (e Launch) ShowHost(ctx context.Context) error {
	host, t, err := e.reach(ctx)
	if err != nil {
		return err
	}
	e.Say(msg.SSHHostInfo(t.shellDir(ctx, e.vmLog()), host))
	return nil
}

// Code gets the folder's VM ready for ssh and editors (reach), then opens
// the folder its shell would open in (shellDir) in VS Code over
// Remote-SSH, in devvm's place, with the
// environment devvm started with: the SSH agent devvm's own connections
// use isn't the editor's.
func (e Launch) Code(ctx context.Context) error {
	host, t, err := e.reach(ctx)
	if err != nil {
		return err
	}
	path, err := exec.LookPath("code")
	if err != nil {
		return msg.NoCodeCommand(host)
	}
	return e.Exec(backend.Command{
		Path: path,
		Args: []string{"code", "--remote", "ssh-remote+" + host, t.shellDir(ctx, e.vmLog())},
		Env:  e.Environ,
	})
}

// reach launches the folder's VM as devvm does without its shell
// (launch), sets it up, checks its backend says how ssh reaches it, and
// makes sure ~/.ssh/config includes devvm's ssh config, asking first. It
// returns the name ssh reaches it by.
func (e Launch) reach(ctx context.Context) (string, *target, error) {
	t, err := ready(ctx, e)
	if err != nil {
		return "", nil, err
	}
	if _, err := e.sshConfig().EnsureInclude(e.Dirs.Home); err != nil {
		return "", nil, err
	}
	return t.sshHost(), t, nil
}

// ready launches the folder's VM as devvm does without its shell (launch),
// sets it up, which puts it in devvm's ssh config, and checks its backend
// says how ssh reaches it.
func ready(ctx context.Context, e Launch) (*target, error) {
	t, _, err := readyConf(ctx, e)
	return t, err
}

// readyConf is ready, with the VM's confinement as its launch gave it,
// which setting it up again needs (syncer.sync).
func readyConf(ctx context.Context, e Launch) (*target, backend.Confinement, error) {
	var none backend.Confinement
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return nil, none, err
	}
	if err := e.Registry.Require(t.b, t.f.Name, backend.FeatureSSH); err != nil {
		return nil, none, err
	}
	conf, err := e.launch(ctx, t, upOpts{})
	if err != nil {
		return nil, none, err
	}
	if err := e.setUpVM(ctx, t, conf); err != nil {
		return nil, none, err
	}
	// A VM running since a start without its ssh (an emulation not yet
	// let through then) has none to reach.
	if _, err := t.b.SSH(ctx, t.f.Name); err != nil {
		return nil, none, err
	}
	return t, conf, nil
}
