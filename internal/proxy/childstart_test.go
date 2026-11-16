package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/redact"
)

// endedChild is a child that had ended by the time it was started, with its exit status code, or
// killed by sig, as sandbox-exec ends when it can't start its program.
type endedChild struct {
	code int
	sig  syscall.Signal
	done chan struct{}
}

// ended is how a child ended: an exit status from 0, or a signal's number, negated.
type ended int

func newEndedChild(e ended) *endedChild {
	c := &endedChild{code: int(e), done: make(chan struct{})}
	if e < 0 {
		c.code, c.sig = -1, syscall.Signal(-e)
	}
	close(c.done)
	return c
}

func (c *endedChild) PID() int                { return 0 }
func (c *endedChild) Done() <-chan struct{}   { return c.done }
func (c *endedChild) Stop(time.Duration) bool { return true }
func (c *endedChild) Kill()                   {}
func (c *endedChild) Exit() string            { return fmt.Sprintf("exit status %d", c.code) }
func (c *endedChild) ExitCode() int           { return c.code }

func (c *endedChild) ExitSignal() syscall.Signal { return c.sig }

// The start of a child: under the sandbox std.Seatbelt gives, with devvm's executable and the
// child's arguments after its command, confined as the child says; unconfined, and why, with no
// sandbox, none there, or a sandbox that failed to start it (sandbox-exec's own exit status, or a
// signal); the proxy's log warns of each child that isn't fully confined; and a child that ended
// any other way fails the start, with no second try.
func TestStartChildSandbox(t *testing.T) {
	t.Parallel()
	exe, err := selfSum()
	must(t, err)
	sandbox := []string{"/sbx", "-p", "(profile)"}
	noSandbox := func(ChildNeeds) ([]string, error) {
		return nil, &fs.PathError{Op: "stat", Path: "/sbx", Err: fs.ErrNotExist}
	}
	for _, tc := range []struct {
		name     string
		seatbelt func(ChildNeeds) ([]string, error)
		ended    *ended // how the sandboxed child ended, or nil for one that runs
		want     string
		starts   int // how many children were started
		fails    bool
		warns    bool
		says     string // what the child says of its confinement, when it's another than it was given
	}{
		{name: "no sandbox", want: "none unsupported", starts: 1, warns: true},
		{name: "sandbox-exec missing", seatbelt: noSandbox, want: "none sandbox-exec-missing", starts: 1, warns: true},
		{name: "a sandbox that failed to be made", seatbelt: func(ChildNeeds) ([]string, error) {
			return nil, errors.New("no profile")
		}, want: "none sandbox-exec-failed", starts: 1, warns: true},
		{name: "sandboxed", want: "full seatbelt", starts: 1},
		{name: "a profile refused", ended: new(ended(65)), want: "none sandbox-exec-failed", starts: 2, warns: true},
		{name: "a program it couldn't run", ended: new(ended(71)), want: "none sandbox-exec-failed", starts: 2, warns: true},
		{name: "aborted as it loaded", ended: new(-ended(syscall.SIGABRT)), want: "none sandbox-exec-failed", starts: 2, warns: true},
		{name: "killed as it loaded", ended: new(-ended(syscall.SIGKILL)), want: "none sandbox-exec-failed", starts: 2, warns: true},
		{name: "ended by SIGTERM", ended: new(-ended(syscall.SIGTERM)), starts: 1, fails: true},
		{name: "the child's own failure", ended: new(ended(1)), starts: 1, fails: true},
		{name: "an unknown confinement", says: "jailed", starts: 1, fails: true},
		{name: "a sandbox it wasn't given", seatbelt: noSandbox, says: "full seatbelt", starts: 1, fails: true, warns: true},
		{name: "unconfined, as it was started", seatbelt: noSandbox, says: "none sandbox-exec-missing", want: "none sandbox-exec-missing", starts: 1, warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seatbelt := tc.seatbelt
			if seatbelt == nil && tc.name != "no sandbox" {
				seatbelt = func(ChildNeeds) ([]string, error) { return sandbox, nil }
			}
			var logged bytes.Buffer
			var cmds []daemon.Cmd
			std := inProcess(IO{
				Stdout: io.Discard, Stderr: io.Discard, Seatbelt: seatbelt,
				confine: func(spawned Confinement, _ string) Confinement {
					if tc.says != "" {
						c, err := ParseConfinement(tc.says)
						if err != nil {
							return Confinement{Level: tc.says}
						}
						return c
					}
					return spawned
				},
			})
			run := std.spawn
			std.spawn = func(c daemon.Cmd) (childProc, error) {
				cmds = append(cmds, c)
				if c.Path == sandbox[0] && tc.ended != nil {
					return newEndedChild(*tc.ended), nil
				}
				return run(c)
			}
			config := filepath.Join(t.TempDir(), "proxy.json")
			b, err := newBroker(t.Context(), Config{VM: "devvm-x", Password: redact.NewSecret("pw")}, std, log.New(&logged, "", 0))
			must(t, err)
			ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
			must(t, err)
			defer ln.Close()
			p, _, confined, err := startChild(b, ln, config, std, exe, time.Now().Add(startWait))
			if (err != nil) != tc.fails {
				t.Fatalf("start: %v", err)
			}
			if len(cmds) != tc.starts {
				t.Errorf("%d children started, want %d", len(cmds), tc.starts)
			}
			if strings.Contains(logged.String(), "warning") != tc.warns {
				t.Errorf("the log: %q", logged.String())
			}
			if tc.fails {
				return
			}
			if confined.String() != tc.want {
				t.Errorf("confined %q, want %q", confined, tc.want)
			}
			last := cmds[len(cmds)-1]
			if tail := last.Args[len(last.Args)-3:]; !slices.Equal(tail, []string{ChildCommand, "-for", config}) {
				t.Errorf("the child's arguments: %q", last.Args)
			}
			if sandboxed := last.Path == sandbox[0]; sandboxed != (confined.By == BySeatbelt) {
				t.Errorf("confined %q, started by %s %q", confined, last.Path, last.Args)
			} else if sandboxed && !slices.Equal(last.Args[:len(sandbox)-1], sandbox[1:]) {
				t.Errorf("the sandbox's command: %q", last.Args)
			}
			_ = p.fc.close()
			<-p.child.Done()
		})
	}
}

