package vm

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
)

// A start of a VM with network policy registers a cleanup that, when the
// run is interrupted while its backend still waits for the VM, stops the
// VM and its proxy and says so. Not interrupted, it does nothing; a VM
// without network policy gets none.
func TestStartInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name             string
		net, interrupted bool
		cleanups         int
		stopped          bool
	}{
		{"interrupted", true, true, 1, true},
		{"not interrupted", true, false, 1, false},
		{"no network policy", false, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
			f.ExecFunc = noDirect
			st := existingVM(t, e, Options{Network: netOf(tc.net)})
			vm := st.Name()
			if err := f.Stop(t.Context(), vm); err != nil {
				t.Fatal(err)
			}
			var cleanups []func(context.Context)
			fake(e).OnCleanup = func(c func(context.Context)) func() {
				cleanups = append(cleanups, c)
				return func() {}
			}
			fake(e).Interrupt = tc.interrupted
			var said []string
			fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
			// The backend's start, cut short.
			f.Errs = map[string]error{"Start": context.Canceled}
			conf := backend.Confinement{Net: backend.NetDirect}
			if tc.net {
				conf.Net = backend.NetProxy
			}
			if _, err := e.starter.start(t.Context(), e.newTarget(project.Folder{Dir: st.Dir, Name: vm}, st, f), conf); !errors.Is(err, context.Canceled) {
				t.Fatalf("start: %v", err)
			}
			if len(cleanups) != tc.cleanups {
				t.Fatalf("cleanups = %d, want %d", len(cleanups), tc.cleanups)
			}
			// As ui.Main runs them: while the start still waits.
			before := len(f.Calls())
			for _, c := range slices.Backward(cleanups) {
				c(t.Context())
			}
			stopped := slices.Contains(f.Methods()[before:], "Stop")
			if stopped != tc.stopped {
				t.Errorf("stopped = %v, want %v", stopped, tc.stopped)
			}
			if _, runs := e.Proxy.PID(vm); runs {
				t.Error("its proxy still runs")
			}
			wantSaid := []string(nil)
			if tc.stopped {
				wantSaid = []string{"interrupted_start vm=" + vm.String()}
			}
			if !slices.Equal(said, wantSaid) {
				t.Errorf("said %q, want %q", said, wantSaid)
			}
		})
	}
}
