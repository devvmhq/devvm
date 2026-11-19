// Package sshagent is SSH agent forwarding into project VMs: each VM's
// choice (off; on, the user's own agent; devvm, devvm's own), and devvm's
// own agent, which holds only the keys added to it, so a VM that forwards
// it can never use the user's others.
package sshagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Section is the name of the SSH agent section in a VM's state.
const Section = "agent"

// InVM is a VM's SSH agent section: its choice, as asked, and what devvm
// made of it.
type InVM struct {
	Mode string `json:"mode,omitempty"` // On, Devvm or Off
	Made Made   `json:"made,omitzero"`
}

// Made is what devvm recorded of a VM's SSH agent forwarding.
type Made struct {
	// Started is the choice devvm last started it with, and that start:
	// "<mode>_<start>", the start a hash of what its backend identifies
	// it by (backend.Run's ID).
	Started string `json:"started,omitempty"`
	// Toggle is set when its backend emulates turning its forwarding on
	// or off, and it was made ready for that, which its starts rely on.
	Toggle bool `json:"toggle,omitempty"`
	// Relayed is set on every start of this devvm's that forwards an
	// agent, through its proxy's relay (paths.Dirs.AgentRelay), one with
	// no agent to relay included, so nothing gives that VM the agent's own
	// socket until its next start. Only an older devvm's start, which
	// forwarded the agent itself, lacks it.
	Relayed bool `json:"relayed,omitempty"`
}

// Of returns a VM's SSH agent section.
func Of(st *state.VM) (InVM, error) { return state.Section[InVM](st, Section) }

// Set sets a VM's SSH agent section, in st alone: inside st.Update.
func Set(st *state.VM, a InVM) error { return state.SetSection(st, Section, a) }

// SetMode sets a VM's choice, in st alone: inside st.Update.
func SetMode(st *state.VM, mode string) error {
	a, err := Of(st)
	if err != nil {
		return err
	}
	a.Mode = mode
	return Set(st, a)
}

// A VM's choices.
const (
	Off   = "off"   // no agent
	On    = "on"    // the user's own (SSH_AUTH_SOCK)
	Devvm = "devvm" // devvm's own
)

// Modes are the choices, as devvm ssh-agent takes them.
var Modes = []string{On, Devvm, Off}

// Mode is a VM's choice: off unless its state says on or devvm.
func Mode(st *state.VM) string {
	a, _ := Of(st)
	switch a.Mode {
	case On, Devvm:
		return a.Mode
	}
	return Off
}

// Forwards reports whether a VM with mode forwards an agent.
func Forwards(mode string) bool { return mode == On || mode == Devvm }

// RecordStart records that devvm started the VM with mode, start
// identifying that start ("" records none), relayed when mode forwards an
// agent (Made.Relayed): this devvm forwards one only through its proxy's
// relay.
func RecordStart(ctx context.Context, st *state.VM, mode, start string) error {
	relayed := Forwards(mode)
	started := ""
	if start != "" {
		started = mode + "_" + startHash(start)
	}
	return st.Update(ctx, func(st *state.VM) error {
		a, err := Of(st)
		if err != nil {
			return err
		}
		a.Made.Started, a.Made.Relayed = started, relayed && started != ""
		return Set(st, a)
	})
}

// Relayed reports whether the VM's current start, start, is one devvm
// recorded forwarding its agent through its proxy's relay: then the VM
// gets the relay or no agent, never the agent's own socket.
func Relayed(st *state.VM, start string) bool {
	a, _ := Of(st)
	return a.Made.Relayed && Recorded(st, start)
}

// Recorded reports whether devvm recorded the VM's current start, start:
// one it didn't, such as one started outside devvm, gets no agent but its
// proxy's relay, never the agent's own socket, as nothing says which
// agent it forwards.
func Recorded(st *state.VM, start string) bool {
	a, _ := Of(st)
	_, h, ok := strings.Cut(a.Made.Started, "_")
	return start != "" && ok && h == startHash(start)
}

// ReachOf is how a running VM whose current start is start gets its
// agent: Relay, its proxy's relay; Raw, the agent's own socket, only for a
// start an older devvm recorded without the relay; or None, for a start
// devvm didn't record, or whose choice forwards none.
func ReachOf(st *state.VM, start string) Reach {
	switch {
	case !Recorded(st, start) || !Forwards(Running(st, start)):
		return None
	case Relayed(st, start):
		return Relay
	}
	return Raw
}

// Reach is how a running VM gets its agent (ReachOf).
type Reach int

// How a running VM gets its agent.
const (
	None  Reach = iota // no agent
	Relay              // its proxy's relay
	Raw                // the agent's own socket: an older devvm's start
)