// What the child needs: devvm's executable by its real path, the folders of its logs, once
// each, and the Nix cache's, and the files it looks at.
func TestChildNeeds(t *testing.T) {
	t.Parallel()
	c := Config{ //nolint:gosec // G101: the bindings file's path, not a credential
		Log: "/d/logs/network.jsonl", ProxyLog: "/d/logs/proxy.log", NixCache: "/c/nix",
		Policies: []string{"/p/a.policy"}, Secrets: "/r/secrets.json", Links: "/r/links.json",
	}
	n, err := childNeeds(c, watchedFiles(c))
	must(t, err)
	if !slices.Equal(n.Folders, []string{"/d/logs", "/c/nix"}) {
		t.Errorf("folders %q", n.Folders)
	}
	if !slices.Equal(n.Watched, []string{"/p/a.policy", "/r/secrets.json", "/r/links.json"}) {
		t.Errorf("watched %q", n.Watched)
	}
	if self, err := os.Executable(); err != nil {
		t.Fatal(err)
	} else if resolved, err := filepath.EvalSymlinks(self); err != nil || n.Exe != resolved {
		t.Errorf("exe %q, want %q (%v)", n.Exe, resolved, err)
	}
	if n, err := childNeeds(Config{}, nil); err != nil || n.Folders != nil || n.Watched != nil {
		t.Errorf("nothing: %+v, %v", n, err)
	}
	// A relative path is the broker's folder's, made absolute for the profile.
	wd, err := os.Getwd()
	must(t, err)
	rel := Config{Log: "events.jsonl", NixCache: "cache", Policies: []string{"nix.policy"}}
	if n, err := childNeeds(rel, watchedFiles(rel)); err != nil || !slices.Equal(n.Folders, []string{wd, filepath.Join(wd, "cache")}) ||
		!slices.Equal(n.Watched, []string{filepath.Join(wd, "nix.policy")}) {
		t.Errorf("relative: %+v, %v", n, err)
	}
}

// A child that still runs, as one the start's timer gave up on, never counts as the sandbox's
// failure, whatever the broker's stop then ends it with; nor does an error but ending early.
func TestSandboxFailed(t *testing.T) {
	t.Parallel()
	running := newFakeChild()
	if sandboxFailed(errEndedEarly, running) {
		t.Error("a child that still runs")
	}
	if sandboxFailed(errors.New("the child failed"), newEndedChild(ended(65))) {
		t.Error("an answer of failed")
	}
	if !sandboxFailed(fmt.Errorf("%w (exit status 65)", errEndedEarly), newEndedChild(ended(65))) {
		t.Error("sandbox-exec's own status")
	}
}
