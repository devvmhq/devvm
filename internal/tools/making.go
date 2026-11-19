package tools

import (
	"context"
	"os"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
)

// Making is the lock that says a VM is being made: each devvm making a
// VM, or giving one new shared tools, holds it shared (Hold) from before
// it picks the tools until the VM's state lists them, and devvm gc --tool-images
// and retention's tools.unused-age take it alone (Exclude), so they never
// delete a tool image such a VM may not list yet. It's an OS lock on a
// file in devvm's data folder (Dirs.MakingLock): the OS lets go of it
// when the process holding it ends, however it ends, and the processes
// devvm starts don't inherit it.
type Making struct {
	Dirs paths.Dirs
	// Reporter shows that Hold is waiting; nil shows nothing.
	msg.Reporter
}

// Hold holds the lock shared until unlock is called or the process ends:
// any number of devvms hold it at once. It waits while devvm gc --tool-images or
// retention has it, until ctx ends, under a status line (making_waiting).
func (m Making) Hold(ctx context.Context) (unlock func(), err error) {
	if err := os.MkdirAll(m.Dirs.Data, 0o700); err != nil {
		return nil, err
	}
	var status func(msg.M) func()
	if m.Reporter != nil {
		status = m.Status
	}
	return platform.LockFileShared(ctx, m.Dirs.MakingLock(), msg.Waiting(status, msg.MakingWaiting()))
}

// Exclude takes the lock alone without waiting, so no VM starts being
// made until unlock is called. ok is false while a VM is being made.
func (m Making) Exclude() (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(m.Dirs.Data, 0o700); err != nil {
		return nil, false, err
	}
	return platform.TryLockFile(m.Dirs.MakingLock())
}
