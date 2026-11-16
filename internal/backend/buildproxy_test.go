package backend

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
)

// A build's proxy starts with its own password, its policy and the
// company proxy in its config, the user's alone, and none of them in its
// arguments; it's found by the port its status file names. Stop ends it
// and deletes the config, leaving the logs.
func TestProxyStarter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "build")
	var args []string
	var ended <-chan struct{}
	s := ProxyStarter{Self: "/devvm", Tick: time.Millisecond, Spawn: func(c daemon.Cmd) (*daemon.Handle, error) {
		args = append([]string{c.Path}, c.Args...)
		h, err := sleeper(t, c, "127.0.0.1:40123")
		if err == nil {
			ended = h.Done()
		}
		return h, err
	}}
	up := &proxy.Upstream{HTTPS: corpProxy}
	p, err := s.Start(dir, proxy.Config{VM: "devvm-img-x", Lifeline: "/run/ha.pid", Upstream: up, UpstreamUnresolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Port != 40123 || len(p.Password.Reveal()) != 64 {
		t.Errorf("port %d, password %q", p.Port, p.Password.Reveal())
	}
	config := filepath.Join(dir, "proxy.json")
	if want := []string{"/devvm", "_proxy", "-config", config}; !slices.Equal(args, want) {
		t.Errorf("arguments %q, want %q", args, want)
	}
	c, err := proxy.ReadConfig(config)
	if want := (proxy.Config{
		Version: proxy.ConfigVersion, Listen: "127.0.0.1:0", VM: "devvm-img-x", Password: p.Password,
		Policies: []string{filepath.Join(dir, "policy")}, Log: filepath.Join(dir, "logs", "events.jsonl"),
		// Its own log, held to its limit as a VM's proxy holds its own.
		ProxyLog: filepath.Join(dir, "logs", "proxy.log"), Lifeline: "/run/ha.pid", Upstream: up, UpstreamUnresolved: true,
	}); err != nil || !reflect.DeepEqual(c, want) {
		t.Errorf("config = %+v, %v; want %+v", c, err, want)
	}
	for _, f := range []string{"proxy.json", "policy"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "policy")); !strings.HasSuffix(string(b), "\n**\n") { //nolint:gosec // the test's own folder
		t.Errorf("policy = %q", b)
	}
	p.Stop()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("not stopped")
	}
	if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its config stayed: %v", err)
	}
	// The logs, in a folder of their own, which the child writes.
	if fi, err := os.Stat(filepath.Join(dir, "logs", "proxy.log")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the log went: %v %v", fi, err)
	}

	// Without a company proxy, none in its config.
	if p, err = s.Start(dir, proxy.Config{VM: "devvm-img-x", Lifeline: "/run/ha.pid"}); err != nil {
		t.Fatal(err)
	}
	if c, err := proxy.ReadConfig(config); err != nil || c.Upstream != nil || c.UpstreamUnresolved {
		t.Errorf("no upstream: %+v %v", c, err)
	}
	p.Stop()
}

// sleeper starts a sleep in c's place that says it's ready with say in
// c's status file ("" for never), stopped when the test ends.
func sleeper(t *testing.T, c daemon.Cmd, say string) (*daemon.Handle, error) {
	t.Helper()
	h, err := daemon.Spawn(daemon.Cmd{Path: "sleep", Args: []string{"60"}, Out: c.Out})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { h.Stop(time.Second) })
	if say != "" {
		err = daemon.WriteStatus(c.Status, h.PID(), say)
	}
	return h, err
}

// A proxy that never says it listens, or says so of an address that
// isn't loopback or has no port, is stopped, naming its log; so is one
// that said it listens but has already ended (another program took the
// port from it).
func TestProxyStarterFails(t *testing.T) {
	for _, say := range []string{"", "192.0.2.1:3128", "127.0.0.1:0", "127.0.0.1:x", "ended"} {
		dir := t.TempDir()
		var h *daemon.Handle
		s := ProxyStarter{Self: "/devvm", Tick: time.Millisecond, Spawn: func(c daemon.Cmd) (*daemon.Handle, error) {
			if say != "ended" {
				var err error
				h, err = sleeper(t, c, say)
				return h, err
			}
			var err error
			if h, err = daemon.Spawn(daemon.Cmd{Path: "true", Out: c.Out}); err != nil {
				return nil, err
			}
			<-h.Done()
			return h, daemon.WriteStatus(c.Status, h.PID(), "127.0.0.1:40123")
		}}
		_, err := s.Start(dir, proxy.Config{VM: "b", Lifeline: "/l"})
		if msg.NamesOf(err) != "proxy_start_failed log="+filepath.Join(dir, "logs", "proxy.log")+" vm=b" {
			t.Errorf("%q: Start = %v", say, err)
		}
		select {
		case <-h.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("%q: left running", say)
		}
		if _, err := os.Stat(filepath.Join(dir, "proxy.json")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: its config, with its password, stayed: %v", say, err)
		}
	}
	// One that can't be started at all leaves no config either.
	dir := t.TempDir()
	s := ProxyStarter{Self: "/devvm", Tick: time.Millisecond, Spawn: func(daemon.Cmd) (*daemon.Handle, error) {
		return nil, errors.New("no such program")
	}}
	if _, err := s.Start(dir, proxy.Config{VM: "b", Password: redact.NewSecret("x")}); err == nil {
		t.Error("Start: no error")
	}
	if _, err := os.Stat(filepath.Join(dir, "proxy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a proxy that never ran left its config: %v", err)
	}
}

// A build's proxy is stopped through the process devvm started: one still
// running ends; one that has ended is left alone (no signal reaches a
// process given its pid since); none is nothing.
func TestBuildProxyStop(t *testing.T) {
	h, err := daemon.Spawn(daemon.Cmd{Path: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatal(err)
	}
	bp := &BuildProxy{proc: h, dir: t.TempDir()}
	bp.Stop()
	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("not stopped")
	}
	bp.Stop()
	var none *BuildProxy
	none.Stop()
}
