package vm

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// linkRuns starts and stops client VMs' link sessions: devvm _links VM,
// one detached process for each running client with links, its output in
// its log (paths.VM.LinksLog), its pid in its run folder
// (paths.VM.LinksPID). The proxies' seams start, find and signal them
// (Proxy.Start, Args and Signal), through daemon.Proc.
type linkRuns struct{ *Env }

// linksLogMax is the most a link session's log keeps: one over it is
// emptied when the next session starts.
const linksLogMax = 1 << 20

// linksReportWait bounds how long devvm waits for a session it started,
// or one whose links changed, to say what came of each link.
const linksReportWait = 10 * time.Second

// proc is vm's link session as a background process: its pid file in the
// run folder, and devvm _links VM its arguments.
func (e linkRuns) proc(vm vmname.VM) daemon.Proc {
	return daemon.Proc{
		PIDFile: e.Dirs.VM(vm).LinksPID(), Args: e.Proxy.Args, Signal: e.Proxy.Signal,
		Is: func(args []string) bool { return len(args) == 3 && args[1] == "_links" && args[2] == vm.String() },
	}
}

// pid returns the pid of vm's running link session: the process its pid
// file names, if that still runs as vm's devvm _links.
func (e linkRuns) pid(vm vmname.VM) (int, bool) { return e.proc(vm).PID() }

// stop stops vm's link session, if it runs: SIGTERM, which ends its
// connections and removes its sockets, then after two seconds SIGKILL.
func (e linkRuns) stop(ctx context.Context, vm vmname.VM) {
	if !e.proc(vm).Running() {
		return
	}
	defer e.removeSockets(ctx, vm)
	e.proc(vm).Stop(4 * e.Proxy.Tick)
}

// removeSockets deletes what a session of vm's that was killed leaves
// behind and nothing listens on: each of its links' sockets and its own
// master's control socket. Its masters' to targets, named by their starts
// too, are devvm gc --orphans's. Its list for the VM's proxy stays: with
// no socket to reach, the proxy refuses those links rather than ask its
// rules, and a new session writes it again.
func (e linkRuns) removeSockets(ctx context.Context, vm vmname.VM) {
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		return
	}
	links, _ := ReadLinks(st)
	s := vmssh.Links{VM: vm, Dir: e.Dirs.Run()}
	paths := []string{s.Control("", "")}
	for _, l := range links {
		paths = append(paths, s.Socket(vmssh.Link{Addr: l.Addr, Port: l.Port}))
	}
	for _, p := range paths {
		_ = RemoveLeftoverSocket(context.WithoutCancel(ctx), p)
	}
}

