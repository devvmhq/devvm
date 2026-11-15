package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// RunInNamespace runs exe with args in a user and mount namespace of its
// own (InUserNamespace), its input and output devvm's, passing on the
// signals devvm gets, and returns once it ends, with its exit status.
// Killed, devvm takes it with it.
func RunInNamespace(exe string, args []string) error {
	cmd := exec.Command(exe, args...) //nolint:noctx,gosec // devvm's own, for as long as it runs
	if err := InUserNamespace(cmd); err != nil {
		return err
	}
	return runChild(cmd)
}

// runChild runs cmd with devvm's input and output, passing on the
// signals devvm gets, and returns once it ends. It's killed if this
// thread ends (its Pdeathsig), so the thread stays.
func runChild(cmd *exec.Cmd) error {
	runtime.LockOSThread()
	// The program's own input and output, as devvm got them: never a message.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr //nolint:forbidigo // passed on whole, as devvm got them
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	passed := make(chan struct{})
	defer func() {
		signal.Stop(sigs)
		// Once Stop returns, no signal reaches sigs, so closing it ends
		// the goroutine that passes them on, which runChild waits for.
		close(sigs)
		<-passed
	}()
	if err := cmd.Start(); err != nil {
		close(passed)
		return err
	}
	go func() {
		defer close(passed)
		for s := range sigs {
			_ = cmd.Process.Signal(s) // devvm's own child, by its handle until Wait reaps it
		}
	}()
	return cmd.Wait()
}

// PinAll makes the mounts of the namespace it runs in (RunInNamespace's)
// private and pins each of pins beneath the folder root, in order, then
// checks each is pinned. It stops at the first that fails.
func PinAll(root string, pins []Pin) error {
	r, err := OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := PrivateMounts(); err != nil {
		return err
	}
	for _, p := range pins {
		if err := PinBeneath(r, p.Path, p.ReadOnly); err != nil {
			return err
		}
		if !Pinned(r, p.Path, p.ReadOnly) {
			return &os.PathError{Op: "pin", Path: p.Path, Err: errors.New("not pinned once bound")}
		}
	}
	return nil
}

// RunPinned runs in a namespace of its own (RunInNamespace): it pins pins
// beneath root (PinAll), watches their folders, and runs argv, which
// drops every capability and becomes QEMU (ExecUnprivileged), as its
// child, killed if this process ends. Until argv ends, it keeps the pins
// (keep), each change told to report, and it returns argv's exit. A pin,
// or a watch, that fails stops it before argv runs, so argv never runs
// unpinned, and never outlives what keeps its pins.
func RunPinned(root string, pins []Pin, argv []string, every time.Duration, report func(path, what string)) error {
	r, err := OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	// Watched first, so a change as it pins is seen.
	changed, unwatch, err := WatchFolders(pinFolders(root, pins))
	if err != nil {
		return err
	}
	defer unwatch()
	if err := PinAll(root, pins); err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx,gosec // devvm itself, which becomes QEMU
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	done := make(chan error, 1)
	go func() { done <- runChild(cmd) }()
	return keep(r, pins, changed, done, every, report)
}

// pinFolders are the folders RunPinned watches: each pin's folder, and
// each fixed pin itself.
func pinFolders(root string, pins []Pin) []string {
	dirs := map[string]bool{}
	for _, p := range pins {
		dirs[filepath.Dir(filepath.Join(root, p.Path))] = true
		if !p.ReadOnly {
			dirs[filepath.Join(root, p.Path)] = true
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// keep pins again each of pins, beneath r, that lost its pin, as soon as
// one of their folders changes, and every every, until done gives the
// child's end, which it returns. It tells report each pin made again
// (PinAgain), and once, until it's pinned again, each it can't make again
// (PinLinked for another name, else PinLost).
func keep(r *os.File, pins []Pin, changed <-chan struct{}, done <-chan error, every time.Duration, report func(path, what string)) error {
	tick := time.NewTicker(every)
	defer tick.Stop()
	lost := map[string]bool{}
	for {
		select {
		case err := <-done:
			return err
		case <-changed:
		case <-tick.C:
		}
		for _, p := range pins {
			if Pinned(r, p.Path, p.ReadOnly) {
				continue
			}
			// Gone, a symlink now, or another name: nothing to pin
			// until it's back.
			switch err := PinBeneath(r, p.Path, p.ReadOnly); {
			case err == nil:
				lost[p.Path] = false
				report(p.Path, PinAgain)
			case lost[p.Path]:
			case errors.Is(err, ErrLinked):
				lost[p.Path] = true
				report(p.Path, PinLinked)
			default:
				lost[p.Path] = true
				report(p.Path, PinLost)
			}
		}
	}
}

// Probe reports whether this host pins paths (nil), or why not: in a
// folder of its own holding a file f, it runs the command line argv gives
// for that folder in a namespace (it runs ProbePin there), for timeout at
// most, and returns the last line it printed when it fails.
func Probe(argv func(dir string) []string, timeout time.Duration) error {
	dir, err := os.MkdirTemp("", "devvm-pins-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("devvm\n"), 0o600); err != nil { //nolint:forbidigo // the probe's own new file, in its own folder
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout) //nolint:forbidigo // its callers have no context (a backend's Caps): bounded by its own timeout
	defer cancel()
	// It's killed if this thread ends.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	a := argv(dir)
	cmd := exec.CommandContext(ctx, a[0], a[1:]...) //nolint:gosec // devvm itself
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := InUserNamespace(cmd); err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(out.String()); s != "" {
			return errors.New(s[strings.LastIndexByte(s, '\n')+1:])
		}
		return err
	}
	return nil
}

// ProbePin runs in a namespace of its own: it pins dir/f read-only and checks
// that writing it fails as a read-only file system's does.
func ProbePin(dir string) error {
	if err := PinAll(dir, []Pin{{Path: "f", ReadOnly: true}}); err != nil {
		return err
	}
	return readOnlyFS(filepath.Join(dir, "f"))
}

// readOnlyFS reports whether opening path to write fails as a read-only
// file system's does (nil), or how it doesn't.
func readOnlyFS(path string) error {
	w, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // the probe's own file, in its own folder
	if err == nil {
		_ = w.Close()
		return errors.New("a pinned file is writable")
	}
	if !errors.Is(err, syscall.EROFS) {
		return err
	}
	return nil
}
