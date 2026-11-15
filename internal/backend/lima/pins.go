package lima

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
)

// On a Linux host, a start with pins (backend.Confinement.Pins: .git's
// hooks and config, as .git protection asks) runs QEMU in a user and
// mount namespace of its own, where each pinned path is bound over
// itself, read-only or not (platform.PinAll). devvm _qemu exec, which
// Lima starts as QEMU, starts devvm _qemu pinned in the namespace, as the
// same user with CAP_SYS_ADMIN there alone: it pins each path, and runs
// devvm _qemu drop as its child, which drops every capability and becomes
// QEMU, killed if _qemu pinned ends. The guest reaches the project folder
// only through QEMU's 9p, so it can't change what's pinned, and the
// namespace, and its pins, end with QEMU. A git command on the host that
// replaces a pinned file (git config renames its new config over the old
// one) leaves the new file unpinned in the namespace until _qemu pinned,
// which watches each pinned path's folder, pins it again, and records
// that in the pins' events file, which .git protection reports.

// PinsEnv names the variable holding the file that says what QEMU's
// namespace pins (backend.Pins.File, written by encodePins); unset or
// empty, QEMU runs in devvm's.
const PinsEnv = "DEVVM_QEMU_PINS"

// maxPinsFile bounds the pins file decodePins reads: 100 nested repos'
// pins take a few hundred KiB at most.
const maxPinsFile = 8 << 20

// NetEnv names the variable that says how QEMU's network is restricted:
// open for a VM on Lima's own network, whose arguments QEMU gets as Lima
// gives them; anything else, restrict=on (RestrictNetdev).
const NetEnv = "DEVVM_QEMU_NET"

// netOpen is NetEnv's value for Lima's own network.
const netOpen = "open"

// pinSet is what a start pins, as devvm _qemu reads it: the project
// folder, and each path below it, in the order they're pinned.
type pinSet struct {
	Root   string     `json:"root"`
	Pins   []pinEntry `json:"pins"`
	Events string     `json:"events,omitempty"` // backend.Pins.Events
}

type pinEntry struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

// pinsOf is p in the order its paths are pinned: by depth, and at a depth
// the fixed ones first, so a folder's pin never hides one below it.
func pinsOf(p backend.Pins) pinSet {
	s := pinSet{Root: p.Root, Events: p.Events}
	for _, d := range p.Fixed {
		s.Pins = append(s.Pins, pinEntry{Path: d})
	}
	for _, r := range p.ReadOnly {
		s.Pins = append(s.Pins, pinEntry{Path: r, ReadOnly: true})
	}
	slices.SortStableFunc(s.Pins, func(a, b pinEntry) int {
		if d := strings.Count(a.Path, "/") - strings.Count(b.Path, "/"); d != 0 {
			return d
		}
		return pinOrder(a.ReadOnly) - pinOrder(b.ReadOnly)
	})
	return s
}

func pinOrder(readOnly bool) int {
	if readOnly {
		return 1
	}
	return 0
}

// check refuses a set whose folder isn't an absolute, clean path, or
// with a path that isn't relative, clean and below it.
func (s pinSet) check() error {
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root || len(s.Pins) == 0 {
		return msg.QEMUPinsInvalid(s.Root)
	}
	if s.Events != "" && !filepath.IsAbs(s.Events) {
		return msg.QEMUPinsInvalid(s.Events)
	}
	for _, e := range s.Pins {
		rel := e.Path
		if rel == "" || rel == "." || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
			return msg.QEMUPinsInvalid(rel)
		}
	}
	return nil
}

// pins are the set's paths as platform pins them.
func (s pinSet) pins() []platform.Pin {
	out := make([]platform.Pin, 0, len(s.Pins))
	for _, e := range s.Pins {
		out = append(out, platform.Pin{Path: e.Path, ReadOnly: e.ReadOnly})
	}
	return out
}

// encodePins writes p, checked first, to its file (p.File), private, and
// returns the file, as PinsEnv holds it.
func encodePins(p backend.Pins) (string, error) {
	s := pinsOf(p)
	if err := s.check(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(p.File) {
		return "", msg.QEMUPinsInvalid(p.File)
	}
	b, _ := json.Marshal(s)
	return p.File, state.WriteFileMode(p.File, b, 0o600)
}

// decodePins reads what encodePins wrote to the file v, a regular file
// read through no symlink, checking it again.
func decodePins(v string) (pinSet, error) {
	if !filepath.IsAbs(v) {
		return pinSet{}, msg.QEMUPinsInvalid(PinsEnv)
	}
	b, err := safefile.Read(v, maxPinsFile, safefile.Opts{})
	if err != nil {
		return pinSet{}, msg.QEMUPinsInvalid(v).Because(err)
	}
	var s pinSet
	if err := json.Unmarshal(b, &s); err != nil {
		return pinSet{}, msg.QEMUPinsInvalid(v).Because(err)
	}
	return s, s.check()
}

// runPinned is devvm _qemu exec with pins: it runs devvm _qemu pinned in a
// namespace of its own, which pins and becomes QEMU, and returns once QEMU
// ends (platform.RunInNamespace). QEMU gets no other file of Lima's, as
// without pins (devvm marks them close-on-exec as it starts).
func runPinned(self, qemu string, args []string) error {
	if _, err := decodePins(os.Getenv(PinsEnv)); err != nil {
		return err
	}
	if err := platform.RunInNamespace(self, append([]string{"_qemu", "pinned", qemu}, args...)); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return err // QEMU's own, or the pins' message, which it printed
		}
		return msg.QEMUPinsFailed(err.Error()).Because(err)
	}
	return nil
}

