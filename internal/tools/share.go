package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/trust"
)

// LockShare holds the share's lock, for a build writing tool images or a
// swap of the lock, until unlock is called or the process ends, however
// it ends: it's an OS lock on an open file. Another devvm holding it is
// waited for until ctx ends, saying so after ShareNote.
func (t *Env) LockShare(ctx context.Context) (unlock func(), err error) {
	if err := os.MkdirAll(t.Dirs.Tools(), 0o700); err != nil {
		return nil, err
	}
	return platform.LockFileWaiting(ctx, t.Dirs.ShareLock(), t.ShareNote, func() { t.Info(msg.ShareWaiting(t.Dirs.Tools())) })
}

// ShareBusy reports whether another devvm holds the share's lock
// (LockShare) now; false when it can't tell.
func (t *Env) ShareBusy() bool {
	unlock, ok, err := platform.TryLockFile(t.Dirs.ShareLock())
	if err != nil {
		return false
	}
	if ok {
		unlock()
	}
	return !ok
}

// Built reports whether a tool image is in the share with the record
// devvm wrote when it moved it in (FILE.json).
func (t Share) Built(file string) bool {
	if fi, err := os.Stat(t.Dirs.ToolImage(file)); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	_, err := state.ReadToolImage(t.Dirs, file)
	return err == nil
}

