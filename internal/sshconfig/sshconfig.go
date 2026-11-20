// Package sshconfig is how ssh and editors reach project VMs by name: the
// ssh config devvm writes for its running VMs (devvm-<folder>), each
// checked against the host keys devvm pins for its start
// (vmssh.PinHostKeys), and the line in ~/.ssh/config that includes it.
package sshconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// header starts the ssh config devvm writes.
const header = "# Written by devvm on every launch; edits are overwritten.\n"

// Config is devvm's ssh config. New sets what its VMs' ssh is checked
// against; its caller sets what it prints and asks.
type Config struct {
	Dirs   paths.Dirs
	Review backend.Review // what their ssh is checked against
	// Reporter is where it prints and asks ([Y/n]).
	msg.Reporter
	// AddInclude adds the Include line without asking (--ssh-config).
	AddInclude bool
}

// New returns the ssh config for the VMs of the backends in reg.
func New(d paths.Dirs, reg *backend.Registry) *Config {
	return &Config{Dirs: d, Review: reg.Review}
}

// ChooseAlias is the name ssh and editors will reach a new VM, vm, by
// besides its own: devvm-<folder>, as its name has the folder's name,
// unless another VM's state records that alias; then the VM's own name.
// It's chosen once, as the VM is made (state.VM.Alias), so a VM's alias
// never changes when others come and go, inside the claim of the VM's
// state.VM.Create, so two VMs made at once never choose one alias.
func ChooseAlias(d paths.Dirs, vm vmname.VM) (string, error) {
	short, ok := vm.Short()
	if !ok {
		return vm.String(), nil
	}
	names, err := state.VMNames(d)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if n == vm {
			continue
		}
		st, err := state.ReadVM(d, n)
		if err != nil {
			return "", err
		}
		if st.Alias == short && vmname.IsAlias(st.Alias, n) {
			return vm.String(), nil
		}
	}
	return short, nil
}

// Host is the name ssh and editors reach vm by: the alias its state
// records, else its own name. An alias ChooseAlias never gives vm (a
// hand-edited vm.json's: another folder's, a pattern, more lines for the
// ssh config) is never taken (vmname.IsAlias).
func (c *Config) Host(vm vmname.VM) string {
	if st, err := state.ReadVM(c.Dirs, vm); err == nil && vmname.IsAlias(st.Alias, vm) {
		return st.Alias
	}
	return vm.String()
}

// Running is a running project VM, as its backend's list has it.
type Running struct {
	Backend  backend.Backend
	Instance backend.Instance
}

