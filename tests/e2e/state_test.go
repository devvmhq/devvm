//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/vmname"
)

// Paths are the sandbox's folders and files that checks look at.
type Paths struct {
	Devvm    string // devvm's data folder ($XDG_DATA_HOME/devvm)
	Images   string // the image records, one folder per key
	Share    string // the shared tool images
	Layers   string // the user's layers ($XDG_CONFIG_HOME/devvm/layers)
	ToolLock string // the shared tools' versions (mise.lock)
	Projects string // the run's project folders
	CAs      string // devvm's CA folder, the user's CAs ($XDG_CONFIG_HOME/devvm/ca)
}

func (sb *sandbox) paths() Paths {
	return Paths{
		Devvm: sb.devvmDir(), Images: sb.images(), Share: sb.share(), Layers: sb.layers(),
		ToolLock: sb.toolLock(), Projects: filepath.Join(sb.work, "p"), CAs: filepath.Join(sb.config(), "devvm", "ca"),
	}
}

// Settle is how long a check waits before it looks for something that
// must not have happened: the one wait that isn't for an event (Until).
const Settle = 3 * time.Second

// Until runs fn each second until it returns nil, for at most timeout or
// the window open now, and returns its last error: a check waits for what
// it looks for, never a fixed time.
func (s *S) Until(timeout time.Duration, fn func() error) error {
	ctx, cancel := context.WithTimeout(s.r.window().ctx, timeout)
	defer cancel()
	for {
		err := fn()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
}

// Port is one of slot 0's fixed ports, as this slot has it: 100 × the
// slot more (1000 more for --newest-base), so slots never share one.
func (s *S) Port(p int) int { return p + s.sb.ports }

// Defer runs fn at the run's end, before its VMs are deleted, with a
// context of its own: for what a section made that the sandbox's cleanup
// doesn't know of (a Keychain item, a process).
func (s *S) Defer(fn func(context.Context)) { s.sb.deferred = append(s.sb.deferred, fn) }

// VMDir is the folder devvm keeps a VM's files in: a project VM's is
// vms/HASH, a build VM's run/builds/NAME.
func (p Paths) VMDir(vm string) string {
	if h, ok := vmname.VM(vm).Hash(); ok {
		return filepath.Join(p.Devvm, "vms", h)
	}
	return filepath.Join(p.Devvm, "run", "builds", vm)
}

// State is a VM's vm.json as the CLI tests' vm_state shows it: each
// field dotted (net.tier), a list of strings space-separated, a list of
// objects by index (links.links.0.name), strings as they are and anything
// else as JSON. Empty for a VM without one.
func (p Paths) State(vm string) map[string]string {
	return readState(filepath.Join(p.VMDir(vm), "vm.json"))
}

func readState(f string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(f)
	if err != nil {
		return out
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return out
	}
	flatten("", v, out)
	return out
}

func flatten(p string, v any, out map[string]string) {
	join := func(k string) string {
		if p == "" {
			return k
		}
		return p + "." + k
	}
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			flatten(join(k), e, out)
		}
	case []any:
		var words []string
		for i, e := range v {
			if w, ok := e.(string); ok {
				words = append(words, w)
			} else {
				flatten(join(strconv.Itoa(i)), e, out)
			}
		}
		if len(words) > 0 || len(v) == 0 {
			out[p] = strings.Join(words, " ")
		}
	case string:
		out[p] = v
	default:
		b, _ := json.Marshal(v)
		out[p] = string(b)
	}
}

// VMOf is the VM devvm made for the folder dir, from its state: nothing
// when there's none.
func (p Paths) VMOf(dir string) string {
	m, _ := filepath.Glob(filepath.Join(p.Devvm, "vms", "*", "vm.json"))
	for _, f := range m {
		if st := readState(f); st["dir"] == dir {
			return st["name"]
		}
	}
	return ""
}

