package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

// agentExt is an extension request named name.
func agentExt(name string) []byte {
	b := binary.BigEndian.AppendUint32([]byte{agentExtension}, uint32(len(name))) //nolint:gosec // G115: a test's short name
	return append(b, name...)
}

// Only listing keys, signing and binding a session pass; removing, locking, adding and every other
// request don't, an extension cut short included.
func TestAgentAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  []byte
		want bool
	}{
		{"list", []byte{11}, true},
		{"sign", []byte{13, 0, 0, 0, 0}, true},
		{"session-bind", agentExt(agentSessionBind), true},
		{"another extension", agentExt("query"), false},
		{"an extension cut short", agentExt(agentSessionBind)[:10], false},
		{"an extension's name that runs past it", []byte{27, 0, 0, 1, 0, 's'}, false},
		{"add", []byte{17}, false},
		{"add constrained", []byte{25}, false},
		{"remove", []byte{18}, false},
		{"remove all", []byte{19}, false},
		{"add a smartcard", []byte{20}, false},
		{"remove a smartcard", []byte{21}, false},
		{"lock", []byte{22}, false},
		{"unlock", []byte{23}, false},
		{"add a constrained smartcard", []byte{26}, false},
		{"protocol 1's list", []byte{1}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		if got := agentAllowed(tt.msg); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// fakeAgent is an agent that records every request it gets and answers each with SSH_AGENT_SUCCESS.
type fakeAgent struct {
	mu   sync.Mutex
	got  [][]byte
	sock string
}

func newFakeAgent(t *testing.T, sock string) *fakeAgent {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeAgent{sock: sock}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				for {
					m, err := readAgentMsg(c)
					if err != nil {
						return
					}
					f.mu.Lock()
					f.got = append(f.got, m)
					f.mu.Unlock()
					if writeAgentMsg(c, []byte{6}) != nil {
						return
					}
				}
			}()
		}
	}()
	return f
}

func (f *fakeAgent) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// shortAgentDir is a folder whose paths are short enough for sockets.
func shortAgentDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The proxy, run with an agent, relays it on its own socket, mode 600, before it says it's ready:
// a request it passes gets the agent's answer, one it doesn't gets SSH_AGENT_FAILURE and never
// reaches the agent, and a message too long ends the connection. The socket goes with the proxy.
func TestAgentRelay(t *testing.T) {
	t.Parallel()
	const tick = 50 * time.Millisecond
	dir := shortAgentDir(t)
	agent := newFakeAgent(t, filepath.Join(dir, "agent.sock"))
	relay := filepath.Join(dir, "relay.sock")
	must(t, os.WriteFile(relay, nil, 0o600)) // one a proxy before left
	sleep := exec.CommandContext(t.Context(), "sleep", "60")
	must(t, sleep.Start())
	t.Cleanup(func() { _ = sleep.Process.Kill() })
	pidFile := filepath.Join(dir, "ha.pid")
	must(t, os.WriteFile(pidFile, []byte(strconv.Itoa(sleep.Process.Pid)), 0o600))
	errs := &syncBuffer{}
	done := make(chan error)
	go func() {
		done <- run([]string{"-config", writeConfig(t, Config{
			Listen: "127.0.0.1:0", VM: "devvm-x", Password: redact.NewSecret("k3y"), Lifeline: pidFile,
			Agent: agent.sock, AgentSocket: relay,
		})}, IO{Stdout: io.Discard, Stderr: errs}, tick)
	}()
	var c net.Conn
	var err error
	for range 100 {
		if c, err = new(net.Dialer).DialContext(t.Context(), "unix", relay); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(relay); err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("the relay's socket: %v, %v", fi.Mode(), err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	passed := [][]byte{{11}, agentExt(agentSessionBind), {13, 1, 2, 3}}
	for _, m := range [][]byte{{11}, {19}, {22, 0, 0, 0, 1, 'x'}, {17}, agentExt("query"), agentExt(agentSessionBind), {13, 1, 2, 3}, {23}} {
		must(t, writeAgentMsg(c, m))
		reply, err := readAgentMsg(c)
		want := byte(agentFailure)
		if agentAllowed(m) {
			want = 6
		}
		if err != nil || !bytes.Equal(reply, []byte{want}) {
			t.Errorf("request %d: %v, %v", m[0], reply, err)
		}
	}
	if got := agent.requests(); !slices.EqualFunc(got, passed, bytes.Equal) {
		t.Errorf("the agent got %v, want %v", got, passed)
	}
	// Too long: the connection ends, and nothing reaches the agent.
	_, _ = c.Write(binary.BigEndian.AppendUint32(nil, agentMsgMax+1))
	if _, err := readAgentMsg(c); err == nil {
		t.Error("a message too long was answered")
	}
	_ = c.Close()
	if n := len(agent.requests()); n != len(passed) {
		t.Errorf("the agent got %d requests", n)
	}
	if !strings.Contains(errs.String(), "ssh agent: refused a request of type 19") {
		t.Errorf("stderr = %q", errs.String())
	}
	_ = sleep.Process.Kill()
	_ = sleep.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Main: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running after the hostagent was gone: %s", errs.String())
	}
	if _, err := os.Stat(relay); !os.IsNotExist(err) {
		t.Errorf("the relay's socket is still there: %v", err)
	}
}

// A connection that sends no whole request within the idle time is closed, its connection to the
// agent with it; one past the most the relay carries at once is closed at once.
func TestAgentRelayBounds(t *testing.T) {
	t.Parallel()
	dir := shortAgentDir(t)
	agent := newFakeAgent(t, filepath.Join(dir, "agent.sock"))
	// relay starts a relay on name, waiting idle for each request.
	relay := func(name string, idle time.Duration) {
		t.Helper()
		r, err := listenAgent(t.Context(), filepath.Join(dir, name), agent.sock, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		r.idle = idle
		t.Cleanup(r.close)
		go r.serve(t.Context())
	}
	relay("idle.sock", 100*time.Millisecond)
	relay("bound.sock", time.Minute)
	sock := "idle.sock"
	dial := func() net.Conn {
		t.Helper()
		c, err := new(net.Dialer).DialContext(t.Context(), "unix", filepath.Join(dir, sock))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	// Idle after a request, and idle halfway through one.
	idle, half := dial(), dial()
	must(t, writeAgentMsg(idle, []byte{11}))
	if reply, err := readAgentMsg(idle); err != nil || !bytes.Equal(reply, []byte{6}) {
		t.Fatalf("the first request: %v, %v", reply, err)
	}
	_, _ = half.Write([]byte{0, 0, 0, 5, 11})
	for _, c := range []net.Conn{idle, half} {
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("an idle connection: %v, want it closed", err)
		}
	}
	// The bound: with every slot held, one more is closed at once.
	sock = "bound.sock"
	held := make([]net.Conn, 0, agentConnsMax)
	for range agentConnsMax {
		c := dial()
		must(t, writeAgentMsg(c, []byte{11}))
		if _, err := readAgentMsg(c); err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	if _, err := dial().Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("one past the bound: %v, want it closed", err)
	}
	_ = held[0].Close()
}

// emfileListener fails Accept with EMFILE, as a process out of files does, its listener still
// open, until it's closed; each Accept is counted.
type emfileListener struct {
	accepts atomic.Int32
	closed  chan struct{}
}

func (l *emfileListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
}
func (l *emfileListener) Close() error   { close(l.closed); return nil }
func (l *emfileListener) Addr() net.Addr { return &net.UnixAddr{Name: "relay", Net: "unix"} }

// A listener that keeps failing doesn't spin the relay's accept loop: it waits after each error,
// twice as long each time up to a second, logs only the first, and ends with the listener's close.
func TestAgentRelayAcceptRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := &emfileListener{closed: make(chan struct{})}
		var logs syncBuffer
		r := &agentRelay{ln: l, idle: agentIdle, log: log.New(&logs, "", 0)}
		done := make(chan struct{})
		go func() { r.serve(t.Context()); close(done) }()
		// The first try, then after waits of 5, 10, 20, … 640 ms, then a second: 10 tries by
		// 2.275 s.
		time.Sleep(2275*time.Millisecond + time.Millisecond)
		synctest.Wait()
		if n := l.accepts.Load(); n != 10 {
			t.Errorf("%d accepts in 2.275 s, want 10", n)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if n := l.accepts.Load(); n != 20 {
			t.Errorf("%d accepts after 10 s more, want 20: a second's wait each", n)
		}
		if got := strings.Count(logs.String(), "too many open files"); got != 1 {
			t.Errorf("logged the error %d times, want once: %q", got, logs.String())
		}
		r.close()
		<-done
	})
}