// Running is the choice a VM runs with, start identifying its current
// start ("" for none): the one devvm recorded for that start, so a change
// waits for the next; else, for a stopped VM or one something else
// started, its choice.
func Running(st *state.VM, start string) string {
	a, _ := Of(st)
	mode, h, ok := strings.Cut(a.Made.Started, "_")
	if start != "" && ok && h == startHash(start) && slices.Contains(Modes, mode) {
		return mode
	}
	return Mode(st)
}

// startHash is a start's identity as a state value holds it.
func startHash(start string) string {
	sum := sha256.Sum256([]byte(start))
	return hex.EncodeToString(sum[:16])
}

// DefaultLifetime is how long devvm's agent keeps a key added without a
// lifetime: the next launch that needs it loads it again.
const DefaultLifetime = "12h"

// Forever is the lifetime of a key the agent keeps until it ends.
const Forever = "forever"

// CheckLifetime refuses a lifetime --lifetime doesn't take: forever, or
// a duration as state.ParseDuration reads one (30m, 8h, 1d, 2w), which
// ssh-add -t takes as it is.
func CheckLifetime(s string) error {
	if s == Forever {
		return nil
	}
	_, err := state.ParseDuration(s)
	return err
}

// Mount is a host folder a project VM mounts.
type Mount struct {
	VM  vmname.VM
	Dir string
}

// Agent is devvm's own SSH agent. New sets every field; tests change them.
type Agent struct {
	Dirs paths.Dirs
	// Run runs an OpenSSH tool (ssh-add, ssh-agent, ssh-keygen) with
	// devvm's agent as its agent, and returns its output and exit code
	// (127 when it can't run). terminal connects ssh-add's input to
	// devvm's, to ask for a key's passphrase; its output isn't kept.
	Run func(ctx context.Context, terminal bool, argv ...string) (out string, code int)
	// Mounts returns the host folders project VMs mount.
	Mounts func(ctx context.Context) ([]Mount, error)
	// Getenv and LookPath find an ssh-askpass, for --confirm.
	Getenv   func(string) string
	LookPath func(string) (string, error)
	// Reporter is where the agent prints, and the run log ssh-add's
	// output goes to when it asks for a passphrase.
	msg.Reporter
}

// New returns devvm's agent, whose socket is in d's data folder; mounts
// lists what project VMs mount. It prints through r.
func New(d paths.Dirs, mounts func(ctx context.Context) ([]Mount, error), r msg.Reporter) *Agent {
	sock := d.AgentSock()
	return &Agent{
		Dirs: d, Mounts: mounts, Getenv: os.Getenv, LookPath: exec.LookPath,
		Run: func(ctx context.Context, terminal bool, argv ...string) (string, int) {
			return run(ctx, sock, r.LogWriter, terminal, argv...)
		},
		Reporter: r,
	}
}

// Socket is the agent's socket, which VMs that forward it get.
func (a *Agent) Socket() string { return a.Dirs.AgentSock() }

// run runs argv with SSH_AUTH_SOCK set to sock; with terminal, its output
// goes to log's writer.
func run(ctx context.Context, sock string, log func() io.Writer, terminal bool, argv ...string) (string, int) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // OpenSSH's tools, with devvm's arguments
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "SSH_AUTH_SOCK=") })
	cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+sock)
	// ssh-agent leaves its agent running: it gets no pipe to hold open.
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	if terminal {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, log(), log()
	} else {
		cmd.Stdout = &out
	}
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), ee.ExitCode()
	}
	if err != nil {
		return out.String(), 127
	}
	return out.String(), 0
}

// list returns ssh-add -l's output and code: 0 with keys, 1 with none, 2
// with no agent.
func (a *Agent) list(ctx context.Context) (string, int) {
	return a.Run(ctx, false, "ssh-add", "-l")
}

// held returns the fingerprints and comments of the keys the agent holds.
func held(out string) (fps, comments []string) {
	for l := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !strings.HasPrefix(f[1], "SHA256:") {
			continue
		}
		fps = append(fps, f[1])
		comments = append(comments, strings.Join(f[2:len(f)-1], " "))
	}
	return fps, comments
}

// start starts the agent unless it runs (a reboot ends it), and reports
// whether it holds keys.
func (a *Agent) start(ctx context.Context) (bool, error) {
	switch _, code := a.list(ctx); code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	sock := a.Socket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return false, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, code := a.Run(ctx, false, "ssh-agent", "-a", sock); code != 0 {
		return false, msg.AgentStartFailed(sock)
	}
	return false, nil
}

