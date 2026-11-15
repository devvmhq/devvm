package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
)

// --- the SSH agent relay ---
//
// A VM that forwards an SSH agent (Config.Agent) reaches it only through this relay, which listens
// on the VM's own socket (Config.AgentSocket): the VM's backend forwards that socket, never the
// agent's. OpenSSH's ssh-agent answers every request a forwarded connection sends, so a VM that
// reached the agent itself could remove every key (ssh-add -D) or lock it with a password of its own
// (ssh-add -x), for the user and every other VM that uses it, and add keys to it without bound. The
// relay passes on only what using a key needs: listing the keys, signing with one, and binding the
// connection to a host (session-bind@openssh.com, which ssh sends as it forwards the agent and as
// it logs in with it, so a key added with destination constraints keeps them), and answers anything
// else with SSH_AGENT_FAILURE itself, so the agent never sees it.

// The agent protocol's messages the relay reads (draft-ietf-sshm-ssh-agent).
const (
	agentFailure           = 5
	agentRequestIdentities = 11
	agentSignRequest       = 13
	agentExtension         = 27
)

// agentSessionBind is the one extension the relay passes on.
const agentSessionBind = "session-bind@openssh.com"

// agentMsgMax bounds a message either way, as OpenSSH's own AGENT_MAX_LEN does.
const agentMsgMax = 256 << 10

// agentConnsMax bounds the connections the relay carries at once: each holds one to the agent,
// and an agent shared by several VMs, or your own, has a limit on its open files (256 by default
// on macOS) that no VM may take most of.
const agentConnsMax = 16

// agentIdle is how long the relay waits for a connection's next request, its whole message, before
// it closes the connection and its own to the agent: ssh's connections to an agent are short, and a
// request the agent asks you about (ssh-add -c) waits on the agent's side, which has no limit.
const agentIdle = 2 * time.Minute

// agentAllowed reports whether the relay passes the request msg (its type and contents) on.
func agentAllowed(msg []byte) bool {
	if len(msg) == 0 {
		return false
	}
	switch msg[0] {
	case agentRequestIdentities, agentSignRequest:
		return true
	case agentExtension:
		name, ok := agentString(msg[1:])
		return ok && name == agentSessionBind
	}
	return false
}

// agentString reads the string at b's start: its length (uint32), then its bytes.
func agentString(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	n := binary.BigEndian.Uint32(b)
	if int64(n) > int64(len(b))-4 {
		return "", false
	}
	return string(b[4 : 4+n]), true
}

// readAgentMsg reads one message: its length (uint32, at least 1 and at most agentMsgMax), then its
// type and contents, which it returns.
func readAgentMsg(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > agentMsgMax {
		return nil, errors.New("an agent message's length is out of range")
	}
	msg := make([]byte, size)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// writeAgentMsg writes msg, its length first.
func writeAgentMsg(w io.Writer, msg []byte) error {
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(msg)), uint32(len(msg))) //nolint:gosec // G115: at most agentMsgMax
	_, err := w.Write(append(b, msg...))
	return err
}

// agentRelay is the relay: where it listens, the agent it passes requests on to, and its log.
type agentRelay struct {
	ln       net.Listener
	upstream string
	idle     time.Duration // agentIdle; tests' shorter
	log      *log.Logger
}

// listenAgent listens on path, the VM's agent socket, which only this user may connect to, for
// connections to the agent at upstream. A socket left by a proxy before this one is removed first.
func listenAgent(ctx context.Context, path, upstream string, l *log.Logger) (*agentRelay, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &agentRelay{ln: ln, upstream: upstream, idle: agentIdle, log: l}, nil
}

// serve carries connections until the listener is closed or ctx ends, at most agentConnsMax at
// once: one past it is closed at once. Another accept error, such as running out of files (EMFILE)
// in a burst, leaves the listener open, so it waits before the next try, as devvm's other accept
// loops do (daemon.AcceptRetry), so a burst of them can't spin it, logging only the first.
func (a *agentRelay) serve(ctx context.Context) {
	slots := make(chan struct{}, agentConnsMax)
	var retry daemon.AcceptRetry
	for {
		c, err := a.ln.Accept()
		if err != nil {
			if retry.Failed(ctx, err, func(err error) { a.log.Printf("ssh agent: %v", err) }) {
				continue
			}
			return
		}
		if ctx.Err() != nil {
			_ = c.Close()
			return
		}
		retry.Accepted()
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				a.relay(ctx, c)
			}()
		default:
			_ = c.Close()
		}
	}
}

// relay carries one connection's requests, each answered before the next is read, as the agent
// protocol has them: one the relay passes on goes to the agent over a connection of this one's
// own, made at its first, and gets the agent's answer; any other gets SSH_AGENT_FAILURE, logged
// once a connection. It ends when either side does, or sends a message that isn't one, or when the
// client sends no whole request within the idle time (agentIdle).
func (a *agentRelay) relay(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	var up net.Conn
	defer func() {
		if up != nil {
			_ = up.Close()
		}
	}()
	logged := false
	for {
		if c.SetReadDeadline(time.Now().Add(a.idle)) != nil {
			return
		}
		req, err := readAgentMsg(c)
		if err != nil {
			return
		}
		if !agentAllowed(req) {
			if !logged {
				a.log.Printf("ssh agent: refused a request of type %d", req[0])
				logged = true
			}
			if writeAgentMsg(c, []byte{agentFailure}) != nil {
				return
			}
			continue
		}
		if up == nil {
			if up, err = (&net.Dialer{}).DialContext(ctx, "unix", a.upstream); err != nil {
				a.log.Printf("ssh agent: %v", err)
				return
			}
		}
		if writeAgentMsg(up, req) != nil {
			return
		}
		reply, err := readAgentMsg(up)
		if err != nil || writeAgentMsg(c, reply) != nil {
			return
		}
	}
}

// close stops the relay listening, removing its socket.
func (a *agentRelay) close() { _ = a.ln.Close() }
