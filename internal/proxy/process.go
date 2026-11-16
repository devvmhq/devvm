package proxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
)

// lifelineTick: how often the broker looks at its VM's lifeline.
const lifelineTick = 3 * time.Second

// lifelineWait bounds how long the broker waits for its VM's lifeline: it starts before the VM does.
const lifelineWait = 15 * time.Minute

// watchLifeline returns once the VM's lifeline (daemon.Lifeline) no longer holds, or never names a
// process that runs, looking every tick. It follows the start it first sees running, not later
// ones: a restarted VM gets a new proxy. A lifeline a start writes whose process is already gone
// (the VM stopped between two looks) ends it too; what the file held when the proxy started is an
// earlier start's, and waited past.
func watchLifeline(path string, tick time.Duration) {
	if l, ok := daemon.FollowLifeline(path, lifelineWait, tick); ok {
		l.Wait(tick)
	}
}

// IO is what devvm _proxy and devvm _proxy-child read and write: devvm's own streams.
type IO struct {
	// Stdout gets data: the event log without Config.Log, and what -help prints.
	Stdout io.Writer
	// Stderr gets the proxy's own log lines.
	Stderr io.Writer
	// OutFile and ErrFile are Stdout and Stderr as the files they are, which the broker gives
	// the child as its own, and which the proxy's own log rotates through (Config.ProxyLog); nil:
	// the null device, and no rotation.
	OutFile, ErrFile *os.File
	// FS is devvm's own files (its assets), which Config.BuiltinPolicies names paths in.
	FS fs.FS
	// Secret looks a binding's value up, in the broker's process (devvm's secrets' store), for
	// Config.Secrets' bindings: required with them.
	Secret Lookup
	// Capture saves the logins in Config.Captures' replies, in the broker's process (devvm's store
	// of agents' logins): required with them.
	Capture Capturer
	// KeepLogs holds Config.VMLogs to their cap until ctx ends, in the broker's process (devvm's
	// vmlog.KeepVM, which rotates them): required with them. They're in the backend's folder for
	// the VM, beside its disk, which the child never writes.
	KeepLogs func(ctx context.Context, paths []string)
	// Seatbelt is the command that starts the child under macOS's Seatbelt, confined from its
	// first instruction to what it needs, to which the broker adds devvm's executable and the
	// child's arguments: sandbox-exec and the child's profile (seatbelt.Sandbox). An error that's
	// fs.ErrNotExist says there's no sandbox-exec. nil: none, elsewhere than macOS.
	Seatbelt func(ChildNeeds) ([]string, error)

	// spawn starts the child: daemon.Spawn, which tests replace with a child in their process.
	spawn func(daemon.Cmd) (childProc, error)
	// inherited are the child's socket to its broker and its listener: its descriptors 3 and 4,
	// which tests replace with their own.
	inherited func() (sock, listener *os.File)
	// confine is how the child confines itself and checks it holds, given how its broker started
	// it and the file it must be refused: confineChild, which tests replace.
	confine func(spawned Confinement, probe string) Confinement
}

// Main runs devvm _proxy with args (its flags): the broker, with -config's file, and its child,
// until the VM's hostagent is gone or ctx ends (devvm stop's SIGTERM). -help prints the flags to
// std.Stdout. Errors are returned, not printed.
func Main(ctx context.Context, args []string, std IO) error { return run(ctx, args, std, lifelineTick) }

// run is Main, looking at the VM's lifeline every tick.
func run(ctx context.Context, args []string, std IO, tick time.Duration) error {
	var configPath string
	fs := newFlags(&configPath)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(std.Stdout)
			fs.PrintDefaults()
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// Without a config, and the password in it, any program on the host could use the proxy, and
	// the VM's tokens with it.
	if configPath == "" {
		return errors.New("-config is required")
	}
	c, err := ReadConfig(configPath)
	if err != nil {
		return err
	}
	if (c.Secrets != "" || c.Upstream != nil && c.Upstream.Secret != "") && std.Secret == nil {
		return errors.New("secrets without a lookup")
	}
	if len(c.Captures) > 0 && std.Capture == nil {
		return errors.New("captures without a capturer")
	}
	if len(c.VMLogs) > 0 && std.KeepLogs == nil {
		return errors.New("the VM's logs without a keeper")
	}
	return runBroker(ctx, c, configPath, std, tick)
}