// Ensure starts the agent unless it runs, and loads the keys added to it
// that it doesn't hold: after a reboot, and after a key's lifetime ran
// out. A key is loaded only while its file holds the key that was added
// (its fingerprint, pinned when it was added), and isn't in a folder a VM
// mounts; it warns about each one it doesn't load.
func (a *Agent) Ensure(ctx context.Context) error {
	hasKeys, err := a.start(ctx)
	if err != nil {
		return err
	}
	keys, err := a.added()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		if !hasKeys {
			a.Warn(msg.AgentNoKeys())
		}
		return nil
	}
	out, _ := a.list(ctx)
	loaded, _ := held(out)
	for _, k := range keys {
		if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
			a.Warn(msg.AgentKeyUnpinned(k.File))
			continue
		}
		if slices.Contains(loaded, k.Fingerprint) {
			continue
		}
		vm, err := a.mountedBy(ctx, k.File)
		if err != nil {
			return err
		}
		if vm != "" {
			a.Warn(msg.AgentKeyMounted(k.File, vm))
			continue
		}
		if _, err := a.load(ctx, k); err != nil {
			if m, ok := errors.AsType[msg.M](err); ok {
				a.Warn(m)
				continue
			}
			return err
		}
	}
	return nil
}

// load loads k's file into the agent with its options, if it's still the
// key with k's fingerprint (any key, with none), and returns its
// fingerprint. The file is read once, into a private copy, and the copy is
// what's checked and loaded: the path may be a name a VM can change (a
// link to the user's main key). The copy has nothing beside it, so its
// public key comes from the private key itself.
func (a *Agent) load(ctx context.Context, k AddedKey) (string, error) {
	key, pinned := k.File, k.Fingerprint
	data, err := os.ReadFile(key) //nolint:gosec // a key the user added
	if err != nil {
		return "", msg.AgentLoadFailed(key)
	}
	tmp, err := os.MkdirTemp("", "devvm-key.")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	copied := filepath.Join(tmp, "key")
	if err := state.WriteFile(copied, data); err != nil {
		return "", err
	}
	out, code := a.Run(ctx, false, "ssh-keygen", "-lf", copied)
	f := strings.Fields(out)
	if code != 0 || len(f) < 2 || !strings.HasPrefix(f[1], "SHA256:") {
		return "", msg.AgentKeyNoFingerprint(key)
	}
	fp := f[1]
	if pinned != "" && fp != pinned {
		return "", msg.AgentKeyChanged(key)
	}
	argv := []string{"ssh-add"}
	if k.Confirm {
		argv = append(argv, "-c")
	}
	if life := k.lifetime(); life != Forever {
		argv = append(argv, "-t", life)
	}
	if _, code := a.Run(ctx, true, append(argv, copied)...); code != 0 {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", msg.AgentLoadFailed(key)
	}
	return fp, nil
}

// mountedBy returns a project VM that mounts a folder holding the file key
// names in the end (a link to a key in a project folder is as readable
// there), or "": both by their physical paths (paths.Physical), and by
// identity (paths.In), so a folder named in another case on a volume that
// ignores it still holds the key.
func (a *Agent) mountedBy(ctx context.Context, key string) (vmname.VM, error) {
	file := paths.Physical(key)
	ms, err := a.Mounts(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if paths.In(file, paths.Physical(m.Dir)) {
			return m.VM, nil
		}
	}
	return "", nil
}

// AddOptions are how devvm ssh-agent add loads a key.
type AddOptions struct {
	Confirm  bool   `json:"confirm,omitempty"`  // the agent asks before each use, through ssh-askpass
	Lifetime string `json:"lifetime,omitempty"` // how long it keeps the key: DefaultLifetime when "", or Forever
}

// Add loads a key into the agent, started first unless it runs, and
// records it with its fingerprint and options, replacing what was recorded
// for it, so it's loaded again when it's needed. A key in a folder a VM
// mounts is refused: that VM could read it.
func (a *Agent) Add(ctx context.Context, file string, o AddOptions) error {
	if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() {
		return msg.AgentKeyNotFound(file)
	}
	key, err := recordedPath(file)
	if err != nil {
		return err
	}
	if o.Confirm && a.Getenv("SSH_ASKPASS") == "" {
		if _, err := a.LookPath("ssh-askpass"); err != nil {
			return msg.AgentConfirmNeedsAskpass()
		}
	}
	if o.Lifetime != "" {
		if err := CheckLifetime(o.Lifetime); err != nil {
			return err
		}
	}
	vm, err := a.mountedBy(ctx, key)
	if err != nil {
		return err
	}
	if vm != "" {
		return msg.AgentKeyMounted(key, vm)
	}
	if _, err := a.start(ctx); err != nil {
		return err
	}
	added := AddedKey{File: key, AddOptions: o}
	if added.Fingerprint, err = a.load(ctx, added); err != nil {
		return err
	}
	keys, err := a.added()
	if err != nil {
		return err
	}
	keys = slices.DeleteFunc(keys, func(k AddedKey) bool { return k.File == key })
	if err := a.saveAdded(append(keys, added)); err != nil {
		return err
	}
	out, _ := a.list(ctx)
	_, comments := held(out)
	a.Info(msg.AgentHolds(strings.Join(comments, " ")))
	a.Note(msg.AgentHoldsNote())
	return nil
}

