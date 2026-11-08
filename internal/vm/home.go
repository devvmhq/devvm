package vm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/tools"
)

// homeOut is what reads a VM's home folder: everything on the home
// folder's own filesystem (no mount below it), what the user can't read
// skipped, as a gzipped tar stream. GNU tar's 1 is a file that changed
// while it was read, which the VM's processes do.
const homeOut = "cd ~ && tar -czf - --one-file-system --ignore-failed-read . ; s=$?; [ \"$s\" -le 1 ]"

// homeIn is what puts a saved home folder into a new VM's: only what the
// new home lacks, so a file or folder the image put there, its metadata
// included, stays the image's (GNU tar takes no --no-overwrite-dir with
// it: it's implied).
const homeIn = "cd ~ && tar -xzf - --skip-old-files"

// homeSkips are the paths in a VM's home, relative to it, that its
// remake doesn't keep, because the new VM's setup writes them itself:
// devvm's own files (its environment, with the tokens' placeholders),
// the ssh key its backend logs in with, the
// agents' folders linked to their mounts and the files copied from
// devvm's store, the agents' logins, and caches, which are made again.
func homeSkips(d paths.Dirs, toolNames []string) []string {
	skips := []string{".config/devvm", ".ssh/authorized_keys", ".cache"}
	skips = append(skips, agents.HistoryExcludes()...)
	for _, a := range image.AgentsOf(toolNames) {
		store := agentStore(d, a)
		skips = append(skips, agentDirs(store, a)...)
		skips = append(skips, agentFiles(store, a)...)
	}
	return skips
}

// copyHome copies a home folder's tar stream, gzipped, from r to w,
// gzipped again, keeping only what's safe to put into a new VM's home:
// regular files, folders and symlinks, and hard links to a file it kept,
// each with a relative path inside the home folder that doesn't pass
// through a symlink it kept, no setuid, setgid or sticky bit, and no
// owner. Paths under skips (homeSkips) are left out, and so is the home
// folder's own entry. The stream is the VM's, so nothing in it is
// trusted: it's never unpacked on the host.
func copyHome(r io.Reader, w io.Writer, skips []string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(w)
	tr, tw := tar.NewReader(zr), tar.NewWriter(zw)
	kept := map[string]byte{} // by path: its type
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		name, ok := homePath(h.Name)
		if !ok || skipped(name, skips) || throughLink(name, kept) || !keepType(h, kept) {
			continue
		}
		out := &tar.Header{
			Typeflag: h.Typeflag, Name: name, Mode: h.Mode & 0o777, ModTime: h.ModTime, Format: tar.FormatPAX,
		}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
		case tar.TypeReg:
			out.Size = h.Size
		case tar.TypeSymlink:
			out.Linkname = h.Linkname
		case tar.TypeLink:
			out.Linkname, _ = homePath(h.Linkname)
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		// Its size as its header has it, which the tar writer holds it to:
		// the home folder's size is the VM's (design.md).
		if h.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return err
			}
		}
		kept[name] = h.Typeflag
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// homePath is a tar entry's path relative to the home folder, cleaned,
// and whether it's one inside it: not the folder itself, not absolute, no
// .., and no control characters.
func homePath(name string) (string, bool) {
	p := path.Clean(strings.TrimPrefix(name, "./"))
	switch {
	case p == "." || p == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../"):
		return "", false
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", false
	}
	return p, true
}

// skipped reports whether p is one of skips or inside one.
func skipped(p string, skips []string) bool {
	return slices.ContainsFunc(skips, func(s string) bool { return paths.Within(p, s) })
}

// throughLink reports whether p is inside a symlink kept before it,
// which tar would follow out of the home folder.
func throughLink(p string, kept map[string]byte) bool {
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if kept[d] == tar.TypeSymlink {
			return true
		}
	}
	return false
}

// keepType reports whether h is of a type a home folder keeps: a file, a
// folder or a symlink, or a hard link to a file kept before it.
func keepType(h *tar.Header, kept map[string]byte) bool {
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		return true
	case tar.TypeLink:
		target, ok := homePath(h.Linkname)
		return ok && kept[target] == tar.TypeReg
	}
	return false
}

// saveHome saves the running VM's home folder (homeOut) to its folder
// (paths.VM.Home), through copyHome, with what its remake doesn't keep
// left out. It's written next to it and renamed into place once
// complete, so a failed save leaves none; failed is the error then.
func (e remaking) saveHome(ctx context.Context, t *target, failed error) error {
	vm := t.f.Name
	file := e.Dirs.VM(vm).Home()
	out, err := os.CreateTemp(filepath.Dir(file), ".home-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	done := e.AddCleanup(func(context.Context) { _ = os.Remove(tmp) })
	defer func() {
		_ = os.Remove(tmp)
		done()
	}()
	e.Info(msg.SavingHome(vm))
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := copyHome(pr, out, homeSkips(e.Dirs, tools.Names(t.st)))
		if err == nil {
			// tar's padding after its end, which the VM still sends.
			_, err = io.Copy(io.Discard, pr)
		}
		_ = pr.CloseWithError(err)
		copied <- err
	}()
	err = t.exec(ctx, []string{"bash", "-c", homeOut}, backend.ExecOpts{Stdout: pw, Stderr: e.vmLog()})
	_ = pw.CloseWithError(err)
	cerr := <-copied
	if err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = io.WriteString(e.LogWriter(), "devvm: saving the home folder: "+err.Error()+"\n")
		return failed
	}
	return os.Rename(tmp, file)
}

// restoreHome puts the VM's saved home folder into its new disk's
// (homeIn), once it runs. A VM with none saved has nothing put in.
func (e remaking) restoreHome(ctx context.Context, t *target) error {
	vm := t.f.Name
	f, err := os.Open(e.Dirs.VM(vm).Home())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	e.Info(msg.RestoringHome(vm))
	log := e.vmLog()
	err = t.exec(ctx, []string{"bash", "-c", homeIn}, backend.ExecOpts{Stdin: f, Stdout: log, Stderr: log})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return msg.RemakeRestoreFailed(vm)
	}
	return nil
}