// Build builds the tool images these files name that the share lacks,
// each in a throwaway VM of its own made from base's image (key base) on
// b, its features checked against rv, in tg's format. base must be the
// base they're for (buildsFor). The VMs run upstream installers, so none
// sees the share, and none sees another tool's build: each writes into a
// folder of its own, and its one image is checked to be a plain file,
// moved into the share and its hash recorded.
func (t Share) Build(ctx context.Context, rv backend.Review, b backend.Backend, base string, tg Target, files []string) error {
	var todo []string
	for _, f := range files {
		if !t.Built(f) {
			todo = append(todo, f)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if err := t.buildsFor(base, tg); err != nil {
		return err
	}
	// The company proxy the build goes through, with config.toml's
	// password saved, before anything starts.
	if t.Proxy != nil {
		if _, err := t.Proxy.Upstream(ctx); err != nil {
			return err
		}
	}
	spec := backend.ToolsSpec{Net: backend.NetInternet}
	if err := rv.Require(b, Label(todo), spec.Features()...); err != nil {
		return err
	}
	unlock, err := t.LockShare(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	lock, err := Versions(t).Lock(ctx)
	if err != nil {
		return err
	}
	var build []string
	for _, f := range todo {
		if t.Built(f) {
			continue // another devvm built it while this one waited
		}
		tool, v, ok := image.ParseToolFile(f)
		if !ok {
			return msg.ToolBadOutput(f)
		}
		// build.sh installs the lock's version: the lock may have moved
		// on while this one waited.
		if cur, err := t.File(lock, tool, tg); err != nil {
			return err
		} else if cur != f {
			return msg.ToolsChanged()
		}
		if t.noDownload(lock, tool, tg.Libc) {
			return msg.ToolUnbuilt(tool, v)
		}
		build = append(build, f)
	}
	if len(build) == 0 {
		return nil
	}
	// The build VM is the backend's; the images come out into devvm's data
	// folder, whatever the backend. Both grow with the tools built.
	space, label := backend.Space{Free: t.FreeSpace}, Label(build)
	if err := space.Check(b, label, func(f backend.Floors) uint64 { return f.ForTools(len(build)) }); err != nil {
		return err
	}
	if err := space.CheckDir(t.Dirs.Data, label, backend.ToolImages(len(build))); err != nil {
		return err
	}
	t.Info(msg.BuildingTools(Label(build)))
	t.LogNote()
	for _, f := range build {
		if err := t.buildOne(ctx, b, base, tg, lock, f); err != nil {
			return err
		}
	}
	return nil
}

// buildsFor refuses to build tg's tool images in the image with key base
// unless its record says it's the base they're for: devvm's own Ubuntu
// base for gnu, whatever base the VM has, and for musl a --from base
// with tg's digest, which their keys hash. So a gnu image, which any VM
// may mount, never builds in a --from image, whose code root in its
// build ran, and a musl one is only ever another VM's on the same image.
func (t Share) buildsFor(base string, tg Target) error {
	rec, err := state.ReadImage(t.Dirs, base)
	if err != nil {
		return msg.ToolsWrongBase(base).Because(err)
	}
	ok := rec.Name == image.Base && rec.Libc == tg.Libc
	switch tg.Libc {
	case "gnu":
		ok = ok && rec.From == "" && tg.Base == ""
	case "musl":
		ok = ok && rec.From != "" && rec.Digest != "" && rec.Digest == tg.Base
	default:
		ok = false
	}
	if !ok {
		return msg.ToolsWrongBase(base)
	}
	return nil
}

// buildOne builds one tool image in a throwaway VM with a folder of its
// own, for tg, then moves it into the share. One VM and folder for each
// tool: whatever a tool's installer does in its VM, as root or not, it
// can't reach another tool's image, which its key doesn't hash.
func (t Share) buildOne(ctx context.Context, b backend.Backend, base string, tg Target, lock Lock, file string) error {
	out, err := os.MkdirTemp(t.Dirs.Data, paths.ToolsBuildPrefix)
	if err != nil {
		return err
	}
	remove := t.AddCleanup(func(context.Context) { _ = os.RemoveAll(out) })
	defer func() {
		_ = os.RemoveAll(out)
		remove()
	}()
	if out, err = filepath.EvalSymlinks(out); err != nil {
		return err
	}
	build := filepath.Join(out, ".build")
	if err := os.Mkdir(build, 0o700); err != nil {
		return err
	}
	config, err := t.config()
	if err != nil {
		return err
	}
	script, err := image.OwnScript(t.Assets, "shared-tools/build.sh")
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"mise.toml": config, "mise.lock": lock.Bytes()} {
		if err := state.WriteFile(filepath.Join(build, name), data); err != nil {
			return err
		}
	}
	tool, version, _ := image.ParseToolFile(file)
	// The user's CAs, trusted in the build VM, which is deleted: a tool
	// image's key holds none of them.
	cas, err := trust.Read(t.Dirs.TrustedCAs())
	if err != nil {
		return err
	}
	ca, err := cas.Build(t.Assets)
	if err != nil {
		return err
	}
	args := []string{tool, version, file}
	if image.OwnDownload(tool) {
		// build.sh fetches it itself, from the lock's download for this
		// VM's platform, checked against the lock's checksum.
		url, sum := lock.Download(tool, t.platform(tg.Libc))
		if !strings.HasPrefix(url, "https://") || !strings.HasPrefix(sum, "sha512:") {
			return msg.ToolUnbuilt(tool, version)
		}
		args = append(args, url, strings.TrimPrefix(sum, "sha512:"))
	}
	spec := backend.ToolsSpec{
		Name: Label([]string{file}), Base: base, Out: out, OutAt: ShareAt, Args: args, Format: tg.Format,
		Net: backend.NetInternet, Script: backend.Script{Name: "build.sh", Data: script}, Trust: ca,
	}
	if err := b.BuildTools(ctx, spec); err != nil {
		return err
	}
	return t.moveIn(b, out, tg, file)
}

// moveIn moves the tool image file that a build wrote into out, its VM
// gone, so nothing changes out now, into the share, with its record. Plain
// files only (a link could name a host file). Only the image leaves out;
// its hash is written in the share, never through a name the VM could
// have planted.
func (t Share) moveIn(b backend.Backend, out string, tg Target, file string) error {
	src := filepath.Join(out, file)
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		return msg.ToolBadOutput(file)
	}
	sum, err := image.SHA256File(src)
	if err != nil {
		return err
	}
	// Its file name doesn't say what it's built for: the record does.
	rec := state.ToolImage{SHA256: sum, Libc: tg.Libc, Base: tg.Base, Format: tg.Format, Backend: b.Name(), Built: t.Now().UTC().Truncate(time.Second), MadeBy: t.Version}
	if tg.Libc == "gnu" {
		if rec.Glibc, err = neededGlibc(filepath.Join(out, file+".glibc")); err != nil {
			return msg.ToolBadOutput(file + ".glibc")
		}
	}
	if err := os.Rename(src, t.Dirs.ToolImage(file)); err != nil {
		return err
	}
	return state.WriteToolImage(t.Dirs, file, rec)
}

// maxGlibcOutput caps what's read of build.sh's FILE.glibc: one version.
const maxGlibcOutput = 64

// neededGlibc reads what build.sh measured of a gnu tool image, FILE.glibc
// in the build's folder: the newest glibc version its programs and
// libraries need, or nothing when they need none. It must be a plain file
// holding just that (image.ValidGlibc).
func neededGlibc(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxGlibcOutput {
		return "", errors.New("not a version")
	}
	data, err := os.ReadFile(path) //nolint:gosec // devvm's own build folder, checked to be a plain file
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(string(data), "\n")
	if v != "" && !image.ValidGlibc(v) {
		return "", errors.New("not a version")
	}
	return v, nil
}

// Runs checks that each of these tool images, built, runs on tg's base:
// a --from gnu base whose glibc is older than the newest one a tool image
// needs refuses it (tool_needs_glibc), naming the tool and both versions.
// A base without a glibc version (Ubuntu's own, musl) isn't checked.
func (t Share) Runs(files []string, tg Target) error {
	if tg.Libc != "gnu" || !image.ValidGlibc(tg.Glibc) {
		return nil
	}
	for _, f := range files {
		rec, err := state.ReadToolImage(t.Dirs, f)
		if err != nil {
			return err
		}
		if image.ValidGlibc(rec.Glibc) && image.GlibcOlder(tg.Glibc, rec.Glibc) {
			tool, v, _ := image.ParseToolFile(f)
			return msg.ToolNeedsGlibc(tg.Glibc, rec.Glibc, tool, v)
		}
	}
	return nil
}

// Unused returns the tool images of a List that it calls unused: neither
// the lock's version nor listed by a VM. Without a lock, none is. Callers
// deleting them hold the share's lock from before the List, so no build
// or lock swap changes the share meanwhile.
func Unused(ls []Listed) []Listed {
	var out []Listed
	for _, l := range ls {
		if l.State == StateUnused && l.File != "" {
			out = append(out, l)
		}
	}
	return out
}

// Delete deletes tool images from the share, with their records. Only
// tool image names are taken, nothing else in the share.
func (t Share) Delete(files []string) error {
	for _, f := range files {
		if _, _, ok := image.ParseToolFile(f); !ok || filepath.Base(f) != f {
			return fmt.Errorf("not a tool image: %q", f)
		}
	}
	for _, f := range files {
		for _, name := range []string{t.Dirs.ToolImage(f), t.Dirs.ToolRecord(f)} {
			if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
