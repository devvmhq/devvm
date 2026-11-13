package vm

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/jsonc"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
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

// CursorCommand is the Cursor editor's command, which devvm cursor runs
// and is named after: the editor's, not its CLI agent's.
const CursorCommand = "cursor"

// Code opens the folder's VM in VS Code, with its code command (openRemote).
func (e Launch) Code(ctx context.Context) error {
	return e.openRemote(ctx, "code", msg.NoCodeCommand, nil)
}

// Cursor opens the folder's VM in the Cursor editor, with its cursor
// command, which takes VS Code's (openRemote), once it finds the command
// first warning when Cursor's Agent will run in the VM
// (cursorAgentWarning).
func (e Launch) Cursor(ctx context.Context) error {
	return e.openRemote(ctx, CursorCommand, msg.NoCursorCommand, func() {
		if m, warn := cursorAgentWarning(e.Dirs.Home, filepath.Dir(e.Dirs.Config)); warn {
			e.Warn(m)
		}
	})
}

// cursorRoute is the setting, in Cursor's user settings, that sends its
// Agent's requests in a Remote-SSH window through the Cursor app on this
// machine when it's cursorRouteAlways; recent Cursor runs Agent in the
// VM's cursor-server otherwise. Cursor reads it only from the user
// settings (it's application-scoped), never from the VM's.
const cursorRoute, cursorRouteAlways = "cursorAgentHost.remoteInferenceRoute", "always"

// cursorUserSettings is Cursor's user settings file, in home or, on
// Linux, the XDG config folder configHome (platform.AppSettingsDir).
func cursorUserSettings(home, configHome string) string {
	return filepath.Join(platform.AppSettingsDir(home, configHome), "Cursor", "User", "settings.json")
}

// cursorSettingsMax is the most of Cursor's user settings devvm cursor
// reads.
const cursorSettingsMax = 1 << 20

// cursorAgentWarning reads Cursor's user settings on this machine, which
// it never writes, and returns the warning devvm cursor gives when they
// don't set cursorRoute to cursorRouteAlways, can't be read, aren't a
// regular file of at most cursorSettingsMax (safefile.Read: never a FIFO
// or a device) or aren't JSON with comments, naming the file.
func cursorAgentWarning(home, configHome string) (msg.M, bool) {
	file := cursorUserSettings(home, configHome)
	warn := msg.CursorAgentInVM(file)
	data, err := safefile.Read(file, cursorSettingsMax, safefile.Opts{Follow: true})
	if err != nil {
		return warn, true
	}
	var settings map[string]any
	if json.Unmarshal(jsonc.Strip(data), &settings) != nil || settings[cursorRoute] != cursorRouteAlways {
		return warn, true
	}
	return msg.M{}, false
}

// openRemote gets the folder's VM ready for ssh and editors (reach), then
// opens the folder its shell would open in (shellDir) over Remote-SSH with
// an editor's command, in devvm's place, with the environment devvm
// started with: the SSH agent devvm's own connections use isn't the
// editor's. Without the command on PATH, it fails with missing, naming the
// host to connect to; with it, found runs first, unless it's nil.
func (e Launch) openRemote(ctx context.Context, command string, missing func(host string) msg.M, found func()) error {
	host, t, err := e.reach(ctx)
	if err != nil {
		return err
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return missing(host)
	}
	if found != nil {
		found()
	}
	return e.Exec(backend.Command{
		Path: path,
		Args: []string{command, "--remote", "ssh-remote+" + host, t.shellDir(ctx, e.vmLog())},
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
