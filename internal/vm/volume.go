package vm

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/platform"
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
// process (Linux), else Env.Volumes', within wait, asked once a run for each mount; "" when it
// can't be told, which matches nothing and records nothing.
func (e *Env) volumeOf(ctx context.Context, s platform.FolderStat, wait time.Duration) string {
	if s.ReadOnly || !s.Unique || e.Volumes == nil || s.Volume != "" && !e.VolumesOnly {
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
	v, err := e.Volumes(ctx, s)
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

// recordedVolume is what a state records of the volume of the folder at dir, whose identity is
// id (platform.VolumeOf): the volume of the folder opened there with no link on its path, when
// that's the folder id names, its identity its alone; "" otherwise.
func (e *Env) recordedVolume(ctx context.Context, dir, id string, wait time.Duration) string {
	s, err := platform.StatFolder(dir)
	if err != nil || !platform.SameFolder(dir, id, dir, s.ID) {
		return "" // no volume a launch could vouch for: none recorded, and today's rule applies
	}
	return platform.VolumeOf(e.volumeOf(ctx, s, wait), s.ID)
}
