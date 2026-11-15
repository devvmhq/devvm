package vmssh

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"syscall"

	"github.com/devvmhq/devvm/internal/platform"
)

// LoopbackForward forwards ports on this machine's loopback to the same
// ports on a VM's loopback, for as long as it's open: a login's callback,
// which a browser on this machine opens, reaches the agent's own server in
// the VM through it. It runs in devvm's own process, so it ends with it.
// It listens on 127.0.0.1 and ::1, as a browser reaches localhost at
// either, never on a port a program on this machine has (ListenLoopback),
// and each connection is an ssh channel of its own (ssh -W) to the VM's
// 127.0.0.1, which the VM's sshd opens: no master, nothing forwarded the
// other way.
type LoopbackForward struct {
	ls     []net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ErrPortTaken is ForwardLoopback's error when a program on this machine
// has the port it needs on 127.0.0.1.
var ErrPortTaken = errors.New("a program on this machine has the port")

// ForwardLoopback forwards need, and each of also that it can listen on,
// from this machine's loopback to the same port in the VM, until Close or
// ctx's end. Without 127.0.0.1's need it forwards nothing and returns
// ErrPortTaken, or the listener's error.
func (o SSH) ForwardLoopback(ctx context.Context, need int, also ...int) (*LoopbackForward, error) {
	return o.forwardLoopback(ctx, func(ctx context.Context, conn net.Conn, port int) {
		pipeSSH(ctx, conn, o.loopbackArgs(port))
	}, need, also...)
}

// forwardLoopback is ForwardLoopback, each connection carried by carry.
func (o SSH) forwardLoopback(ctx context.Context, carry func(context.Context, net.Conn, int), need int, also ...int) (*LoopbackForward, error) {
	listen := func(host string, port int) (net.Listener, error) {
		return platform.ListenLoopback(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	l, err := listen(Loopback, need)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrPortTaken
		}
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &LoopbackForward{ls: []net.Listener{l}, cancel: cancel}
	ports := []int{need} // each listener's, in f.ls's order
	for _, p := range slices.Concat([]int{need}, also) {
		for _, h := range []string{Loopback, "::1"} {
			if p == need && h == Loopback {
				continue
			}
			if l, err := listen(h, p); err == nil {
				f.ls, ports = append(f.ls, l), append(ports, p)
			}
		}
	}
	for i, l := range f.ls {
		f.wg.Go(func() { f.serve(ctx, l, ports[i], carry) })
	}
	go func() {
		<-ctx.Done()
		f.closeListeners()
	}()
	return f, nil
}

// serve accepts l's connections until it's closed, carrying each to port.
func (f *LoopbackForward) serve(ctx context.Context, l net.Listener, port int, carry func(context.Context, net.Conn, int)) {
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		conns.Go(func() { carry(ctx, c, port) })
	}
}

func (f *LoopbackForward) closeListeners() {
	for _, l := range f.ls {
		_ = l.Close()
	}
}

// Close stops listening, ends the connections it carries and waits for
// them. It can be called more than once.
func (f *LoopbackForward) Close() {
	f.cancel()
	f.closeListeners()
	f.wg.Wait()
}

// loopbackArgs are ssh's arguments for one forwarded connection: a
// connection of its own to the VM, as connArgs has it, and a channel to
// port on the VM's 127.0.0.1.
func (o SSH) loopbackArgs(port int) []string {
	return slices.Concat(
		[]string{"-F", "none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-W", net.JoinHostPort(Loopback, strconv.Itoa(port))},
		o.connArgs(),
	)
}
