package lima

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

// ToolsVM is the throwaway VM shared tools are built in.
const ToolsVM vmname.VM = "devvm-tools-build"

// BuildTools clones base's instance into ToolsVM, with spec.Out its only
// mount, starts it confined as spec.Net asks, runs the script with its
// arguments, and deletes it. A ToolsVM left by an earlier run is deleted
// first, and the VM goes however the run ends (a cleanup on the ui stack).
// The registry's guard has checked spec (ToolsSpec.Validate).
func (l *images) BuildTools(ctx context.Context, spec backend.ToolsSpec) error {
	base, err := vmname.Image(spec.Base)
	if err != nil {
		return msg.ImageMissing(spec.Base)
	}
	in, err := l.inspect(ctx, base)
	if errors.Is(err, backend.ErrNotFound) {
		return msg.ImageMissing(spec.Base)
	} else if err != nil {
		return err
	}
	if backend.Status(in.Status) != backend.StatusStopped {
		if err := l.stop(ctx, base); err != nil {
			return err
		}
	}
	if err := l.deleteIfThere(ctx, ToolsVM); err != nil {
		return err
	}
	mounts, err := mountsSet([]mount{outMount(spec.Out, spec.OutAt)})
	if err != nil {
		return err
	}
	// A failure leaves it to the cleanup, which runs once the caller has
	// shown the run log's end.
	done := l.AddCleanup(func(ctx context.Context) { _ = l.deleteIfThere(ctx, ToolsVM) })
	// Its steps: clone, configure (its new ssh port) and start, the script
	// (the user's CAs before it), delete.
	steps := 5
	if !spec.Trust.None() {
		steps++
	}
	// Each step names the tool it builds, not ToolsVM, which every
	// tool's build shares.
	ctx = withShown(l.WithSteps(ctx, steps), ToolsVM, msg.ToolsBuildVM(spec.Name).Text())
	if err := l.Runner.Run(ctx, msg.CloningForTools(spec.Name), l.limactl(ctx, toolsArgs(base, mounts)...)); err != nil {
		return backend.Logged(msg.VMCreateFailed(ToolsVM))
	}
	// Behind a company proxy, through a proxy of devvm's (buildproxy.go).
	conf, hp, proxyDone, err := buildProxy(ctx, l.Lima, ToolsVM, spec.Net)
	if err != nil {
		return err
	}
	if err := l.start(ctx, ToolsVM, nil, conf); err != nil {
		return err
	}
	if err := l.runScript(ctx, ToolsVM, spec.Trust.Add, true); err != nil {
		return err
	}
	cmd := l.limactl(ctx, append([]string{"shell", "--workdir", "/", ToolsVM.String(), "bash", "-s", "--"}, spec.Args...)...)
	// The CAs' variables and the build's proxy for its commands, as each
	// image build's step has them, never written into the VM.
	cmd.Stdin = bytes.NewReader(slices.Concat(spec.Trust.Env, withProxy(hp, spec.Script).Data))
	if err := l.Runner.Run(ctx, msg.BuildingTool(spec.Name), cmd); err != nil {
		return backend.Logged(msg.ToolsBuildFailed())
	}
	if err := l.delete(ctx, ToolsVM); err != nil {
		return err
	}
	proxyDone(true)
	done()
	return nil
}

// toolsArgs are limactl clone's arguments for ToolsVM: a clone of base's
// instance with mounts (mountsSet's) and nothing else from the host.
func toolsArgs(base vmname.VM, mounts string) []string {
	return []string{"clone", "--tty=false", "--mount-none", "--set", mounts, base.String(), ToolsVM.String()}
}

// ToolFormat is build.sh's own: Lima's guests mount compressed EROFS.
func (l *images) ToolFormat() string { return "" }

type shownKey struct{}

// shownName is what a phase's steps call one VM.
type shownName struct {
	vm vmname.VM
	as string
}

// withShown has the steps on vm under ctx name it as as: a tool's build
// VM by its tool.
func withShown(ctx context.Context, vm vmname.VM, as string) context.Context {
	return context.WithValue(ctx, shownKey{}, shownName{vm, as})
}

// shown is the name the steps on vm under ctx give it: withShown's, else
// its own.
func shown(ctx context.Context, vm vmname.VM) string {
	if s, ok := ctx.Value(shownKey{}).(shownName); ok && s.vm == vm {
		return s.as
	}
	return vm.String()
}

// deleteIfThere deletes a VM if devvm's Lima home has it.
func (l *images) deleteIfThere(ctx context.Context, vm vmname.VM) error {
	if _, err := l.inspect(ctx, vm); errors.Is(err, backend.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return l.delete(ctx, vm)
}
