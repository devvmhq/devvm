package state

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/vmname"
)

// The state's locks in their order: a VM's lock, then the claims lock,
// passes; a second VM's lock, a VM's lock under the claims lock, and a
// lock taken again panic, naming the lock, before waiting for it. Each
// goroutine's locks are its own, and none are left held.
func TestLockOrder(t *testing.T) {
	d := testDirs(t)
	a := create(t, d, mustNew(t, d, "devvm-a-00000000000001"), nil)
	b := create(t, d, mustNew(t, d, "devvm-b-00000000000002"), nil)
	ctx := t.Context()
	noop := func(*VM) error { return nil }
	for _, c := range []struct {
		name, panics string
		run          func() error
	}{
		{"a VM's lock, then the claims lock", "", func() error {
			return a.Update(ctx, func(*VM) error { return Claims(ctx, d, func() error { return nil }) })
		}},
		{"a claim", "", func() error { return a.Claim(ctx, noop) }},
		{"another VM's lock", "one VM's lock at a time", func() error {
			return a.Update(ctx, func(*VM) error { return b.Update(ctx, noop) })
		}},
		{"a VM's lock under the claims lock", "holding the claims lock", func() error {
			return Claims(ctx, d, func() error { return a.Update(ctx, noop) })
		}},
		{"a removal under the claims lock", "holding the claims lock", func() error {
			return Claims(ctx, d, func() error { return RemoveFolder(ctx, d, b.Name()) })
		}},
		{"the claims lock again", "taken again", func() error {
			return a.Claim(ctx, func(*VM) error { return Claims(ctx, d, func() error { return nil }) })
		}},
		{"the same VM's lock again", "taken again", func() error {
			return a.Update(ctx, func(*VM) error { return a.Remove(ctx) })
		}},
	} {
		got := func() (p string) {
			defer func() {
				if r := recover(); r != nil {
					p = fmt.Sprint(r)
				}
			}()
			if err := c.run(); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			return ""
		}()
		if c.panics == "" && got != "" || !strings.Contains(got, c.panics) {
			t.Errorf("%s: panicked %q, want %q", c.name, got, c.panics)
		}
	}
	// Two goroutines each hold a VM's lock at once: each its own.
	var wg sync.WaitGroup
	in := make(chan struct{})
	for _, v := range []*VM{a, b} {
		wg.Go(func() {
			if err := v.Update(ctx, func(*VM) error { <-in; return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	close(in)
	wg.Wait()
	held.mu.Lock()
	defer held.mu.Unlock()
	if len(held.held) != 0 {
		t.Errorf("locks left held: %v", held.held)
	}
}

// mustNew is New's VM named name.
func mustNew(t *testing.T, d paths.Dirs, name vmname.VM) *VM {
	t.Helper()
	v, err := New(d, name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