// keepEvery is how often _qemu pinned looks at the pins without a change
// in their folders: when a folder itself was replaced, which its watch
// misses.
const keepEvery = 2 * time.Second

// RunPinned is devvm _qemu pinned QEMU ARGS, in QEMU's namespace: it pins
// what PinsEnv says, and runs QEMU as its child through devvm _qemu drop
// (self), keeping the pins while QEMU runs and recording each change in
// the set's events file (platform.RunPinned). It refuses to start QEMU
// when a path can't be pinned, or their folders can't be watched.
func RunPinned(self, qemu string, args []string) error {
	s, err := decodePins(os.Getenv(PinsEnv))
	if err != nil {
		return err
	}
	// What changes is recorded, or QEMU doesn't start: an events file
	// it can't write would hide them.
	if err := addPinEvent(s.Events, backend.PinEvent{What: backend.PinKeeping}); err != nil {
		return msg.QEMUPinsFailed(err.Error()).Because(err)
	}
	argv := append([]string{self, "_qemu", "drop", qemu}, args...)
	err = platform.RunPinned(s.Root, s.pins(), argv, keepEvery, func(path, what string) {
		recordPinEvent(s.Events, backend.PinEvent{Path: path, What: what})
	})
	var exit *exec.ExitError
	if err == nil || errors.As(err, &exit) {
		return err // QEMU's own
	}
	return msg.QEMUPinsFailed(err.Error()).Because(err)
}

// recordPinEvent adds e to the events file, when there's one, and says it
// on QEMU's error output, which Lima logs. One it can't add is said
// there loudly, and marked in the file if it still takes a line
// (backend.PinUnrecorded), which the next check names; a file removed
// while QEMU runs is made again without its first line, which the check
// names too (gitprotect's ReportPinEvents).
func recordPinEvent(file string, e backend.PinEvent) {
	b, _ := json.Marshal(e)
	_, _ = os.Stderr.Write(append([]byte("devvm: pin "), append(b, '\n')...)) //nolint:forbidigo // QEMU's error output, which Lima logs
	if err := addPinEvent(file, e); err != nil {
		_, _ = os.Stderr.WriteString("devvm: WARNING: couldn't record the pin event above (" + err.Error() + "); check .git once the VM stops\n") //nolint:forbidigo // QEMU's error output, which Lima logs
		_ = addPinEvent(file, backend.PinEvent{Path: e.Path, What: backend.PinUnrecorded})
	}
}

// addPinEvent adds e to the events file, as a line of JSON; "" is none.
func addPinEvent(file string, e backend.PinEvent) error {
	if file == "" {
		return nil
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // devvm's own, in the VM's run folder
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

// Drop is devvm _qemu drop QEMU ARGS, _qemu pinned's child: it replaces
// itself with QEMU with no capability left and without PinsEnv
// (platform.ExecUnprivileged).
func Drop(qemu string, args []string) error {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, PinsEnv+"=") {
			env = append(env, kv)
		}
	}
	return platform.ExecUnprivileged(qemu, append([]string{qemu}, args...), env)
}

// qemuMounts is the host paths that the QEMU whose pid file is pidFile
// sees through a mount of their own, from its mountinfo: those
// read-only (backend.Run.ReadOnly), and all of them (Mounted), what its
// pins hold. nil when there's no such QEMU to read.
func qemuMounts(pidFile string) (readOnly, mounted []string) {
	b, err := os.ReadFile(pidFile) //nolint:gosec // Lima's own, in the VM's Lima folder
	if err != nil {
		return nil, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return nil, nil
	}
	info, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/mountinfo")
	if err != nil {
		return nil, nil
	}
	return mountPoints(string(info))
}

// mountPoints is a mountinfo's mount points, their octal escapes undone:
// those whose mount is read-only, and all of them.
func mountPoints(info string) (readOnly, all []string) {
	readOnly, all = []string{}, []string{}
	for line := range strings.Lines(info) {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		p := unescapeMount(f[4])
		all = append(all, p)
		if slices.Contains(strings.Split(f[5], ","), "ro") {
			readOnly = append(readOnly, p)
		}
	}
	return readOnly, all
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ProbePin is devvm _qemu probe DIR, in a namespace of its own: it pins
// DIR/f read-only and checks that writing it fails (platform.ProbePin).
func ProbePin(dir string) error { return platform.ProbePin(dir) }

// probeTimeout bounds the probe.
const probeTimeout = 10 * time.Second

// probePins reports whether this host pins paths for QEMU (nil), or why
// not: it pins a file in a namespace as a start would (ProbePin), with
// devvm's own binary, as AppArmor's profile for devvm is for it.
func probePins(self string) error {
	return platform.Probe(func(dir string) []string { return []string{self, "_qemu", "probe", dir} }, probeTimeout)
}