// proxyLogger is the log a side of the proxy writes its own lines to, std.Stderr, each line
// saying which side wrote it (prefix), rotated past LogLimit through its file when the config
// names the proxy's log (copyRotating).
func proxyLogger(std IO, prefix, proxyLog string) (*log.Logger, error) {
	l := log.New(std.Stderr, prefix, log.LstdFlags)
	if proxyLog != "" && std.ErrFile != nil {
		f, err := copyRotating(std.ErrFile, proxyLog, std.Stderr, LogLimit)
		if err != nil {
			return nil, err
		}
		l.SetOutput(f)
	}
	return l, nil
}

// runBroker runs the broker with c, read from configPath: it reads its own executable's SHA-256
// first, then its config's files, listens on the VM's proxy port, starts the agent's relay and
// the child, and once the child says it's ready, within startWait of the broker's own start,
// writes the child's pid file and how it's confined, then its own status file, which devvm waits
// for (daemon.Ready), and runs with the child (live) until the VM's hostagent is gone (the
// lifeline, looked at every tick), ctx ends, or the child does.
func runBroker(ctx context.Context, c Config, configPath string, std IO, tick time.Duration) error {
	began := time.Now()
	exe, err := selfSum()
	if err != nil {
		return fmt.Errorf("devvm's own executable: %w", err)
	}
	l, err := proxyLogger(std, "devvm proxy: ", c.ProxyLog)
	if err != nil {
		return err
	}
	// The lifeline's watch starts first: a hostagent that ends while the child starts is one the
	// watch saw, never one it takes for an earlier start's.
	var lifeline chan struct{}
	if c.Lifeline != "" {
		lifeline = make(chan struct{})
		go func() { watchLifeline(c.Lifeline, tick); close(lifeline) }()
	}
	b, err := newBroker(ctx, c, std, l)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.Listen) //nolint:noctx // listens for the proxy's whole life, in the child
	if err != nil {
		return err
	}
	// The VM's agent's relay listens before the proxy says it's ready: the VM's backend forwards
	// its socket from the VM's start.
	if c.AgentSocket != "" {
		actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		agent, err := listenAgent(actx, c.AgentSocket, c.Agent, l)
		if err != nil {
			_ = ln.Close()
			return err
		}
		defer agent.close()
		go agent.serve(actx)
	}
	// The backend's logs are held to their cap for as long as the broker runs.
	if len(c.VMLogs) > 0 && std.KeepLogs != nil {
		kctx, stop := context.WithCancel(context.WithoutCancel(ctx))
		defer stop()
		go std.KeepLogs(kctx, c.VMLogs)
	}
	p, ready, confined, err := startChild(b, ln, configPath, std, exe, began.Add(startWait))
	_ = ln.Close()
	if err != nil {
		return err
	}
	p.lifeline = lifeline
	// Both stay once the pair has ended, as the broker's own pid file does: a pid file names a
	// process only while its arguments say it's that one (daemon.Proc), and devvm's stop removes
	// them.
	if err := daemon.WritePID(ChildPIDFile(configPath), p.child.PID()); err != nil {
		l.Printf("the child's pid file: %v", err)
	}
	if err := writeAtomic(ConfinedFile(configPath), []byte(confined.String()+"\n"), 0o600); err != nil {
		l.Printf("how the child is confined: %v", err)
	}
	l.Printf("listening on %s for %s, in a child (pid %d) confined: %s", ready.Addr, c.VM, p.child.PID(), confined)
	// What devvm waits for as it starts the proxy: the address it listens on.
	if err := daemon.Ready(ready.Addr); err != nil {
		l.Printf("%v", err)
	}
	return b.live(ctx, p, ctx.Done())
}

