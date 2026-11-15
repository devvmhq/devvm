package vm

import (
	"errors"
	"strconv"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

// SizeChoice chooses the size new VMs get, which devvm size -g saves in
// settings.json: from flags (Set), or asked for (Ask). Nothing rebuilds,
// and existing VMs keep their size.
type SizeChoice struct {
	Dirs paths.Dirs
	Host Host
	// Reporter asks for a value (Answer: the line typed, "" for Enter),
	// the question's context said first, warns of a refused answer and
	// says what's saved (Info).
	msg.Reporter
}

// sizeValues is what s sets, as settings.Save takes it.
func sizeValues(s Size) settings.Values {
	var v settings.SizeValues
	for _, f := range []struct {
		val string
		p   **string
	}{{s.CPUs, &v.CPUs}, {s.Memory, &v.Memory}, {s.Disk, &v.Disk}} {
		if f.val != "" {
			*f.p = settings.Of(f.val)
		}
	}
	return settings.Values{Size: v}
}

// Set saves the CPUs, memory and disk s sets as new VMs' size, after
// checking them against the host, and keeps what it leaves unset as
// saved. One config.toml sets is refused (set_in_config).
func (c SizeChoice) Set(s Size) error {
	s, err := s.Parse()
	if err != nil {
		return err
	}
	if err := s.Check(c.Host, 0, ""); err != nil {
		return err
	}
	def, err := settings.Save(c.Dirs, sizeValues(s))
	if err != nil {
		return err
	}
	c.Info(msg.SizeDefaults(SizeText(defaultSize(def).Backend())))
	return nil
}

// Ask asks for new VMs' CPUs and memory, each starting from the size in
// effect, else base's fitted to the host (Fitted), and says it skips one
// config.toml sets. Enter keeps one; a value the host can't give is said,
// and asked for again. Only what's typed is saved, so keeping both writes
// nothing, and where it can't ask, it returns the question's refusal,
// having saved nothing.
func (c SizeChoice) Ask() error {
	def, err := settings.Read(c.Dirs)
	if err != nil {
		return err
	}
	shown := defaultSize(def).Or(c.Fitted())
	// Said before the first question asked.
	about := []msg.M{msg.SizeQuestion(strconv.Itoa(c.Host.CPUs), strconv.Itoa(c.Host.MemoryGiB))}
	var typed Size
	for _, q := range []struct {
		key    string
		from   settings.Value
		prompt msg.M
		into   *string
		parse  func(string) (Size, error)
	}{
		{settings.KeyCPUs, def.CPUs, msg.SizeCpusPrompt(shown.CPUs), &typed.CPUs, typedCPUs},
		{settings.KeyMemory, def.Memory, msg.SizeMemoryPrompt(shown.Memory), &typed.Memory, typedMemory},
	} {
		if q.from.Source == settings.FromConfig {
			c.Info(msg.SetupInConfig(c.Dirs.ConfigFile(), q.key))
			continue
		}
		if *q.into, err = c.ask(q.prompt.AnsweredBy("devvm size -g"), about, q.parse); err != nil {
			return err
		}
		about = nil
	}
	if typed != (Size{}) {
		if _, err := settings.Save(c.Dirs, sizeValues(typed)); err != nil {
			return err
		}
	}
	c.Info(msg.SizeDefaults(SizeText(typed.Or(shown).Backend())))
	return nil
}

// typedCPUs and typedMemory are the CPUs and the memory typed at a size
// question, parsed as --cpus and --memory take them.
func typedCPUs(v string) (Size, error) {
	v, err := settings.ParseCPUs("--cpus", v)
	return Size{CPUs: v}, err
}

func typedMemory(v string) (Size, error) {
	v, err := settings.ParseGiB("--memory", v)
	return Size{Memory: v}, err
}

// typedDisk is the disk typed at a size question, parsed as --disk takes
// it.
func typedDisk(v string) (Size, error) {
	v, err := settings.ParseGiB("--disk", v)
	return Size{Disk: v}, err
}

// AskVM asks for vm's CPUs, memory and disk, once its summary showed
// shown and the user chose to customize it, each starting from shown's
// and skipped when given (--cpus, --memory, --disk) set it; the disk's
// question says first that a disk grows but never shrinks. Enter keeps
// one; a value the host can't give, or a disk below the image's
// (backend.BaseDiskGiB), is said, and asked for again. It returns s with
// what's typed; where it can't ask, s as it is. Nothing is saved.
func (c SizeChoice) AskVM(s, shown, given Size, vm vmname.VM) Size {
	about := []msg.M{msg.NewVMSizeQuestion(strconv.Itoa(c.Host.CPUs), strconv.Itoa(c.Host.MemoryGiB))}
	for _, q := range []struct {
		given  string
		prompt msg.Question
		about  []msg.M // said before it instead, when set
		into   *string
		parse  func(string) (Size, error)
	}{
		{given.CPUs, msg.NewVMCpusPrompt(shown.CPUs).AnsweredBy("--cpus"), nil, &s.CPUs, typedCPUs},
		{given.Memory, msg.NewVMMemoryPrompt(shown.Memory).AnsweredBy("--memory"), nil, &s.Memory, typedMemory},
		{given.Disk, msg.NewVMDiskPrompt(shown.Disk).AnsweredBy("--disk"), []msg.M{msg.NewVMDiskQuestion()}, &s.Disk, typedDisk},
	} {
		if q.given != "" {
			continue
		}
		if q.about != nil {
			about = q.about
		}
		v, err := c.askFor(q.prompt, about, q.parse, backend.BaseDiskGiB, vm)
		if err != nil {
			return s
		}
		if v != "" {
			*q.into = v
		}
		about = nil
	}
	return s
}

// ask asks q, about said first the first time, until the answer is
// empty, or parse takes it and the host can give it; it returns the
// parsed value, "" for Enter, or the question's refusal.
func (c SizeChoice) ask(q msg.Question, about []msg.M, parse func(string) (Size, error)) (string, error) {
	return c.askFor(q, about, parse, 0, "")
}

// askFor is ask, with a disk typed for vm checked against minDisk too
// (Size.Check).
func (c SizeChoice) askFor(q msg.Question, about []msg.M, parse func(string) (Size, error), minDisk float64, vm vmname.VM) (string, error) {
	for {
		v, err := c.Answer(q.About(about...))
		if err != nil || v == "" {
			return "", err
		}
		about = nil
		s, err := parse(v)
		if err == nil {
			err = s.Check(c.Host, minDisk, vm)
		}
		if err == nil {
			return s.CPUs + s.Memory + s.Disk, nil
		}
		if m, ok := errors.AsType[msg.M](err); ok {
			c.Warn(m)
		}
	}
}

// Fitted is base's CPUs and memory (backend.BaseCPUs,
// backend.BaseMemoryGiB), fitted to the host (backend.FitCPUs,
// backend.FitMemory): base's own when the host has room for them.
func (c SizeChoice) Fitted() Size {
	cpus := backend.FitCPUs(backend.BaseCPUs, c.Host.CPUs)
	mem := backend.FitMemory(backend.BaseMemoryGiB, c.Host.memoryBytes())
	return Size{CPUs: strconv.Itoa(cpus), Memory: strconv.FormatFloat(mem, 'g', -1, 64)}
}

// firstRunSize asks for the size new VMs get on devvm's first run, until
// base's features are chosen (def), when nothing sets a size and the
// user gave neither --cpus nor --memory; where it can't ask, new VMs
// keep the size they get. The first run chooses base's features after
// it (firstRun), so it doesn't ask again.
func (e creation) firstRunSize(given Size, def settings.Defaults) error {
	if given.CPUs != "" || given.Memory != "" || def.Features.Chosen {
		return nil
	}
	for _, v := range []settings.Value{def.CPUs, def.Memory, def.Disk} {
		if v.Source != settings.FromBuiltin {
			return nil
		}
	}
	host, err := e.Host()
	if err != nil {
		return err
	}
	err = SizeChoice{Dirs: e.Dirs, Host: host, Reporter: e.Reporter}.Ask()
	if _, unasked := errors.AsType[*msg.Refusal](err); unasked {
		return nil
	}
	return err
}
