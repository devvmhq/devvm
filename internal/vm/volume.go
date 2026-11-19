package vm

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// How long a lookup of a folder's volume waits (Env.Volumes, macOS's diskutil): a launch's, for
// a VM whose state records no volume yet, briefly, as it records one at a later launch when this
// one can't; an adopt's or a move's longer, as it records the folder anew.
const (
	volumeWaitLaunch = time.Second
	volumeWaitMove   = 5 * time.Second
)

// volumeCache is a run's answers of Env.Volumes, by mount point and device, and whether one
// failed, after which the run asks no more, so a diskutil that hangs costs one wait a run.
type volumeCache struct {
	mu     sync.Mutex
	got    map[string]string
	failed bool
}

// volumeOf is the volume of the folder s describes: s.Volume where the system gives it in the
// process (Linux), else Env.Volumes', within wait, asked once a run for each mount, under a status
// line; "" when it can't be told, which matches nothing and records nothing.
func (e *Env) volumeOf(ctx context.Context, s platform.FolderStat, wait time.Duration) string {
	if volumeFree(e, s) {
		return s.Volume
	}
	c := e.volumes
	if c == nil {
		c = new(volumeCache)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := s.Mount + "\x00" + s.Device
	if v, ok := c.got[key]; ok {
		return v
	}
	if c.failed {
		return "" // a lookup failed earlier in this run, so this one isn't tried
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	stop := e.Status(msg.LookingUpVolume(s.Mount))
	v, err := e.Volumes(ctx, s)
	stop()
	if err != nil {
		c.failed = !errors.Is(err, errors.ErrUnsupported)
		return "" // no volume: the folder's record keeps none, and a later launch asks again
	}
	if c.got == nil {
		c.got = map[string]string{}
	}
	c.got[key] = v
	return v
}

// volumeFree reports whether volumeOf tells s's volume without asking e's Volumes, so it costs
// nothing: on Linux, where the system gives it in the process, and wherever there's none to ask
// for (read-only, not unique, no Volumes). It's a function, not a method, as Env has the most
// methods the structure test allows.
func volumeFree(e *Env, s platform.FolderStat) bool {
	return s.ReadOnly || !s.Unique || e.Volumes == nil || s.Volume != "" && !e.VolumesOnly
}

// keepsVolume reports whether the folder s describes, which SameOpenFolder took for st's
// folder, is on the volume st records for it (platform.KeepsVolume), which a renumbering alone
// can't tell from a clone of the disk mounted at the folder's path. It looks the volume up when
// the folder's disk was numbered anew since it was recorded, when the record's volume wasn't
// checked since a renumbering (state.VM.FolderUnchecked), and at every launch where it costs
// nothing (volumeFree: Linux). It's true when the state records no volume, or the volume can't
// be told.
func (e *Env) keepsVolume(ctx context.Context, st *state.VM, s platform.FolderStat) bool {
	if platform.RecordedVolume(st.Folder(), st.FolderVolume()) == "" {
		return true
	}
	if platform.RecordedAs(st.Folder(), s) && !st.FolderUnchecked() && !volumeFree(e, s) {
		return true
	}
	return platform.KeepsVolume(st.Folder(), st.FolderVolume(), e.volumeOf(ctx, s, volumeWaitMove))
}

// recordedVolume is what a state records of the volume of the folder at dir, whose identity is
// id (platform.VolumeOf): the volume of the folder opened there with no link on its path, when
// that's the folder id names, its identity its alone; "" otherwise.
func (e *Env) recordedVolume(ctx context.Context, dir, id string, wait time.Duration) string {
	s, err := platform.StatFolder(dir)
	if err != nil || !platform.RecordedAs(id, s) {
		return "" // no volume a launch could vouch for: none recorded, and today's rule applies
	}
	return platform.VolumeOf(e.volumeOf(ctx, s, wait), s.ID)
}
