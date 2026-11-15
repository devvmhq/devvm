package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// finderNeeds are what the commands on the folder's VM need of it, by
// name.
func finderNeeds() map[string]need {
	return map[string]need{
		"any":    {},
		"own":    {own: true},
		"vm":     {has: hasVM},
		"made":   {has: hasMade},
		"ports":  {has: hasMade, noVM: msg.NoVMPorts},
		"policy": {has: hasMachine, folder: true, noVM: msg.NoVMPolicy},
	}
}

// The finder finds the VM each need asks for, in each folder: none, its
// own, a parent folder's, a parent's past a repo of its own, one made for
// an earlier folder at its path, one whose creation didn't finish, and
// one whose remake didn't, with its machine and without.
func TestFinder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *Env, st *state.VM) // st: web's VM, nil for none
		lost  bool                                     // its backend no longer has web's VM
		in    string                                   // the folder devvm runs in, below web
		want  map[string]string                        // by need: what it finds, or its error's names
	}{
		{
			name: "none",
			want: map[string]string{
				"any": "new $web", "own": "new $web", "vm": "no_vm dir=$web", "made": "no_vm dir=$web",
				"ports": "no_vm_ports dir=$web", "policy": "no_vm_policy dir=$web",
			},
		},
		{
			name:  "own",
			setup: func(*testing.T, *Env, *state.VM) {},
			want: map[string]string{
				"any": "VM $web", "own": "VM $web", "vm": "VM $web", "made": "VM $web", "ports": "VM $web", "policy": "VM $web",
			},
		},
		{
			name:  "parent",
			setup: func(*testing.T, *Env, *state.VM) {},
			in:    "src",
			want: map[string]string{
				"any": "VM $web from $src", "own": "new $src", "vm": "VM $web from $src", "made": "VM $web from $src",
				"ports": "VM $web from $src", "policy": "VM $web from $src",
			},
		},
		{
			name: "nested repo",
			setup: func(t *testing.T, e *Env, _ *state.VM) {
				if err := os.MkdirAll(filepath.Join(e.Dir, "src", ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			in: "src",
			want: map[string]string{
				"any": "new $src", "own": "new $src", "vm": "no_vm dir=$src", "made": "no_vm dir=$src",
				"ports": "no_vm_ports dir=$src", "policy": "no_vm_policy dir=$src",
			},
		},
		{
			name:  "other folder",
			setup: func(t *testing.T, _ *Env, st *state.VM) { _ = set(t, st, keyFolder, "1:2") },
			want: map[string]string{
				"any": "VM $web", "own": "VM $web", "vm": "VM $web", "made": "vm_other_folder vm=$vm was=$web",
				"ports": "vm_other_folder vm=$vm was=$web", "policy": "vm_other_folder vm=$vm was=$web",
			},
		},
		{
			name: "unfinished",
			setup: func(t *testing.T, _ *Env, st *state.VM) {
				change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
			},
			want: map[string]string{
				"any": "VM $web", "own": "VM $web", "vm": "VM $web", "made": "vm_unfinished vm=$vm",
				"ports": "vm_unfinished vm=$vm", "policy": "VM $web",
			},
		},
		{
			name: "remake pending",
			setup: func(t *testing.T, _ *Env, st *state.VM) {
				pending(t, st)
			},
			want: map[string]string{
				"any": "VM $web", "own": "VM $web", "vm": "VM $web", "made": "remake_pending dir=$web vm=$vm",
				"ports": "remake_pending dir=$web vm=$vm", "policy": "VM $web",
			},
		},
		{
			name:  "remake pending without its machine",
			setup: func(t *testing.T, _ *Env, st *state.VM) { pending(t, st) },
			lost:  true,
			want: map[string]string{
				"any": "lost $web", "own": "lost $web", "vm": "lost $web", "made": "remake_pending dir=$web vm=$vm",
				"ports": "remake_pending dir=$web vm=$vm", "policy": "no_vm_policy dir=$web",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			web, vm := e.Dir, vmname.VM("")
			if tc.setup != nil {
				st := existingVM(t, e, Options{Network: "open"})
				vm = st.Name()
				tc.setup(t, e, st)
			}
			if tc.lost {
				delete(f.Instances, vm)
			}
			src := filepath.Join(web, "src")
			if err := os.MkdirAll(src, 0o700); err != nil {
				t.Fatal(err)
			}
			e.Dir = filepath.Join(web, tc.in)
			r := strings.NewReplacer("$web", web, "$src", src, "$vm", vm.String())
			for n, nd := range finderNeeds() {
				got, err := (finder{e}).target(t.Context(), nd)
				if want := r.Replace(tc.want[n]); foundAs(got, err) != want {
					t.Errorf("%s: %q, want %q", n, foundAs(got, err), want)
				}
			}
		})
	}
}

// pending makes st's VM one whose remake didn't finish.
func pending(t *testing.T, st *state.VM) {
	t.Helper()
	err := update(t.Context(), st, remakeSection, func(r *remakeInVM) error {
		*r = remakeInVM{Image: "docker", Command: "devvm layer add docker"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// foundAs is what a finder found, for TestFinder: its error's names, or
// the VM's folder (VM: its backend has it; lost: only its state; new:
// neither) and the folder it's found from, when that's below it.
func foundAs(t *target, err error) string {
	if err != nil {
		return msg.NamesOf(err)
	}
	s := "VM " + t.f.Dir
	switch {
	case !t.exists && t.st.Exists():
		s = "lost " + t.f.Dir
	case !t.exists:
		s = "new " + t.f.Dir
	}
	if t.sub != "" {
		s += " from " + t.sub
	}
	return s
}
