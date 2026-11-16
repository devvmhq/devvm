package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/desktop"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// DesktopOptions is how devvm desktop opens the VM's desktop.
type DesktopOptions struct {
	// Clipboard shares the clipboard both ways: this machine's reaches
	// the VM, and the VM's this machine.
	Clipboard bool
	// NoOpen doesn't open Screen Sharing on macOS: the address and
	// password are only shown.
	NoOpen bool
}

// How long a viewer's handshake may take, how long a wrong password holds
// the next check, and how often devvm desktop checks the VM still runs.
const (
	desktopTimeout   = 15 * time.Second
	desktopFailDelay = time.Second
	desktopEvery     = 5 * time.Second
)

// Desktop is devvm desktop: the folder's VM's desktop on this machine.
type Desktop struct{ *Env }

// Open gets the folder's VM ready as devvm host does (ready), starts its
// desktop (the desktop layer's devvm-desktop), and serves it on this
// machine's loopback through devvm's relay (desktop.Relay), each viewer
// checked against the VM's password before anything reaches the VM, until
// ctx ends (Ctrl+C) or the VM stops. On macOS it opens the address in
// Screen Sharing, unless NoOpen.
func (e Desktop) Open(ctx context.Context, o DesktopOptions) error {
	t, err := ready(ctx, e.launcher)
	if err != nil {
		return err
	}
	vm, b, log := t.f.Name, t.b, e.LogWriter()
	logs := func() backend.ExecOpts {
		w := e.vmLog()
		return backend.ExecOpts{Stdout: w, Stderr: w}
	}
	if err := t.exec(ctx, []string{"test", "-x", desktop.Command}, logs()); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return msg.DesktopNoLayer(vm)
	}
	password, err := desktopPassword(ctx, t.st)
	if err != nil {
		return err
	}
	clip := "off"
	if o.Clipboard {
		clip = "on"
	}
	if err := t.exec(ctx, []string{desktop.Command, "start", clip}, logs()); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.DesktopStartFailed(vm))
	}
	l, err := e.listener(ctx, t.st)
	if err != nil {
		return err
	}
	addr := l.Addr().String()
	logf := func(f string, args ...any) { _, _ = fmt.Fprintf(log, "devvm desktop: "+f+"\n", args...) }
	r := &desktop.Relay{
		Password: password, Clipboard: o.Clipboard, Dial: e.dial(vm, logf), Logf: logf,
		Timeout: desktopTimeout, FailDelay: desktopFailDelay,
	}
	e.Say(msg.DesktopReady(addr, password, vm))
	if url := "vnc://" + addr; e.OS == "darwin" && !o.NoOpen {
		if err := e.OpenURL(ctx, url); err != nil {
			e.Warn(msg.DesktopOpenFailed(url))
		}
	}
	return e.serve(ctx, r, l, b, vm)
}

// serve serves the relay on l until ctx ends or the VM stops, which it
// says.
func (e Desktop) serve(ctx context.Context, r *desktop.Relay, l net.Listener, b backend.Backend, vm vmname.VM) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(desktopEvery):
			}
			if in, err := b.Inspect(ctx, vm); ctx.Err() == nil && (err != nil || in.Status != backend.StatusRunning) {
				close(stopped)
				cancel()
				return
			}
		}
	}()
	err := r.Serve(ctx, l)
	select {
	case <-stopped:
		e.Info(msg.DesktopVMStopped(vm))
	default:
	}
	return err
}

// desktopPassword returns the VM's desktop's password (desktop.NewPassword),
// made the first time devvm desktop runs and kept in its state's desktop
// section, never sent to the VM: devvm's relay checks it.
func desktopPassword(ctx context.Context, st *state.VM) (string, error) {
	if p := sectionOf[desktopInVM](st, desktopSection).Made.Password; desktop.ValidPassword(p) {
		return p, nil
	}
	p, err := desktop.NewPassword()
	if err != nil {
		return "", err
	}
	err = update(ctx, st, desktopSection, func(d *desktopInVM) error {
		// One another devvm made meanwhile is kept.
		if desktop.ValidPassword(d.Made.Password) {
			p = d.Made.Password
		}
		d.Made.Password = p
		return nil
	})
	return p, err
}

