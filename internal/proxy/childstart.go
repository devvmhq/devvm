package proxy

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// The exit statuses of sandbox-exec's own failures, before the child's program runs: a profile it
// refused (EX_DATAERR) and a program it couldn't run (EX_OSERR).
const (
	sandboxExecRefused = 65
	sandboxExecNoRun   = 71
)

// startChild starts the broker's child with ln, the VM's proxy port's listener, and waits until
// deadline for it to say it's ready (handshake): under std.Seatbelt's sandbox where there is one
// (childSandbox). A child that the sandbox failed to start (sandboxFailed) is started again
// unconfined within the same deadline, with a warning in the proxy's log: sandbox-exec's failures
// come before devvm's code runs, so nothing of a VM's can cause them. A child that answers failed,
// ends any other way or doesn't answer in time fails the start, as what it read by then could be a
// VM's, and so does a ready that claims a sandbox the broker didn't start it under
// (Confinement.fits). It returns the child, its ready and how it's confined, which the proxy's log
// warns of when it isn't fully.
func startChild(b *broker, ln net.Listener, configPath string, std IO, exe string, deadline time.Time) (pair, wireReady, Confinement, error) {
	// The child makes regular files alone, so the Nix cache's folders are made before it starts.
	if b.cfg.NixCache != "" {
		if err := makeNixFolders(b.cfg.NixCache); err != nil {
			return pair{}, wireReady{}, Confinement{}, fmt.Errorf("the shared Nix cache: %w", err)
		}
	}
	sandbox, spawned := childSandbox(b, std)
	p, ready, failed, err := startOnce(b, ln, configPath, std, exe, sandbox, spawned, deadline)
	if err != nil && sandbox != nil && failed {
		b.log.Printf("warning: sandbox-exec didn't start the child (%s): starting it unconfined", p.child.Exit())
		spawned = unconfined(WhySandboxFailed)
		p, ready, _, err = startOnce(b, ln, configPath, std, exe, nil, spawned, deadline)
	}
	if err != nil {
		return pair{}, wireReady{}, Confinement{}, err
	}
	confined, err := ParseConfinement(ready.Confined)
	if err == nil && !confined.fits(spawned) {
		err = errors.New("more than it was started with")
	}
	if err != nil {
		_ = p.fc.close()
		p.child.Stop(stopWait)
		return pair{}, wireReady{}, Confinement{}, broke("a ready of a confinement it can't have")
	}
	if confined.Level != ConfinedFull {
		b.log.Printf("warning: the child isn't confined to what it needs: %s", confined)
	}
	return p, ready, confined, nil
}

// startOnce starts the child, under sandbox's command when it isn't nil, and runs the start
// (handshake), telling the child how it was started (spawned). A child whose start failed is
// stopped, and returned with the error and whether its sandbox failed it (sandboxFailed), which is
// decided before the broker's own signals could end it.
func startOnce(b *broker, ln net.Listener, configPath string, std IO, exe string, sandbox []string, spawned Confinement, deadline time.Time) (pair, wireReady, bool, error) {
	p, err := spawnChild(ln, configPath, std, sandbox)
	if err != nil {
		return pair{}, wireReady{}, false, fmt.Errorf("the child: %w", err)
	}
	ready, err := b.handshake(p, exe, spawned, deadline)
	if err != nil {
		failed := sandboxFailed(err, p.child)
		_ = p.fc.close()
		p.child.Stop(stopWait)
		return p, wireReady{}, failed, err
	}
	return p, ready, false, nil
}

// sandboxFailed reports whether a start's error (err) is a child that had already ended before it
// answered (errEndedEarly) with one of sandbox-exec's own exit statuses, or killed by SIGABRT or
// SIGKILL, as the system kills a program that a profile refuses what it needs to load. A child the
// start's timer gave up on still runs, so its end is the broker's own stop, never the sandbox's.
func sandboxFailed(err error, child childProc) bool {
	if !errors.Is(err, errEndedEarly) || child == nil {
		return false
	}
	select {
	case <-child.Done():
	default:
		return false
	}
	if code := child.ExitCode(); code == sandboxExecRefused || code == sandboxExecNoRun {
		return true
	}
	sig := child.ExitSignal()
	return sig == syscall.SIGABRT || sig == syscall.SIGKILL
}

// childSandbox is the command the broker starts its child with, before the child's own arguments:
// std.Seatbelt's for what the child needs, then devvm's executable by its real path, which its
// profile names, and how that confines it. It's nil, and the child unconfined, without a sandbox,
// or when there's none to start it under (WhySandboxMissing) or making it failed
// (WhySandboxFailed), which the proxy's log warns of.
func childSandbox(b *broker, std IO) ([]string, Confinement) {
	if std.Seatbelt == nil {
		return nil, unconfined(WhyUnsupported)
	}
	needs, err := childNeeds(b.cfg, b.files)
	var cmd []string
	if err == nil {
		cmd, err = std.Seatbelt(needs)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.log.Printf("warning: no sandbox to start the child under (%v): starting it unconfined", err)
		return nil, unconfined(WhySandboxMissing)
	case err != nil:
		b.log.Printf("warning: the child's sandbox: %v: starting it unconfined", err)
		return nil, unconfined(WhySandboxFailed)
	}
	return append(cmd, needs.Exe), Confinement{Level: ConfinedFull, By: BySeatbelt}
}

// childNeeds is what the child of c needs (ChildNeeds): devvm's executable by its real path, the
// folders of its logs and the shared Nix cache, and the files it looks at (files), each absolute.
func childNeeds(c Config, files []watchedFile) (ChildNeeds, error) {
	exe, err := selfExe()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return ChildNeeds{}, fmt.Errorf("devvm's own executable: %w", err)
	}
	n := childFiles(c, files)
	n.Exe = exe
	return n, nil
}

// childFiles is what the child of c needs of the files on this machine, without its executable,
// which the child itself, already in its sandbox on macOS, can't look up: the folders of its logs
// and the shared Nix cache, and the files it looks at (files), each absolute.
func childFiles(c Config, files []watchedFile) ChildNeeds {
	var n ChildNeeds
	// A config's relative path is the broker's folder's, which the child shares, and a profile's
	// paths are absolute.
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	for _, f := range []string{c.Log, c.ProxyLog} {
		if d := abs(filepath.Dir(f)); f != "" && !slices.Contains(n.Folders, d) {
			n.Folders = append(n.Folders, d)
		}
	}
	if c.NixCache != "" {
		n.Folders = append(n.Folders, abs(c.NixCache))
	}
	for _, f := range files {
		n.Watched = append(n.Watched, abs(f.path))
	}
	return n
}
