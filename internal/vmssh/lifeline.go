package vmssh

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
)

// recheck is how often a forwarder gives up the ports devvm reserves now,
// whatever its VM reports (Forwarder.Recheck): as often as ports.sh
// reports when nothing changes.
const recheck = 10 * time.Second

// Main runs a VM's forwarder, as devvm _ports (Cmd) with its arguments
// (Forwards.Start's): ports.sh is script, d devvm's folders, where it
// reads the ports it never takes (Forwards.Reserved) and records those
// it forwards (Forwards.record), and its lines go to log. It runs while
// the VM's lifeline holds (daemon.Lifeline), which is the backend's: the
// pid file of the host process that runs the VM.
func Main(ctx context.Context, args []string, script []byte, d paths.Dirs, log io.Writer) error {
	fs := flag.NewFlagSet(Cmd, flag.ContinueOnError)
	fs.SetOutput(log)
	var fl Flags
	fl.Register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fl.lifeline == "" {
		return errors.New("-lifeline is needed")
	}
	fw := Forwards{Dirs: d, Args: platform.ProcessArgs}
	f, err := fl.Forwarder(script, fw.Reserved, log)
	if err != nil {
		return err
	}
	f.Note = func(now []int, failed failedPorts) {
		if err := fw.record(fl.VM, os.Getpid(), now, failed); err != nil {
			f.logf("its ports not recorded: %v", err)
		}
	}
	// The VM runs as it starts the forwarder: no wait.
	life, ok := daemon.FollowLifeline(fl.lifeline, 0, f.Tick)
	if !ok {
		return errors.New(fl.VM + ": no lifeline at " + fl.lifeline)
	}
	f.Alive = func(context.Context) bool { return life.Holds() }
	f.Every, f.Recheck = f.Tick, recheck
	return f.Run(ctx)
}
