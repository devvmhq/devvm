package proxy

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/redact"
)

// newServer is the proxy c describes in the test's process: its broker (newBroker), whose values
// come from std.Secret and whose logins go to std.Capture, and the child's server, which asks the
// broker as its hostSide directly, with no socket between them, logging to l and its events to
// out. Its policy files are read once, as the child reads them once it's ready. It looks up its
// names and connects in the test's process, as the proxy did before its split, so a test of the
// child's own floor and policy can change them (s.egress) without the broker's; the tests that
// run the child over a socket (inProcess) and dial_test.go's send them through the broker.
func newServer(c Config, std IO, l *log.Logger, out io.Writer) (*server, error) {
	b, err := newBroker(context.Background(), c, std, l) //nolint:forbidigo // the tests' own servers, made where no test's context is passed
	if err != nil {
		return nil, err
	}
	s, err := newChildServer(b.start, b, std.FS, l, out)
	if err != nil {
		return nil, err
	}
	s.egress.lookup, s.egress.connect = net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: dialWait}).DialContext
	for _, u := range []*upstream{s.egress.up.https, s.egress.up.http} {
		if u != nil {
			u.dial = nil
		}
	}
	s.egress.pol.all()
	return s, nil
}

// newSecrets is the child's bindings in path ("": none), read in the test's process, their
// values from lookup, through a broker's own read of the same file (lookups).
func newSecrets(path string, lookup Lookup, l *log.Logger) *secrets {
	k := newLookups(path, lookup, l)
	var src watchSource
	if path != "" {
		src = watchedFile{watchBindings, path}.local()
	}
	return newSecretsOf(src, func(ctx context.Context, b Binding) (redact.Secret, time.Time, error) {
		return k.value(ctx, valueKeyOf(b))
	}, l)
}

// inProcess is std with a spawn that runs the broker's child in the test's process, on the
// socket and the listener the broker gives it, as devvm _proxy-child runs it: the test's binary
// isn't devvm's, so it can't be the child. Its confinement is as the broker started it (noConfine),
// as confineChild would confine the test's own process on Linux. Its broker waits testStartWait
// for the child, and the test's executable is hashed once for all its children.
func inProcess(std IO) IO {
	if std.confine == nil {
		std.confine = noConfine
	}
	std.dumpable = true
	if std.startWait == 0 {
		std.startWait = testStartWait
	}
	sum := sync.OnceValues(selfSum)
	std.spawn = func(c daemon.Cmd) (childProc, error) {
		conn, err := net.FileConn(c.Files[0])
		if err != nil {
			return nil, err
		}
		ln, err := net.FileListener(c.Files[1])
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		exe, err := sum()
		if err != nil {
			_ = conn.Close()
			_ = ln.Close()
			return nil, err
		}
		g := &goChild{done: make(chan struct{}), stop: func() { _ = conn.Close(); _ = ln.Close() }}
		go func() {
			g.err = runChildOn(newFrameConn(conn), ln, std, exe, "", log.New(std.Stderr, "devvm proxy child: ", log.LstdFlags))
			_ = ln.Close()
			close(g.done)
		}()
		return g, nil
	}
	return std
}

// testStartWait is how long a test's broker waits for its child to say it's ready (IO.startWait),
// and how long a test waits for a proxy to listen: a child in the test's process starts among the
// package's other tests, and on a loaded host under the race detector, hashing the test's
// executable (selfSum) alone can take most of startWait.
const testStartWait = 30 * time.Second

// retryFor calls try every 20 ms until it returns nil or d has passed, and returns its last error:
// for a proxy a test started, which listens once its broker and its child have started.
func retryFor(d time.Duration, try func() error) error {
	end := time.Now().Add(d)
	for {
		err := try()
		if err == nil || time.Now().After(end) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// noConfine is a test's child's confinement: as its broker started it, with nothing done in the
// test's process.
func noConfine(spawned Confinement, _ ChildNeeds, _ string) (Confinement, error) { return spawned, nil }

// goChild is a child the test runs in its own process (inProcess): stopping or killing it closes
// its socket and its listener, which ends it.
type goChild struct {
	done chan struct{}
	err  error
	stop func()
}

func (g *goChild) PID() int              { return os.Getpid() }
func (g *goChild) Done() <-chan struct{} { return g.done }
func (g *goChild) Kill()                 { g.stop() }

func (g *goChild) Stop(wait time.Duration) bool {
	g.stop()
	select {
	case <-g.done:
		return true
	case <-time.After(wait):
		return false
	}
}

// ExitCode is 1 for a child that returned an error, 0 for one that didn't, and -1 while it runs.
func (g *goChild) ExitCode() int {
	select {
	case <-g.done:
		if g.err != nil {
			return 1
		}
		return 0
	default:
		return -1
	}
}

// ExitSignal is none: a child in the test's process ends by returning.
func (g *goChild) ExitSignal() syscall.Signal { return 0 }

func (g *goChild) Exit() string {
	select {
	case <-g.done:
		if g.err != nil {
			return g.err.Error()
		}
		return "exit status 0"
	default:
		return "running"
	}
}

// loadCA is the CA whose files are certPath and keyPath, read as the broker reads them (readCA)
// and parsed as the child does (parseCA).
func loadCA(certPath, keyPath string) (*ca, error) {
	certPEM, keyPEM, err := readCA(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return parseCA(certPEM, keyPEM)
}

// capturerFunc is an IO.Capture whose Request is f and whose Response saves nothing.
type capturerFunc func(x Exchange) (out, answer []byte, err error)

func (f capturerFunc) Request(_ context.Context, x Exchange) ([]byte, []byte, error) { return f(x) }

func (f capturerFunc) Response(_ context.Context, x Exchange, _ []byte) ([]byte, string, error) {
	return []byte(`{}`), x.Agents[0], nil
}

// testCaptures is s's captures of list, their logins saved through c by a broker in the test's
// process, which reaches each endpoint's host on the test's port rather than 443, trusting s's
// roots.
func testCaptures(s *server, list []Capture, c Capturer, port int) *captures {
	x := &exchanger{
		vm: s.vm, list: list, c: c, log: s.log, roots: s.upstreamCAs, route: direct,
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, p, err := net.SplitHostPort(addr); err == nil && p == "443" {
				addr = net.JoinHostPort(host, strconv.Itoa(port))
			}
			return new(net.Dialer).DialContext(ctx, network, addr)
		},
	}
	return newCaptures(list, x.exchange)
}