// spawnChild starts the child of the broker whose config is at configPath, with ln, the VM's proxy
// port's listener, and one end of a socketpair, whose other end it returns: sandbox, a sandbox's
// command ending in devvm's executable (childSandbox), or nil for devvm's own executable alone
// (selfExe), then _proxy-child -for configPath, its input the null device, its output and its
// error output the broker's (std.OutFile, std.ErrFile), the socket its descriptor 3 and the
// listener its 4, its environment childEnv's alone, and on Linux killed with the thread that
// started it. The broker's copies of the child's descriptors are closed once it runs.
func spawnChild(ln net.Listener, configPath string, std IO, sandbox []string) (pair, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return pair{}, errors.New("the proxy's listener isn't TCP's")
	}
	lnFile, err := tl.File()
	if err != nil {
		return pair{}, err
	}
	defer func() { _ = lnFile.Close() }()
	ends, err := socketpair()
	if err != nil {
		return pair{}, err
	}
	brokerEnd, childEnd := os.NewFile(uintptr(ends[0]), "broker"), os.NewFile(uintptr(ends[1]), "child")
	defer func() { _ = brokerEnd.Close() }()
	cmd := sandbox
	if len(cmd) == 0 {
		exe, err := selfExe()
		if err != nil {
			_ = childEnd.Close()
			return pair{}, err
		}
		cmd = []string{exe}
	}
	spawn := std.spawn
	if spawn == nil {
		spawn = func(c daemon.Cmd) (childProc, error) { return daemon.Spawn(c) }
	}
	child, err := spawn(daemon.Cmd{
		Path: cmd[0], Args: append(slices.Clone(cmd[1:]), ChildCommand, "-for", configPath), Out: std.OutFile, Err: std.ErrFile,
		Files: []*os.File{childEnd, lnFile}, Env: childEnv(os.Environ()), DieWithThread: true,
	})
	_ = childEnd.Close()
	if err != nil {
		return pair{}, err
	}
	conn, err := net.FileConn(brokerEnd)
	if err != nil {
		child.Kill()
		return pair{}, err
	}
	return pair{fc: newFrameConn(conn), child: child}, nil
}

// checkInherited checks the child's descriptors 3 and 4 are what its broker passes: a unix socket
// and a TCP listener (inheritedSocket). Run by hand, without them, those numbers can be the Go
// runtime's own, such as its poller's, which wrapping and then closing would crash it, so the child
// refuses before it touches either.
func checkInherited() error {
	if err := inheritedSocket(3, true); err != nil {
		return fmt.Errorf("descriptor 3, the socket to the broker: %w; only devvm _proxy starts devvm %s", err, ChildCommand)
	}
	if err := inheritedSocket(4, false); err != nil {
		return fmt.Errorf("descriptor 4, the proxy's listener: %w; only devvm _proxy starts devvm %s", err, ChildCommand)
	}
	return nil
}

// inheritedSocket checks fd is a socket, a unix one when unix is set and an IP one otherwise, by
// its own address alone, which reads nothing from it and changes nothing.
func inheritedSocket(fd int, unix bool) error {
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return err
	}
	switch sa.(type) {
	case *syscall.SockaddrUnix:
		if unix {
			return nil
		}
	case *syscall.SockaddrInet4, *syscall.SockaddrInet6:
		if !unix {
			return nil
		}
	}
	return errors.New("not the socket the broker passes")
}

// ChildMain runs devvm _proxy-child with args, -for FILE, its broker's config, which it names only
// so the child can be told apart and never opens: the child, on its descriptor 3, the socket to
// its broker, and its 4, the VM's proxy port's listener, until the broker's end closes. -help
// prints the flag to std.Stdout. Errors are returned, not printed.
func ChildMain(args []string, std IO) error {
	var forConfig string
	fs := flag.NewFlagSet("devvm "+ChildCommand, flag.ContinueOnError)
	fs.StringVar(&forConfig, "for", "", "the broker's config, which the child never opens: so it can be told apart")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(std.Stdout)
			fs.PrintDefaults()
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if forConfig == "" {
		return errors.New("-for is required")
	}
	exe, err := selfSum()
	if err != nil {
		return fmt.Errorf("devvm's own executable: %w", err)
	}
	inherited := std.inherited
	if inherited == nil {
		if err := checkInherited(); err != nil {
			return err
		}
		inherited = func() (*os.File, *os.File) { return os.NewFile(3, "broker"), os.NewFile(4, "listener") }
	}
	sock, lnFile := inherited()
	defer func() { _ = sock.Close(); _ = lnFile.Close() }()
	conn, err := net.FileConn(sock)
	if err != nil {
		return fmt.Errorf("the socket to the broker: %w", err)
	}
	ln, err := net.FileListener(lnFile)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("the proxy's listener: %w", err)
	}
	defer func() { _ = ln.Close() }()
	return runChildOn(newFrameConn(conn), ln, std, exe, forConfig, log.New(std.Stderr, "devvm proxy child: ", log.LstdFlags))
}
