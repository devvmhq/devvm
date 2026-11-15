package lima

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest"
	"github.com/devvmhq/devvm/internal/vmname"
)

// conformanceSubject is l, holding base's dummy image, as the
// conformance suite checks it.
func conformanceSubject(t *testing.T, l *Lima) backendtest.Subject {
	t.Helper()
	yaml, err := fs.ReadFile(l.Assets, originConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.createFromUbuntu(t.Context(), dummyBase, yaml, backend.Size{CPUs: 2, MemoryGiB: 3}); err != nil {
		t.Fatal(err)
	}
	return backendtest.Subject{Backend: l, Image: strings.TrimPrefix(string(dummyBase), vmname.ImagePrefix), Dir: filepath.Join(l.Dirs.Home, "conformance")}
}

// Lima keeps backend.Backend's contract, on the limactl stub, whose VMs
// start, and on real Lima's dummy instances, which can't: there the
// checks that need a running VM are skipped, and tests/e2e runs
// them on real VMs, through devvm.
func TestConformance(t *testing.T) {
	t.Run("stub", func(t *testing.T) {
		backendtest.Conformance(t, func(t *testing.T) backendtest.Subject {
			stubs := stubtest.New(t)
			l, _ := dummyLima(t, sandboxDirs(t), stubtest.ExecRunner{})
			// Linux's confinement, as macOS's runs limactl inside
			// sandbox-exec.
			l.OS = "linux"
			s := conformanceSubject(t, l)
			s.Boots = true
			// A start outside Start: limactl's own, on a hostagent of
			// its own, which lasts.
			s.Restart = func(ctx context.Context, vm vmname.VM) error {
				stubs.Write(t, "hostagent", strconv.Itoa(os.Getppid()))
				for _, args := range [][]string{{"stop", vm.String()}, {"start", "--tty=false", vm.String()}} {
					if err := l.Runner.Run(ctx, msg.M{}, l.limactl(ctx, args...)); err != nil {
						return err
					}
				}
				return nil
			}
			return s
		})
	})
	t.Run("lima", func(t *testing.T) {
		backendtest.Conformance(t, func(t *testing.T) backendtest.Subject {
			l, _ := realLima(t)
			return conformanceSubject(t, l)
		})
	})
}