// Write rewrites the ssh config with the running project VMs up, each by
// its alias (Host) and its full name, neither of which ever changes,
// checked against the host keys devvm pins for its current start
// (vmssh.Checked, vmssh.PinKeys), read from it once. A VM whose host keys
// can't be pinned (vmssh.ErrNoHostKeys) gets no entry: a stopped VM's port
// is free for anything on the host to take, so ssh must check it reaches
// the VM. Nor does one whose backend's ssh isn't usable
// (vmssh.UnusableError): an emulation not yet reviewed, or not a user and
// a key on a host loopback port. An alias an earlier
// entry has (two VMs made at once) is left out: ssh would take the first.
func (c *Config) Write(ctx context.Context, up []Running) error {
	up = slices.Clone(up)
	slices.SortFunc(up, func(a, b Running) int { return strings.Compare(a.Instance.Name.String(), b.Instance.Name.String()) })
	var buf strings.Builder
	buf.WriteString(header)
	taken := map[string]bool{}
	for _, r := range up {
		b, in := r.Backend, r.Instance
		if !in.Name.IsProject() || in.Status != backend.StatusRunning {
			continue
		}
		reach, err := vmssh.Checked(ctx, c.Dirs, c.Review, b, in.Name, vmssh.PinKeys)
		var unusable *vmssh.UnusableError
		if errors.As(err, &unusable) || errors.Is(err, vmssh.ErrNoHostKeys) {
			continue
		} else if err != nil {
			return err
		}
		alias := c.Host(in.Name)
		if taken[alias] {
			alias = in.Name.String()
		}
		taken[alias] = true
		if err := c.entry(&buf, alias, in, reach.SSH, reach.Options, reach.Start); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return state.WriteFile(c.Dirs.SSHConfig(), []byte(buf.String()))
}

// agentSocket is the agent an editor connecting to vm forwards, its
// current start being start (sshagent.ReachOf): for a start devvm relayed,
// its proxy's relay of the agent that start forwarded, as devvm's own
// shell has it, whatever agent the editor has, even while the relay is
// down. A start an older devvm recorded without the relay forwards what
// that start did: devvm's own agent's socket, or "yes", the editor's own
// agent. A start devvm didn't record, such as one outside devvm, gets none,
// as does a choice of none: "".
func (c *Config) agentSocket(vm vmname.VM, st *state.VM, start string) string {
	switch sshagent.ReachOf(st, start) {
	case sshagent.Relay:
		return c.Dirs.AgentRelay(vm)
	case sshagent.Raw:
		if sshagent.Running(st, start) == sshagent.Devvm {
			return c.Dirs.AgentSock()
		}
		return "yes"
	case sshagent.None:
	}
	return ""
}

// entry writes vm's Host block. Its own host keys are checked, and agent
// forwarding is as its current start (start) was made, as devvm's own
// shell has it (agentSocket), an explicit no included, so
// a Host * of the user's doesn't apply: ssh takes an option's first value.
// It logs in as login says, its backend's checked target (vmssh.FromTarget),
// with the target's other login options (opts).
func (c *Config) entry(buf *strings.Builder, alias string, in backend.Instance, login vmssh.SSH, opts []backend.SSHOption, start string) error {
	hosts := alias
	if alias != in.Name.String() {
		hosts += " " + in.Name.String()
	}
	st, err := state.ReadVM(c.Dirs, in.Name)
	if err != nil {
		return err
	}
	forward, identity := "no", ""
	if sock := c.agentSocket(in.Name, st, start); sock == "yes" {
		forward = sock
	} else if sock != "" {
		// OpenSSH forwards a socket only when it sees an agent of its
		// own: the same one, so an editor started without SSH_AUTH_SOCK
		// (or with another) still forwards it.
		forward = `"` + sock + `"`
		identity = "  IdentityAgent " + forward
	}
	lines := []string{
		"", "Host " + hosts,
		"  HostKeyAlias " + in.Name.String(),
		`  UserKnownHostsFile "` + login.KnownHosts + `"`,
		"  StrictHostKeyChecking yes",
		"  NoHostAuthenticationForLocalhost no",
		"  ForwardAgent " + forward,
		// A connection of its own: over a shared one, a session would
		// forward the agent of whichever process opened it.
		"  ControlMaster no",
		"  ControlPath none",
	}
	if identity != "" {
		lines = append(lines, identity)
	}
	lines = append(lines,
		// Nothing else of the host's for the VM, whatever a Host * says:
		// no X11 display, tunnel or remote SOCKS forward, none of its
		// keys added to an agent, no hop through another host or a
		// command, and no host keys the VM offers later (devvm reads them
		// itself). A RemoteForward can't be turned off here without
		// refusing -R on the command line too. PermitRemoteOpen needs
		// OpenSSH 8.7 or newer. No IgnoreUnknown: its first value wins,
		// and would replace a Host * one of the user's.
		"  ForwardX11 no",
		"  ForwardX11Trusted no",
		"  Tunnel no",
		"  PermitRemoteOpen none",
		"  AddKeysToAgent no",
		"  ProxyJump none",
		"  ProxyCommand none",
		"  UpdateHostKeys no",
		"  HashKnownHosts no",
		// The terminal's 24-bit color, which the VM's sshd accepts where
		// its image's config does (base's Ubuntu): ssh sends no
		// COLORTERM unless asked, and a SendEnv adds to a Host *'s
		// rather than replacing it.
		"  SendEnv COLORTERM",
	)
	lines = append(lines,
		"  Hostname "+vmssh.Loopback, "  Port "+strconv.Itoa(login.Port),
		"  User "+login.User, `  IdentityFile "`+login.Key+`"`,
	)
	for _, o := range opts {
		lines = append(lines, "  "+o.Key+" "+o.Value)
	}
	buf.WriteString(strings.Join(lines, "\n") + "\n")
	return nil
}

// IncludeLine is the line ~/.ssh/config needs to read devvm's ssh config.
func (c *Config) IncludeLine() string { return "Include " + c.Dirs.SSHConfig() }

// EnsureInclude makes sure home's ~/.ssh/config includes devvm's ssh
// config, asking first: once said yes to, the line goes at the top, where
// it must be, before any Host block. It reports whether the line is
// there. Where it can't ask, it says to add it.
func (c *Config) EnsureInclude(home string) (bool, error) {
	line := c.IncludeLine()
	sshDir := filepath.Join(home, ".ssh")
	path := filepath.Join(sshDir, "config")
	data, err := os.ReadFile(path) //nolint:gosec // the user's ssh config
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if bytes.Contains(data, []byte(line)) {
		return true, nil
	}
	c.Say(msg.SSHIncludeNeeded(line))
	if !c.AddInclude {
		add, err := c.Confirm(msg.AddItNow().AnsweredBy("--ssh-config"))
		if err != nil {
			// Unasked, it's the user's to add.
			c.Warn(msg.SSHIncludeYourself())
			return false, nil //nolint:nilerr // a refusal falls back on the warning
		}
		if !add {
			return false, nil
		}
	}
	// The user's ~/.ssh, which may be a link (a dotfiles repo's): followed,
	// as state.PrivateDir, for devvm's own folders, wouldn't.
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(sshDir, 0o700); err != nil { //nolint:gosec // a folder: owner-only with search
		return false, err
	}
	// A linked config (a dotfiles repo) stays linked: its file is written.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	if err := state.WriteFile(path, append([]byte(line+"\n"), data...)); err != nil {
		return false, err
	}
	c.Info(msg.SSHIncludeAdded())
	return true, nil
}
