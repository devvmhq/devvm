package vm

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// Restart stops the folder's VM, then opens it as Up does: what changes
// only when it starts (its mounts, size, ports) applies.
func (e Launch) Restart(ctx context.Context, o Options) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	if err := t.stop(ctx); err != nil {
		return err
	}
	return e.open(ctx, o, t)
}

// restartNow reports whether the user agrees to restart the VM vm, which
// is running, so that a change that applies only when it starts applies
// at once (restart_now). Where it can't ask it's no, and the caller says
// when the change applies. ask is how it asks, Confirm ([Y/n]) or Ask
// ([y/N]); count is how many changes the restart applies, and context,
// said first, what they are.
func (e *Env) restartNow(vm string, ask func(msg.Question) (bool, error), count int, context ...msg.M) bool {
	yes, err := ask(msg.RestartNow(count, vm).AnsweredBy("devvm restart").About(context...))
	return err == nil && yes
}

// restartOr restarts the folder's VM vm, which is running, as devvm
// restart does once the user agrees (restartNow), so that a setting just
// changed applies now; otherwise say(later) tells the user when it
// applies.
func (e *Env) restartOr(ctx context.Context, vm string, say func(msg.M), later msg.M) error {
	if !e.restartNow(vm, e.Confirm, 1) {
		say(later)
		return nil
	}
	return e.launcher.Restart(ctx, Options{})
}

// open opens a shell in a VM that exists, made for this folder. Flags for
// new VMs are ignored, with a warning. On the way it warns about images
// and tools newer than the VM's (--clone on a VM that mounts the folder is
// refused); then it launches the VM (launch) with its agent folder mounts
// matched to the host's, sets it up and opens the shell, as for a new VM.
func (e Launch) open(ctx context.Context, o Options, t *target) error {
	vm, st, b := t.f.Name, t.st, t.b
	if o.Clone && !isClone(st) {
		return msg.CloneNeedsNewVM(vm)
	}
	builder := e.Builder(b, st.From)
	if err := (build.Builds{Env: builder}).WarnIfOutdated(st.Chain); err != nil {
		return err
	}
	tg, err := e.toolTarget(b, st)
	if err != nil {
		return err
	}
	if err := (tools.VMs{Env: e.Tools}).WarnNewer(ctx, tools.Names(st), tools.Files(st), tg); err != nil {
		return err
	}
	if err := (tools.Versions{Env: e.Tools}).RefreshDaily(); err != nil {
		return err
	}
	e.warnIgnored(o, st)
	conf, err := e.launch(ctx, t, upOpts{agentDirs: true})
	if err != nil {
		return err
	}
	if err := e.setUpVM(ctx, t, conf); err != nil {
		return err
	}
	cmd, err := t.shell(ctx)
	if err != nil {
		return err
	}
	return e.Exec(cmd)
}

// launch takes a VM that exists, made for this folder, through what each
// launch of it does before its setup: the folder's devcontainer.json
// compared with the one it was made from, then what every start of it
// does (starter.bringUp). It returns the confinement the VM runs with.
func (e Launch) launch(ctx context.Context, t *target, o upOpts) (backend.Confinement, error) {
	e.devcontainers.check(t.st, t.f.Dir)
	return e.starter.bringUp(ctx, t, o)
}

// warnIgnored warns about the flags only new VMs take, given to st's VM,
// which exists: once for each group of them (ignoredFlags), with the
// command that changes the same on it.
func (e Launch) warnIgnored(o Options, st *state.VM) {
	vm := st.Name()
	for _, g := range ignoredFlags(o, st.From) {
		flags := strings.Join(g.flags, ", ")
		if g.command == "" {
			e.Warn(msg.NewVMFlagsIgnored(flags, vm))
		} else {
			e.Warn(msg.NewVMFlagsIgnoredRun(g.command, flags, vm))
		}
	}
}

// flagGroup is flags given that only a new VM takes, and the command that
// changes the same on a VM that exists, "" for none.
type flagGroup struct {
	flags   []string
	command string
}