// listener listens on this machine's loopback (127.0.0.1 alone) for the
// VM's desktop: on the port it had, while no program has it and it isn't
// one only VMs' ssh may have (ports.Allocator's SSHOnly), else on a new
// one the allocator chooses (ports.Allocator), which it keeps, so a
// viewer finds it at the same address each time.
func (e Desktop) listener(ctx context.Context, st *state.VM) (net.Listener, error) {
	listen := func(p int) (net.Listener, error) {
		return platform.ListenLoopback(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	}
	if p := sectionOf[ports.Ports](st, ports.Section).Host.Desktop; backend.ValidPort(p) && !e.Proxy.Ports().SSHOnly(p) {
		if l, err := listen(p); err == nil {
			return l, nil
		}
	}
	for range 10 {
		p, err := e.Proxy.Ports().Choose(ctx, st, ports.Desktop, func(int, ports.Ports) bool { return false })
		if err != nil {
			return nil, err
		}
		if l, err := listen(p); err == nil {
			return l, nil
		}
	}
	return nil, errors.New("no free port on 127.0.0.1")
}

// dial connects to the VM's desktop socket over the VM's ssh, as
// devvm's ssh config has it (its host key pinned), for as long as ctx
// lasts: a channel to the socket (ssh -W), with no session, agent or
// forwarding. An ssh that fails goes in the log through logf.
func (e Desktop) dial(vm vmname.VM, logf func(string, ...any)) func(ctx context.Context) (io.ReadWriteCloser, error) {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		cmd := exec.CommandContext(ctx, "ssh", desktopSSHArgs(e.Dirs.SSHConfig(), vm)...) //nolint:gosec // devvm's own arguments
		cmd.Stderr = e.LogWriter()
		c, err := startChannel(ctx, cmd, logf)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// startChannel starts cmd, made with ctx, as a channel: its input and
// output are the channel's, and a failure goes in the log through logf.
func startChannel(ctx context.Context, cmd *exec.Cmd, logf func(string, ...any)) (*sshChannel, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &sshChannel{cmd: cmd, in: in, out: out, logf: logf, cancelled: ctx.Done()}, nil
}

// desktopSSHArgs are ssh's arguments for a channel to the VM's desktop
// socket: devvm's ssh config's entry for the VM, with nothing of the
// user's agent, no session and nothing forwarded either way.
func desktopSSHArgs(config string, vm vmname.VM) []string {
	return []string{
		"-F", config, "-T",
		"-o", "BatchMode=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "IdentityAgent=none", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR",
		"-W", desktop.Socket, vm.String(),
	}
}

// sshChannel is an ssh -W process as a connection.
type sshChannel struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
	// logf gets the line for an ssh that failed, the relay's Logf.
	logf func(format string, args ...any)
	// cancelled is closed once the dial's context ends, which kills ssh.
	cancelled <-chan struct{}
	once      sync.Once
	err       error // how ssh failed, once Close has ended it
}

func (c *sshChannel) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *sshChannel) Write(p []byte) (int, error) { return c.in.Write(p) }

// channelClose is how long a channel's ssh gets to end on its own once its
// input is closed (ssh sends the end of the channel, and the VM's sshd
// closes the socket) before it's killed.
const channelClose = 2 * time.Second

// Close ends the channel and its ssh, once: its input closed first, so
// the VM's sshd ends its side, then killed if it hasn't ended. An ssh
// that ended on its own with a failure, such as 255 for a connection the
// VM dropped, goes in the log (logf) and is Close's error, each time it's
// called. One Close killed isn't, as its end is the kill, and nor is any
// that ends once the dial's context has ended, which kills ssh as devvm
// desktop ends, so a failure of its own just before then goes unlogged.
func (c *sshChannel) Close() error {
	c.once.Do(func() {
		_ = c.in.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		killed := false
		select {
		case c.err = <-done:
		case <-time.After(channelClose):
			_ = c.cmd.Process.Kill() //nolint:forbidigo // its own child, not yet waited for, so its pid is still its own
			killed = true
			c.err = <-done
		}
		select {
		case <-c.cancelled:
			killed = true // by the context, as devvm desktop ends
		default:
		}
		// A kill's error ("signal: killed") says nothing of the channel.
		if killed {
			c.err = nil
		}
		if c.err != nil {
			c.logf("the VM's ssh channel ended: %v", c.err)
		}
	})
	return c.err
}