// With OpenSSH's own tools, a throwaway agent of the test's own and throwaway keys: through the
// relay, ssh-add lists the agent's key and ssh-keygen signs with it, but ssh-add can't remove it,
// lock the agent or add another key, and the agent still holds its one key, unlocked.
func TestAgentRelayOpenSSH(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " isn't installed")
		}
	}
	dir := shortAgentDir(t)
	agentSock, relaySock := filepath.Join(dir, "agent.sock"), filepath.Join(dir, "relay.sock")
	agent := exec.CommandContext(t.Context(), "ssh-agent", "-D", "-a", agentSock) //nolint:gosec // the test's own socket
	must(t, agent.Start())
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	for range 100 {
		if _, err := os.Stat(agentSock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// with runs an OpenSSH tool with the agent at sock, and an askpass that answers "pw".
	askpass := filepath.Join(dir, "askpass")
	must(t, os.WriteFile(askpass, []byte("#!/bin/sh\necho pw\n"), 0o700)) //nolint:gosec // G306: the test's own script
	with := func(sock string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), args[0], args[1:]...) //nolint:gosec // the test's own tools
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock, "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=:0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, k := range []string{"held", "other"} {
		if out, err := with("", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "devvm-"+k, "-f", filepath.Join(dir, k)); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
	}
	if out, err := with(agentSock, "ssh-add", "-q", filepath.Join(dir, "held")); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}
	r, err := listenAgent(t.Context(), relaySock, agentSock, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	go r.serve(t.Context())

	if out, err := with(relaySock, "ssh-add", "-l"); err != nil || !strings.Contains(out, "devvm-held") {
		t.Errorf("ssh-add -l through the relay: %v\n%s", err, out)
	}
	data := filepath.Join(dir, "data")
	must(t, os.WriteFile(data, []byte("signed"), 0o600))
	if out, err := with(relaySock, "ssh-keygen", "-Y", "sign", "-n", "file", "-f", filepath.Join(dir, "held.pub"), data); err != nil {
		t.Errorf("signing through the relay: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"ssh-add", "-D"},
		{"ssh-add", "-d", filepath.Join(dir, "held.pub")},
		{"ssh-add", "-x"},
		{"ssh-add", filepath.Join(dir, "other")},
		{"ssh-add", "-t", "1", filepath.Join(dir, "other")},
	} {
		if out, err := with(relaySock, args...); err == nil {
			t.Errorf("%v through the relay succeeded:\n%s", args, out)
		}
	}
	if out, err := with(agentSock, "ssh-add", "-l"); err != nil || !strings.Contains(out, "devvm-held") || strings.Contains(out, "devvm-other") {
		t.Errorf("the agent itself: %v\n%s", err, out)
	}
}