// recordedPath is the path the agent's keys file records for file: made
// absolute, its folder's links resolved (paths.PhysicalParent).
func recordedPath(file string) (string, error) {
	key, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	return paths.PhysicalParent(key), nil
}

// Remove takes a key added to the agent out of it, when the agent runs
// and holds it, then out of the keys file, so it isn't loaded again. The
// key file itself, wherever it is, isn't touched, and needn't still exist.
// A key that isn't recorded is refused.
func (a *Agent) Remove(ctx context.Context, file string) error {
	key, err := recordedPath(file)
	if err != nil {
		return err
	}
	keys, err := a.added()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(keys, func(k AddedKey) bool { return k.File == key })
	if i < 0 {
		return msg.AgentKeyNotRecorded(file)
	}
	if err := a.unload(ctx, keys[i]); err != nil {
		return err
	}
	if err := a.saveAdded(slices.Delete(keys, i, i+1)); err != nil {
		return err
	}
	a.Info(msg.AgentKeyRemoved(key))
	return nil
}

// unload deletes k from the agent if the agent holds it, found by its
// fingerprint: each public key the agent lists is written to a private
// file, and the one whose fingerprint is k's is the one ssh-add -d gets.
// The key's own file isn't read, as it may have changed or be gone.
func (a *Agent) unload(ctx context.Context, k AddedKey) error {
	out, code := a.list(ctx)
	if fps, _ := held(out); code != 0 || !slices.Contains(fps, k.Fingerprint) {
		return nil
	}
	pubs, code := a.Run(ctx, false, "ssh-add", "-L")
	if code != 0 {
		return msg.AgentUnloadFailed(k.File)
	}
	tmp, err := os.MkdirTemp("", "devvm-key.")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	pub := filepath.Join(tmp, "key.pub")
	for line := range strings.SplitSeq(pubs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := state.WriteFile(pub, []byte(line+"\n")); err != nil {
			return err
		}
		fp, code := a.Run(ctx, false, "ssh-keygen", "-lf", pub)
		if f := strings.Fields(fp); code != 0 || len(f) < 2 || f[1] != k.Fingerprint {
			continue
		}
		if _, code := a.Run(ctx, false, "ssh-add", "-d", pub); code != 0 {
			return msg.AgentUnloadFailed(k.File)
		}
		return nil
	}
	return msg.AgentUnloadFailed(k.File)
}

// Keys writes to w the keys the agent holds, as ssh-add -l lists them,
// loading those it lacks first, then each key added to it: its file, its
// fingerprint and how it's loaded. With none added, it writes nothing and
// doesn't start the agent.
func (a *Agent) Keys(ctx context.Context, w io.Writer) error {
	keys, err := a.KeyList(ctx)
	if err != nil || len(keys) == 0 {
		return err
	}
	out, _ := a.list(ctx)
	if _, err := io.WriteString(w, out); err != nil {
		return err
	}
	for _, k := range keys {
		line := k.File + ": " + k.Fingerprint
		if k.Confirm {
			line += " confirm"
		}
		if _, err := io.WriteString(w, line+" lifetime="+k.Lifetime+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// ListedKey is a key added to devvm's own agent, as devvm ssh-agent
// --json lists it.
type ListedKey struct {
	File        string // the key file
	Fingerprint string // its fingerprint when added, "" if unknown
	Confirm     bool   // the agent asks before each use
	Lifetime    string // how long the agent keeps it once loaded: 12h, forever
	Loaded      bool   // the agent holds it now
}

// KeyList returns the keys added to the agent, loading those it lacks
// first, each with whether the agent holds it. With none added, it
// returns none and doesn't start the agent.
func (a *Agent) KeyList(ctx context.Context) ([]ListedKey, error) {
	keys, err := a.added()
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	if err := a.Ensure(ctx); err != nil {
		return nil, err
	}
	out, _ := a.list(ctx)
	fps, _ := held(out)
	listed := make([]ListedKey, 0, len(keys))
	for _, k := range keys {
		listed = append(listed, ListedKey{
			File: k.File, Fingerprint: k.Fingerprint, Confirm: k.Confirm, Lifetime: k.lifetime(),
			Loaded: k.Fingerprint != "" && slices.Contains(fps, k.Fingerprint),
		})
	}
	return listed, nil
}