// KeysOf is the keys of the images recorded under that name, sorted.
func (p Paths) KeysOf(image string) []string {
	m, _ := filepath.Glob(filepath.Join(p.Images, "*", "record.json"))
	var keys []string
	for _, f := range m {
		key := filepath.Base(filepath.Dir(f))
		if p.Record(key, "name") == image {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Record is a string field of the record of the image with that key,
// nothing without one.
func (p Paths) Record(key, field string) string {
	b, err := os.ReadFile(filepath.Join(p.Images, key, "record.json"))
	if err != nil {
		return ""
	}
	var rec map[string]any
	if json.Unmarshal(b, &rec) != nil {
		return ""
	}
	v, _ := rec[field].(string)
	return v
}

// ToolVersion is the tool's version in the shared tools' lock, its
// [[tools.TOOL]] entry's version: nothing without
// one.
func (p Paths) ToolVersion(tool string) string {
	b, err := os.ReadFile(p.ToolLock)
	if err != nil {
		return ""
	}
	in := false
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		switch {
		case line == "[[tools."+tool+"]]":
			in = true
		case strings.HasPrefix(line, "["):
			in = false
		case in:
			if v, ok := strings.CutPrefix(line, "version = "); ok {
				return strings.Trim(v, `"`)
			}
		}
	}
	return ""
}

// Own makes the images by that name this run's: deleted at its end.
func (s *S) Own(image string) {
	for _, k := range s.Paths.KeysOf(image) {
		s.sb.created = append(s.sb.created, "devvm-img-"+k)
	}
}

// NewProject is a new, empty project folder of the run's, as devvm sees
// it (its physical path), deleted at the run's end.
func (s *S) NewProject(name string) (string, error) {
	p := filepath.Join(s.sb.paths().Projects, name)
	unlock(s.r.window().ctx, p)
	if err := os.RemoveAll(p); err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0o755); err != nil { //nolint:gosec // a project folder, as a user's mkdir makes it
		return "", err
	}
	if !slices.Contains(s.sb.projects, p) {
		s.sb.projects = append(s.sb.projects, p)
	}
	return filepath.EvalSymlinks(p)
}

// Launch runs devvm args in dir, with input for the shell it opens
// (which then ends), and is the VM devvm made for the folder, recorded
// for the run's cleanup.
func (s *S) Launch(dir, input string, args ...string) (string, Res, error) {
	return s.launch(Cmd{Args: append([]string{s.sb.devvm}, args...), Dir: dir, Stdin: input + "\n"})
}

// launch runs c, a launch of devvm in c.Dir: the VM devvm made for the
// folder, recorded for the run's cleanup.
func (s *S) launch(c Cmd) (string, Res, error) {
	res, err := s.Run(c)
	vm := s.Paths.VMOf(c.Dir)
	OwnVM(s, vm)
	return vm, res, err
}

// Reuse is Launch in the run's project folder name, which it makes, for a
// VM whose configuration its folder's name fixes. With --reuse, the
// folder and its VM stay for the next run, which takes the VM as it
// finds it when it's running: iterating on one check doesn't launch it
// again. Without it, it's NewProject and Launch.
func (s *S) Reuse(name, input string, args ...string) (string, string, Res, error) {
	if !s.sb.o.reuse {
		dir, err := s.NewProject(name)
		if err != nil {
			return "", "", Res{}, err
		}
		vm, res, err := s.Launch(dir, input, args...)
		return dir, vm, res, err
	}
	p := filepath.Join(s.sb.paths().Projects, name)
	if err := os.MkdirAll(p, 0o755); err != nil { //nolint:gosec // a project folder, as a user's mkdir makes it
		return "", "", Res{}, err
	}
	dir, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", "", Res{}, err
	}
	vm := s.Paths.VMOf(dir)
	var res Res
	if vm == "" || s.Status(vm) != "Running" {
		vm, res, err = s.Launch(dir, input, args...)
	}
	if vm != "" {
		s.sb.kept = append(s.sb.kept, vm)
	}
	return dir, vm, res, err
}

// Layer writes a layer of the run's, name its file's (e2egc.sh), deleted
// at the run's end, and is its path.
func (s *S) Layer(name, script string) (string, error) {
	if !strings.HasPrefix(name, "e2e") {
		return "", errors.New("a run's layers are named e2e*: " + name)
	}
	f := filepath.Join(s.sb.layers(), name)
	s.sb.made = append(s.sb.made, f)
	return f, os.WriteFile(f, []byte(script), 0o600)
}