// logSize is how long vm's link session's log is now: what's written
// after it is new.
func (e linkRuns) logSize(vm vmname.VM) int64 {
	fi, err := os.Stat(e.Dirs.VM(vm).LinksLog())
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ensure makes sure vm's link session runs, starting it when it doesn't.
// It reports whether it started one, and where in its log the new
// session's lines start.
func (e linkRuns) ensure(vm vmname.VM) (bool, int64, error) {
	if _, ok := e.pid(vm); ok {
		return false, 0, nil
	}
	from, err := e.start(vm)
	return err == nil, from, err
}

// start starts vm's link session, detached, its output in its log, and
// returns how long that log was before.
func (e linkRuns) start(vm vmname.VM) (int64, error) {
	files := e.Dirs.VM(vm)
	logPath := files.LinksLog()
	for _, dir := range []string{files.Logs(), files.Run(), e.Dirs.Run()} {
		if err := state.PrivateDir(dir); err != nil {
			return 0, err
		}
	}
	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > linksLogMax {
		flags |= os.O_TRUNC //nolint:forbidigo // the session's log, which it writes as it runs: started again empty once too long
	}
	out, err := os.OpenFile(logPath, flags, 0o600) //nolint:gosec // devvm's own log
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Close() }()
	fi, err := out.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := e.proc(vm).Start(e.Proxy.Start, daemon.Cmd{Path: e.Proxy.Self, Args: []string{"_links", vm.String()}, Out: out}); err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// report waits, up to linksReportWait, for vm's link session to say what
// came of links, in its log after from, and warns of each the VM refused:
// a program there holds the link's address and port.
func (e linkRuns) report(ctx context.Context, vm vmname.VM, links []Link, from int64) {
	want := make([]vmssh.Link, len(links))
	for i, l := range links {
		want[i] = vmssh.Link{Name: l.Name, Target: l.Target, Addr: l.Addr, Port: l.Port}
	}
	var r vmssh.LinksReport
	deadline := time.Now().Add(linksReportWait)
	for {
		data, _ := os.ReadFile(e.Dirs.VM(vm).LinksLog())
		if int64(len(data)) < from {
			from = 0 // emptied since
		}
		r = vmssh.ReadLinksLog(data[from:], vm, want)
		_, running := e.pid(vm)
		if r.Settled || !running || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, l := range r.Refused {
		e.Warn(Link{Name: l.Name, Port: l.Port}.PortTaken(vm))
	}
}

// atLaunch runs a launched client's link session, once it runs and its
// setup is done: its backend must have links, each stopped target and
// each whose policy rule no longer holds is warned of once, and a session
// it starts is waited for to say what came of each link.
func (e linkRuns) atLaunch(ctx context.Context, b backend.Backend, st *state.VM) error {
	vm := st.Name()
	links, err := ReadLinks(st)
	if err != nil || len(links) == 0 {
		return err
	}
	if err := e.Registry.Require(b, vm, linkFeatures()...); err != nil {
		return err
	}
	if err := e.warnTargets(ctx, st, links); err != nil {
		return err
	}
	started, from, err := e.ensure(vm)
	if err != nil {
		return err
	}
	if started {
		e.report(ctx, vm, links, from)
	}
	return nil
}

// warnTargets warns once for each target of a client's links that its
// links can't reach now: one whose policy rule no longer holds, which the
// client starts without (guestLinks), one whose backend lacks links, one
// that's stopped, which devvm doesn't start, and, for a client on a
// confined tier, one that runs with its network tier not in force
// (policyUnenforced), until it starts again.
func (e linkRuns) warnTargets(ctx context.Context, st *state.VM, links []Link) error {
	seen := map[string]bool{}
	for _, l := range links {
		key := l.Target.String() + "\x00" + l.TargetID
		if seen[key] {
			continue
		}
		seen[key] = true
		why, tst, err := CheckLink(e.Dirs, st, l)
		switch {
		case err != nil:
			return err
		case why == LinkGone || why == LinkReplaced || why == LinkUngrouped:
			continue // devvm link says so
		case !linkPolicy(st, tst):
			e.Warn(tierMismatch(st, tst))
			continue
		}
		c, err := e.Registry.For(tst)
		if err != nil {
			return err
		}
		if err := e.Registry.Require(c.Backend, l.Target, linkFeatures()...); err != nil {
			e.Warn(msg.LinkTargetUnusable(l.Target.String(), st.Name()).Because(err))
			continue
		}
		in, err := c.Backend.Inspect(ctx, l.Target)
		switch {
		case err != nil || in.Status != backend.StatusRunning:
			e.Warn(msg.LinkTargetStopped(l.Target.String(), st.Name()))
		case policyUnenforced(ctx, c.Backend, st, tst):
			e.Warn(msg.LinkTargetUnenforced(l.Target.String(), st.Name()))
		}
	}
	return nil
}

// guestLinks returns the links a client's start and setup give it
// (Env.Links): an address and a name for each target, by address, but
// none for a target whose policy rule no longer holds, which the client
// starts without.
func guestLinks(d paths.Dirs, st *state.VM) ([]GuestLink, error) {
	links, err := ReadLinks(st)
	if err != nil {
		return nil, err
	}
	var out []GuestLink
	for _, l := range links {
		tst, err := state.ReadVM(d, l.Target)
		if err != nil {
			return nil, err
		}
		if tst.Exists() && tst.ID == l.TargetID && !linkPolicy(st, tst) ||
			slices.ContainsFunc(out, func(g GuestLink) bool { return g.Addr == l.Addr }) {
			continue
		}
		out = append(out, GuestLink{Addr: l.Addr, Names: []string{l.Name}})
	}
	slices.SortFunc(out, func(a, b GuestLink) int { return a.Addr.Compare(b.Addr) })
	return out, nil
}

// apply gives a running client its changed links: its setup sent again,
// for its /etc/hosts and NO_PROXY settings, and its link session running,
// which reads its links again each second. It waits for the session to
// say what came of added, the links that are new, in its log after from
// (logSize, before the change was written). It reports whether the client
// runs: a stopped one gets them at its next start.
func (e linkRuns) apply(ctx context.Context, b backend.Backend, st *state.VM, added []Link, from int64) (bool, error) {
	vm := st.Name()
	in, err := b.Inspect(ctx, vm)
	if err != nil || in.Status != backend.StatusRunning {
		return false, err
	}
	// The confinement it runs with, its agent included, for its setup
	// sent again.
	mode, err := runningAgent(ctx, b, st)
	if err != nil {
		return true, err
	}
	conf, err := e.starter.runningConfinement(ctx, b, st, mode)
	if err != nil {
		return true, err
	}
	slug := project.Folder{Name: vm}.Slug()
	if err := e.syncer.sync(ctx, b, st, slug, tools.Files(st), conf); err != nil {
		return true, err
	}
	links, err := ReadLinks(st)
	if err != nil || len(links) == 0 {
		return true, err // its session ends on its own, having none
	}
	if err := e.Registry.Require(b, vm, linkFeatures()...); err != nil {
		return true, err
	}
	started, start, err := e.ensure(vm)
	switch {
	case err != nil:
		return true, err
	case started:
		e.report(ctx, vm, links, start)
	case len(added) > 0:
		e.report(ctx, vm, added, from)
	}
	return true, nil
}

// LeftoverSockets returns the link sessions' sockets in the run folder
// that nothing listens on any more, which a session that was killed
// leaves behind: devvm gc --orphans deletes them, and devvm space counts
// them.
func LeftoverSockets(ctx context.Context, d paths.Dirs) ([]string, error) {
	var out []string
	for _, ext := range []string{".sock", ".ctl"} {
		m, err := filepath.Glob(filepath.Join(d.Run(), "link-*"+ext))
		if err != nil {
			return nil, err
		}
		for _, p := range m {
			if staleSocket(ctx, p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// RemoveLeftoverSocket deletes one of LeftoverSockets's, checked again
// first: one that something listens on again stays.
func RemoveLeftoverSocket(ctx context.Context, path string) error {
	if !staleSocket(ctx, path) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// staleSocket reports whether path is a unix socket that refuses a
// connection.
func staleSocket(ctx context.Context, path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return ctx.Err() == nil
	}
	_ = c.Close()
	return false
}