// ignoredFlags groups the flags in o that only a new VM takes, by the
// command that changes the same on a VM that exists; from is what the VM
// was made from, which --from naming again changes nothing.
func ignoredFlags(o Options, from string) []flagGroup {
	var gs []flagGroup
	// add adds command's group: those of flags given, each "" when not.
	add := func(command string, flags ...string) {
		if g := (flagGroup{slices.DeleteFunc(flags, func(f string) bool { return f == "" }), command}); len(g.flags) > 0 {
			gs = append(gs, g)
		}
	}
	add("", flagIf(o.From != "" && o.From != from, "--from"), flagIf(o.NoDevcontainer, "--no-devcontainer"))
	add("devvm tools", flagIf(o.Toolchains != nil, "--tools"), flagIf(o.Agents != nil, "--agents"))
	add("devvm layer add", flagIf(o.Layers != nil, "--layers"))
	add("devvm network "+string(o.Network), flagIf(o.Network != "", "--network"))
	add("devvm git-protect off", flagIf(o.NoGitProtect, "--no-git-protect"))
	add("devvm size", flagIf(o.Size.CPUs != "", "--cpus"), flagIf(o.Size.Memory != "", "--memory"), flagIf(o.Size.Disk != "", "--disk"))
	add("devvm ports LIST", flagIf(o.Ports != "", "--ports"))
	add("devvm ssh-agent on", flagIf(o.SSHAgent, "--ssh-agent"))
	return gs
}

// flagIf is flag when given, else "".
func flagIf(given bool, flag string) string {
	if given {
		return flag
	}
	return ""
}

// toolTarget is what a VM's tool images are built for: its base's C
// library, with a --from gnu base's glibc version, for musl the digest
// its --from image has as last recorded, and its backend's format.
func (e *Env) toolTarget(b backend.Backend, st *state.VM) (tools.Target, error) {
	t, _ := tools.Of(st)
	tg := tools.Target{Libc: t.Made.Libc, Glibc: t.Made.Glibc, Format: b.ToolFormat()}
	if tg.Libc == "musl" {
		digest, err := from.Recorded(e.Dirs, st.From)
		if err != nil {
			return tools.Target{}, err
		}
		tg.Base = digest
	}
	return tg, nil
}

// buildTarget is toolTarget for building the VM's tools with builder: a
// musl VM's base is built first, its digest looked up, as its tool images
// build in that base as it is now and their keys hash its digest.
func (e Launch) buildTarget(ctx context.Context, builder *build.Env, b backend.Backend, st *state.VM) (tools.Target, error) {
	if t, _ := tools.Of(st); t.Made.Libc == "musl" {
		if err := (build.Builds{Env: builder}).Build(ctx, image.Base); err != nil {
			return tools.Target{}, err
		}
	}
	return e.toolTarget(b, st)
}

// buildTools builds the tool images the share lacks of files, then checks
// that each runs on tg's base (tools.Share.Runs), refusing first a tool
// that has no build for its C library (tools.HasBuilds). They build in a base as
// it is now, built first if it isn't: for gnu, devvm's own Ubuntu base on
// builder's backend, whatever builder's base starts from, as any VM may
// mount them, and for musl builder's own, as their keys hash its digest.
func (e Launch) buildTools(ctx context.Context, builder *build.Env, tg tools.Target, files []string) error {
	if err := tools.HasBuilds(files, tg); err != nil {
		return err
	}
	share := tools.Share{Env: e.Tools}
	if slices.ContainsFunc(files, func(f string) bool { return !share.Built(f) }) {
		if tg.Libc != "musl" && builder.From != "" {
			own, err := e.ownBase(builder.Backend)
			if err != nil {
				return err
			}
			builder = own
		}
		if err := (build.Builds{Env: builder}).Build(ctx, image.Base); err != nil {
			return err
		}
		base, err := builder.Key(image.Base)
		if err != nil {
			return err
		}
		if err := share.Build(ctx, builder.Review, builder.Backend, base, tg, files); err != nil {
			return err
		}
	}
	return share.Runs(files, tg)
}

// ownBase is what builds devvm's own Ubuntu base on b, which a --from
// VM's gnu tool images build in. When it isn't built yet, it says that
// it builds it first (tools_need_own_base), before the build says it
// builds base.
func (e Launch) ownBase(b backend.Backend) (*build.Env, error) {
	own := e.Builder(b, "")
	key, err := own.Key(image.Base)
	if err != nil {
		return nil, err
	}
	if _, err := state.ReadImage(e.Dirs, key); errors.Is(err, fs.ErrNotExist) {
		e.Info(msg.ToolsNeedOwnBase())
	}
	return own, nil
}
