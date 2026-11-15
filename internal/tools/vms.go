package tools

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Section is the name of the shared tools section in a VM's state.
const Section = "tools"

// InVM is a VM's shared tools section: its shared tools by name, as
// asked, in their order (image.SortTools), and the tool images devvm gave
// it.
type InVM struct {
	Names []string `json:"names,omitempty"`
	Made  Made     `json:"made,omitzero"`
}

// Made is the tool images devvm gave a VM: their files in the share, and
// the C library its base had, which they're built for, with the glibc
// version of a --from gnu base (Target's Glibc).
type Made struct {
	Files []string `json:"files,omitempty"`
	Libc  string   `json:"libc,omitempty"`
	Glibc string   `json:"glibc,omitempty"`
}

// Of returns a VM's shared tools section.
func Of(st *state.VM) (InVM, error) { return state.Section[InVM](st, Section) }

// Set sets a VM's shared tools section, in st alone: inside st.Update.
func Set(st *state.VM, t InVM) error { return state.SetSection(st, Section, t) }

// Names returns a VM's shared tools by name: node claude. A section that
// can't be read (the folder's VM's is checked as it's found) has none.
func Names(st *state.VM) []string {
	t, _ := Of(st)
	return t.Names
}

// Files returns a VM's tool images, as Names does.
func Files(st *state.VM) []string {
	t, _ := Of(st)
	return t.Made.Files
}

// VMTool is one of a VM's shared tools, as devvm tools lists it.
type VMTool struct {
	Tool    string // its name in the lock: node, pnpm
	Version string // the VM's version of it
	// Newer is the lock's version when the VM has another build, else "";
	// Built is whether the share has that one.
	Newer string
	Built bool
}

// Show returns a VM's tools, from its tool files, in their order. The
// lock is read as it is, never resolved: without one, none is newer.
func (t VMs) Show(files []string, tg Target) ([]VMTool, error) {
	lock, err := Versions(t).ReadLock()
	if err != nil {
		return nil, err
	}
	share := Share(t)
	var out []VMTool
	for _, f := range files {
		tool, v, ok := image.ParseToolFile(f)
		if !ok {
			continue
		}
		vt := VMTool{Tool: tool, Version: v}
		// Another file is another build: a newer version, or the same
		// version from a changed lock entry or build script, as WarnNewer
		// tells it.
		if lv := lock.Version(tool); lv != "" {
			if cur, err := t.File(lock, tool, tg); err == nil && cur != f {
				vt.Newer, vt.Built = lv, share.Built(cur)
			}
		}
		out = append(out, vt)
	}
	return out, nil
}

// Remove takes shared tools out of a VM with these shared tools (names)
// and tool files, made from img: it returns the VM's shared tools and
// tool files without them. What the VM installed itself stays. A layer
// ran its script into the VM's disk, so it can't be removed, and neither
// can a shared tool the VM lacks.
func Remove(vm vmname.VM, img image.Name, names, files, gone []string) ([]string, []string, error) {
	var drop []string
	for _, n := range gone {
		switch {
		case img.Has(n):
			return nil, nil, msg.CustomLayerRemove(n, vm)
		case !slices.Contains(names, n):
			return nil, nil, msg.VMLacks(listOrNone(names), n, vm)
		}
		drop = append(drop, image.GroupTools(n)...)
	}
	var rest []string
	for _, h := range names {
		if !slices.Contains(gone, h) {
			rest = append(rest, h)
		}
	}
	var kept []string
	tools, _ := split(files)
	for i, f := range files {
		if !slices.Contains(drop, tools[i]) {
			kept = append(kept, f)
		}
	}
	return rest, kept, nil
}

// listOrNone is names as a message shows a list of them: space-separated,
// or none.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// Newer returns the shared tools names at the lock's versions that a VM
// with these tool files doesn't have, as "claude 2.1.295".
func (t VMs) Newer(ctx context.Context, names, have []string, tg Target) ([]string, error) {
	files, err := Versions(t).Files(ctx, names, tg)
	if err != nil {
		return nil, err
	}
	var newer []string
	for _, f := range files {
		if !slices.Contains(have, f) {
			newer = append(newer, Label([]string{f}))
		}
	}
	return newer, nil
}

// WarnNewer warns when there are newer shared tools than a VM's: moving
// to them is cheap, as nothing in the VM's disk changes. It never changes
// the VM.
func (t VMs) WarnNewer(ctx context.Context, names, have []string, tg Target) error {
	newer, err := t.Newer(ctx, names, have, tg)
	if err != nil {
		return err
	}
	if len(newer) > 0 {
		t.Warn(msg.NewerTools(strings.Join(newer, ", ")))
	}
	return nil
}

// Update moves a VM to the newest of its shared tools (names): it
// checks upstream first (unless there's no lock yet: resolving it is
// checking), has build build what the share lacks of them (the VM's own,
// when they're the newest and one is gone from the share), and returns the
// VM's new tool files. changed is false when they were the newest already.
func (t VMs) Update(ctx context.Context, build func(context.Context, []string) error, vm vmname.VM, names, have []string, tg Target) (files []string, changed bool, err error) {
	versions := Versions(t)
	if _, err := os.Stat(t.Dirs.ToolLock()); err == nil {
		versions.Check(ctx)
	}
	if files, err = versions.Files(ctx, names, tg); err != nil {
		return nil, false, err
	}
	if err := build(ctx, files); err != nil {
		return nil, false, err
	}
	// The same files in any order: tools added since are listed last.
	if slices.Equal(slices.Sorted(slices.Values(files)), slices.Sorted(slices.Values(have))) {
		t.Info(msg.ToolsNewest(vm))
		return files, false, nil
	}
	t.Info(msg.ToolsSwitch(vm))
	return files, true, nil
}
