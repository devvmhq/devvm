package vm

import (
	"context"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Size is a VM's size as devvm stores it, its state's size section (what
// was asked for): CPUs a whole number, memory and disk in GiB (decimals
// allowed). "" is unset: the image's.
type Size struct {
	CPUs   string `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// The parts of a size, as devvm size shows them.
const (
	KeyCPUs   = "cpus"
	KeyMemory = "memory" // GiB
	KeyDisk   = "disk"   // GiB
)

// Parse checks every size set, as flags give them.
func (s Size) Parse() (Size, error) {
	var err error
	if s.CPUs != "" {
		if s.CPUs, err = settings.ParseCPUs("--cpus", s.CPUs); err != nil {
			return Size{}, err
		}
	}
	if s.Memory != "" {
		if s.Memory, err = settings.ParseGiB("--memory", s.Memory); err != nil {
			return Size{}, err
		}
	}
	if s.Disk != "" {
		if s.Disk, err = settings.ParseGiB("--disk", s.Disk); err != nil {
			return Size{}, err
		}
	}
	return s, nil
}

// defaultSize is the size new VMs get, as def has it.
func defaultSize(def settings.Defaults) Size {
	return Size{CPUs: def.CPUs.Value, Memory: def.Memory.Value, Disk: def.Disk.Value}
}

// Or returns s with what it leaves unset taken from def.
func (s Size) Or(def Size) Size {
	if s.CPUs == "" {
		s.CPUs = def.CPUs
	}
	if s.Memory == "" {
		s.Memory = def.Memory
	}
	if s.Disk == "" {
		s.Disk = def.Disk
	}
	return s
}

// Host is what a host can give a VM.
type Host struct {
	CPUs      int
	MemoryGiB int // whole GiB, rounded down
}

// memoryBytes is the host's whole GiB in bytes, 0 for 0 or less.
func (h Host) memoryBytes() uint64 {
	if h.MemoryGiB <= 0 {
		return 0
	}
	return uint64(h.MemoryGiB) << 30
}

// ThisHost returns the CPUs and memory of h, this machine as devvm reads
// it.
func ThisHost(h platform.Host) (Host, error) {
	mem, err := h.Memory()
	if err != nil {
		return Host{}, err
	}
	return Host{CPUs: h.CPUs(), MemoryGiB: int(mem >> 30)}, nil
}

// Check refuses a parsed size the host can't give: more CPUs than it has,
// or more memory than backend.MaxMemory allows, which keeps memory for
// the host as base's fitting does. It also refuses a disk smaller than
// minDisk GiB (the image's, which can't shrink; 0 for no limit). vm names
// the VM for the disk's refusal.
func (s Size) Check(h Host, minDisk float64, vm vmname.VM) error {
	b := s.Backend()
	if b.CPUs > h.CPUs {
		return msg.TooManyCpus(strconv.Itoa(h.CPUs), s.CPUs)
	}
	if most := backend.MaxMemory(h.memoryBytes()); b.MemoryGiB > most {
		return msg.TooMuchMemory(strconv.Itoa(h.MemoryGiB), strconv.FormatFloat(most, 'g', -1, 64), s.Memory)
	}
	if b.DiskGiB > 0 && b.DiskGiB < minDisk {
		return msg.DiskShrink(strconv.FormatFloat(minDisk, 'g', -1, 64), vm, s.Disk)
	}
	return nil
}

// Backend returns a parsed size as the backend has it.
func (s Size) Backend() backend.Size {
	var b backend.Size
	b.CPUs, _ = strconv.Atoi(s.CPUs)
	b.MemoryGiB, _ = strconv.ParseFloat(s.Memory, 64)
	b.DiskGiB, _ = strconv.ParseFloat(s.Disk, 64)
	return b
}

// pendingSize returns what of the size st asks for the VM, which has has,
// lacks: zero where it matches. Disks only grow, so a smaller one asked
// for is had.
func pendingSize(st *state.VM, has backend.Size) backend.Size {
	want := sectionOf[Size](st, sizeSection).Backend()
	var p backend.Size
	if want.CPUs != 0 && want.CPUs != has.CPUs {
		p.CPUs = want.CPUs
	}
	if want.MemoryGiB != 0 && want.MemoryGiB != has.MemoryGiB {
		p.MemoryGiB = want.MemoryGiB
	}
	if want.DiskGiB > has.DiskGiB {
		p.DiskGiB = want.DiskGiB
	}
	return p
}

// SizeText returns a size as devvm size shows it, without what's unset:
// cpus=4 memory=8GiB disk=100GiB.
func SizeText(s backend.Size) string {
	var f []string
	if s.CPUs > 0 {
		f = append(f, KeyCPUs+"="+strconv.Itoa(s.CPUs))
	}
	if s.MemoryGiB > 0 {
		f = append(f, KeyMemory+"="+strconv.FormatFloat(s.MemoryGiB, 'g', -1, 64)+"GiB")
	}
	if s.DiskGiB > 0 {
		f = append(f, KeyDisk+"="+strconv.FormatFloat(s.DiskGiB, 'g', -1, 64)+"GiB")
	}
	return strings.Join(f, " ")
}

// SizeShown is a size as devvm size shows it: the folder's VM's, or with
// -g the one new VMs get, 0 for what's the image's.
type SizeShown struct {
	backend.Size
	// Pending is what changes at the VM's next start, 0 for what doesn't.
	Pending backend.Size
	Global  bool
	// Sources is where each of -g's came from, by KeyCPUs, KeyMemory and
	// KeyDisk, and Labels each as text shows it (settings.Source.Label).
	Sources, Labels map[string]string
}

// Text is the size as devvm size prints it: cpus=4 memory=8GiB
// disk=100GiB for a VM, and for -g a key=value line each, empty for the
// image's, followed by where it came from.
func (s SizeShown) Text() string {
	if s.Global {
		var b strings.Builder
		for _, kv := range [][2]string{{KeyCPUs, intText(s.CPUs)}, {KeyMemory, gibValue(s.MemoryGiB)}, {KeyDisk, gibValue(s.DiskGiB)}} {
			b.WriteString(kv[0] + "=" + kv[1] + " # " + s.Labels[kv[0]] + "\n")
		}
		return b.String()
	}
	return SizeText(s.Size) + "\n"
}

// intText and gibValue write a size's numbers as devvm size -g shows
// them, "" for 0.
func intText(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func gibValue(n float64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// ShowSize returns the size of the folder's VM, saying what changes at
// its next start; with global, the size new VMs get and where each part
// came from.
func (e Settings) ShowSize(ctx context.Context, global bool) (SizeShown, error) {
	if global {
		def, err := settings.Read(e.Dirs)
		if err != nil {
			return SizeShown{}, err
		}
		s := SizeShown{Size: defaultSize(def).Backend(), Global: true, Sources: map[string]string{}, Labels: map[string]string{}}
		for k, v := range map[string]settings.Value{KeyCPUs: def.CPUs, KeyMemory: def.Memory, KeyDisk: def.Disk} {
			s.Sources[k], s.Labels[k] = string(v.Source), v.Source.Label(e.Dirs)
		}
		return s, nil
	}
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMSize})
	if err != nil {
		return SizeShown{}, err
	}
	s := SizeShown{Size: t.in.Size, Pending: pendingSize(t.st, t.in.Size)}
	switch {
	case s.Pending == (backend.Size{}):
	case t.in.Status == backend.StatusRunning:
		e.Note(msg.SizePendingRunning(SizeText(s.Pending), t.f.Name))
	default:
		e.Note(msg.SizePending(SizeText(s.Pending), t.f.Name))
	}
	return s, nil
}

// SetSize changes the folder's VM's size to what s sets: from its next
// start when it's stopped; when it runs, after a restart the user agrees
// to (restartOr), else from its next start. A disk only grows. With
// global, it changes the size new VMs get instead.
func (e Settings) SetSize(ctx context.Context, s Size, global bool) error {
	s, err := s.Parse()
	if err != nil {
		return err
	}
	host, err := e.Host()
	if err != nil {
		return err
	}
	if global {
		return SizeChoice{Dirs: e.Dirs, Host: host, Reporter: e.Reporter}.Set(s)
	}
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMSize})
	if err != nil {
		return err
	}
	vm, st, b := t.f.Name, t.st, t.b
	if err := e.Registry.Require(b, vm, s.Backend().Features()...); err != nil {
		return err
	}
	in := t.in
	if err := s.Check(host, in.Size.DiskGiB, vm); err != nil {
		return err
	}
	// What s sets, at once; what it doesn't set stays as asked before.
	err = update(ctx, st, sizeSection, func(had *Size) error {
		*had = s.Or(*had)
		return nil
	})
	if err != nil {
		return err
	}
	pending := pendingSize(st, in.Size)
	switch {
	case pending == (backend.Size{}):
		e.Info(msg.VMSize(SizeText(in.Size), vm))
	case in.Status == backend.StatusRunning:
		return e.restartOr(ctx, vm, e.Warn, msg.NextStart(1, vm))
	default:
		e.Info(msg.SizePending(SizeText(pending), vm))
	}
	return nil
}
