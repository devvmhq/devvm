package lima

import (
	"context"
	"errors"
	"os"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/paths"
)

// Location is devvm's Lima home, which DEVVM_LIMA_HOME moves.
func (l *host) Location() (string, bool) {
	return l.Dirs.Lima, l.Dirs.Lima == l.Dirs.DefaultLima()
}

// Strays are the VMs in devvm's Lima home it has no state for.
func (l *housekeeping) Strays(ctx context.Context) ([]backend.Stray, error) {
	vms, err := l.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []backend.Stray
	for _, in := range vms {
		// A VM's old machine, set aside while a remake makes the new one,
		// is that VM's.
		st := l.Dirs.VM(in.Name).State()
		if h, ok := in.Name.AsideHash(); ok {
			st = paths.VM{Dir: l.Dirs.VMDir(h)}.State()
		}
		if _, err := os.Stat(st); errors.Is(err, os.ErrNotExist) {
			out = append(out, backend.Stray{Name: in.Name, Data: l.Dirs.Data})
		}
	}
	return out, nil
}
