package lima

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vmname"
)

// downloadCache is Lima's own download cache, in the user's cache folder,
// outside its homes: every Lima home of the user's shares it, devvm's and
// their own. An error when there's no cache folder to look in.
func (l *housekeeping) downloadCache() (string, error) {
	if l.Dirs.UserCache == "" {
		return "", errors.New("lima: no cache folder")
	}
	return filepath.Join(l.Dirs.UserCache, "lima", "download", "by-url-sha256"), nil
}

// Usage is the disk each VM's folder in devvm's Lima home takes (a clone
// shares its image's blocks on macOS, which du counts for both), and
// Lima's download cache, by entry.
func (l *housekeeping) Usage(ctx context.Context) (backend.Usage, error) {
	u := backend.Usage{VMs: map[vmname.VM]int64{}}
	vms, err := l.List(ctx)
	if err != nil {
		return u, err
	}
	for _, in := range vms {
		if n, err := platform.DiskUsage(l.Dirs.LimaVM(in.Name)); err == nil {
			u.VMs[in.Name] = n
		}
	}
	if c, ok := l.cacheUsage(); ok {
		u.Caches = append(u.Caches, c)
	}
	return u, nil
}

// cacheUsage is Lima's download cache, by entry; ok is false when there's
// none to read.
func (l *housekeeping) cacheUsage() (backend.Cache, bool) {
	dir, err := l.downloadCache()
	if err != nil {
		return backend.Cache{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return backend.Cache{}, false
	}
	c := backend.Cache{Name: "download cache", Path: filepath.Dir(filepath.Dir(dir)), Count: len(entries)}
	if n, err := platform.DiskUsage(c.Path); err == nil {
		c.Size, c.Sized = n, true
	}
	return c, true
}
