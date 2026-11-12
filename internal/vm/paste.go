package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/settings"
)

// Paste is devvm paste: an image, a PDF or one copied file, from this machine's
// clipboard into the folder's VM's paste folder (config.toml's paste-dir),
// its path in the VM then copied to the clipboard for the agent; devvm
// paste rm, which deletes what devvm paste left there; and the launch's
// offer to do the same (offerRm). Its connections to the VM carry no SSH
// agent, and the only thing it reads back from the VM is a count. It's a
// type of its own because Lifecycle and Net are at the structure test's
// limit of methods.
type Paste struct{ *Env }

// PasteSent is what devvm paste sent: the VM, the file's path in it, which
// devvm chose on this machine, its kind (image, pdf or file), an image's
// or a PDF's type (image/png…, application/pdf, "" for a file), a copied
// file's path on this machine ("" otherwise), its size, and whether the
// path reached the clipboard. When you said no to sending a file, it has
// only the VM, the kind and the source, and no Path: nothing was sent.
type PasteSent struct {
	VM, Path string
	Kind     string
	Type     string
	Source   string
	Bytes    int64
	Copied   bool
}

// The kinds of a PasteSent.
const (
	PasteImage = "image"
	PastePDF   = "pdf"
	PasteFile  = "file"
)

// pasteMax is the most devvm paste sends, an image, a PDF or a file.
const pasteMax = 64 << 20

// pasteMaxText is how a message says pasteMax: 64 MiB.
func pasteMaxText() string { return strconv.Itoa(pasteMax>>20) + " MiB" }

// pasteCountMax bounds what the VM prints for a count.
const pasteCountMax = 32

// pastePayload is what one devvm paste sends, chosen once from the
// clipboard: the bytes, the file's name in the VM, which devvm picks from
// a fixed set of characters, its kind, an image's type, a copied file's
// path on this machine, and how many links opening it followed (the
// system's, platform.OpenNoLinks), which make its path not where it is.
type pastePayload struct {
	data       []byte
	name       string
	kind, mime string
	source     string
	links      int
}

// Send reads the clipboard and puts what's there into the folder's VM,
// which must be running, as a new file in its paste folder, then copies
// the file's path. It reads the clipboard before it touches the VM, so an
// empty one fails fast. A copied file is sent only after a question
// showing its path on this machine (confirmFile), because the clipboard
// can name any file of yours; a no sends nothing and returns a PasteSent
// without a Path. A copy that fails is a warning, as the file is already in
// the VM, and the path is shown.
func (e Paste) Send(ctx context.Context, sendFile bool) (PasteSent, error) {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return PasteSent{}, err
	}
	vm := t.f.Name
	if t.in.Status != backend.StatusRunning {
		return PasteSent{}, msg.VMNotRunning(vm)
	}
	dir, err := settings.PasteDir(e.Dirs)
	if err != nil {
		return PasteSent{}, err
	}
	clip, err := e.ReadClip(ctx, pasteMax)
	if err != nil {
		return PasteSent{}, clipError(err)
	}
	p, err := pastePayloadOf(clip, time.Now())
	if err != nil {
		return PasteSent{}, err
	}
	if p.kind == PasteFile {
		yes, err := e.confirmFile(t, p, sendFile)
		if err != nil {
			return PasteSent{}, err
		}
		if !yes {
			return PasteSent{VM: vm, Kind: p.kind, Source: p.source}, nil
		}
	}
	path := dir + "/" + p.name
	if err := e.run(ctx, t, []string{"put", dir, p.name}, bytes.NewReader(p.data), nil); err != nil {
		if ctx.Err() != nil {
			return PasteSent{}, ctx.Err()
		}
		return PasteSent{}, backend.Logged(msg.PasteNotSent(path, vm))
	}
	sent := PasteSent{VM: vm, Path: path, Kind: p.kind, Type: p.mime, Source: p.source, Bytes: int64(len(p.data))}
	if err := e.CopyText(ctx, path); err != nil {
		e.Warn(msg.PasteCopyFailed(path).Because(err))
	} else {
		sent.Copied = true
	}
	return sent, nil
}

// confirmFile asks, [y/N], before the copied file p goes into t's VM,
// showing its path. sendFile (--send-file) answers for a file in the
// folder the VM mounts, which it can read already, reached through no
// link (sendFileAnswers). A file anywhere else, one reached through a
// link, or any file for a VM with a clone of its own, is asked about
// whatever the flag says, and where devvm can't ask it's refused, naming
// the file (paste_file_outside, with the folder, or
// paste_file_through_link), so a script or a hotkey with --send-file
// can't send a file it never showed.
func (e Paste) confirmFile(t *target, p pastePayload, sendFile bool) (bool, error) {
	ask := msg.PasteFileAsk(p.source, t.f.Name)
	switch {
	case sendFileAnswers(isClone(t.st), p.links, t.f.Dir, p.source) && sendFile:
		return true, nil
	case sendFileAnswers(isClone(t.st), p.links, t.f.Dir, p.source):
		return e.Ask(ask.AnsweredBy("--send-file"))
	case p.links > 0:
		return e.Ask(ask.RefusedWith(msg.PasteFileThroughLink(p.source)))
	}
	return e.Ask(ask.RefusedWith(msg.PasteFileOutside(t.f.Dir, p.source)))
}

// sendFileAnswers reports whether --send-file answers the question for
// file, opened through links links (pastePayload): only for a file below
// dir, the folder the VM mounts, reached through no link at all, and
// never for a clone VM, which mounts nothing of the folder. A link
// OpenNoLinks follows is root's, but one below the folder, such as a root
// container made on a Linux host, can lead anywhere, so a path that went
// through one isn't where it says.
func sendFileAnswers(clone bool, links int, dir, file string) bool {
	return !clone && links == 0 && inFolder(dir, file)
}

// inFolder reports whether file, a clean absolute path opened as it's
// named, through no link but the system's (pastePayloadOf), is below dir,
// a folder's physical path. Comparing the paths is enough: the open
// followed no link a VM or you could make, so the file is where its path
// says. A path through a system link, such as /tmp for /private/tmp,
// isn't below the physical folder, and is asked about.
func inFolder(dir, file string) bool {
	rel, err := filepath.Rel(dir, file)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// Rm deletes what devvm paste left in the folder's VM's paste folder,
// which must be running: it counts first, asks unless force ([y/N],
// answered by --force), then deletes and says how many. With nothing to
// delete it says so.
func (e Paste) Rm(ctx context.Context, force bool) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm := t.f.Name
	if t.in.Status != backend.StatusRunning {
		return msg.VMNotRunning(vm)
	}
	dir, err := settings.PasteDir(e.Dirs)
	if err != nil {
		return err
	}
	n, err := e.numberFrom(ctx, t, "count", dir)
	if err != nil {
		return err
	}
	if n == 0 {
		e.Say(msg.PasteRmNone(dir, vm))
		return nil
	}
	if !force {
		yes, err := e.Ask(msg.PasteRmAsk(n, dir, vm).AnsweredBy("--force"))
		if err != nil || !yes {
			return err
		}
	}
	return e.rm(ctx, t, dir)
}

// offerRm is a launch's offer, once it started t's VM, which was stopped,
// to delete what devvm paste left in its paste folder: it asks, [y/N],
// when there's anything, and where it can't ask it keeps them and says
// nothing, as devvm paste rm deletes them later. A count that fails is
// in the run log, and a delete that fails after the yes is a warning
// (paste_rm_not_done); the launch goes on either way.
func (e Paste) offerRm(ctx context.Context, t *target) error {
	dir, err := settings.PasteDir(e.Dirs)
	if err != nil {
		return err
	}
	n, err := e.numberFrom(ctx, t, "count", dir)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return nil //nolint:nilerr // the count's failure is in the run log; the launch goes on without the offer
	case n == 0:
		return nil
	}
	yes, err := e.Ask(msg.PasteLaunchAsk(n, dir, t.f.Name).AnsweredBy("devvm paste rm"))
	if err != nil {
		return nil //nolint:nilerr // the question's refusal: the files stay for devvm paste rm
	}
	if !yes {
		return nil
	}
	if err := e.rm(ctx, t, dir); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.Warn(msg.PasteRmNotDone(dir, t.f.Name))
	}
	return nil
}

// rm deletes what devvm paste left in dir in t's VM and says how many.
func (e Paste) rm(ctx context.Context, t *target, dir string) error {
	n, err := e.numberFrom(ctx, t, "rm", dir)
	if err != nil {
		return err
	}
	e.Info(msg.PasteRmDone(n, dir, t.f.Name))
	return nil
}

// numberFrom runs paste.sh's cmd on dir in t's VM, count (how many files
// devvm paste left) or rm (deletes them, and says how many), and reads
// the number it prints, the one thing devvm reads back from the VM: a
// whole number within pasteCountMax bytes, or the command's failure
// (paste_count_failed, paste_rm_not_done).
func (e Paste) numberFrom(ctx context.Context, t *target, cmd, dir string) (int, error) {
	out := &bounded{}
	err := e.run(ctx, t, []string{cmd, dir}, nil, out)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	n := -1
	if err == nil && !out.over {
		n, err = strconv.Atoi(strings.TrimSpace(out.String()))
	}
	if err != nil || n < 0 {
		m := msg.PasteCountFailed(dir, t.f.Name)
		if cmd == "rm" {
			m = msg.PasteRmNotDone(dir, t.f.Name)
		}
		return 0, backend.Logged(m)
	}
	return n, nil
}

// run runs paste.sh (assets/guest/paste.sh) in t's running VM with args
// after its command name, the script itself an argument of sh -c, so
// nothing of it is left on disk, with stdin as its input and out, when
// it's given, keeping its output, at most pasteCountMax bytes. The VM's
// stderr goes to the run log, at most vmLogMax bytes. Past either cap the
// command is stopped, so a VM that prints without end can't keep devvm
// reading, and so is one past its time (pasteWait), so a VM that never
// answers can't keep devvm waiting. The connection carries no SSH agent:
// it's the backend's Exec as it is, never target.exec.
func (e Paste) run(ctx context.Context, t *target, args []string, stdin io.Reader, out *bounded) error {
	script, err := fs.ReadFile(e.Assets, "guest/paste.sh")
	if err != nil {
		return err
	}
	run, cancel := context.WithTimeout(ctx, e.pasteWait(args[0]))
	defer cancel()
	if out == nil {
		out = &bounded{}
	}
	out.max, out.cancel = pasteCountMax, cancel
	log := &bounded{to: e.LogWriter(), max: vmLogMax, cancel: cancel}
	return t.b.Exec(run, t.f.Name, pasteArgv(string(script), args), backend.ExecOpts{Stdin: stdin, Stdout: out, Stderr: log})
}

// How long paste.sh may take in the VM: put, which takes up to pasteMax
// over the VM's ssh, and count and rm, which list one folder.
const (
	pastePutWait   = 10 * time.Minute
	pasteCountWait = 2 * time.Minute
)

// pasteWait is how long paste.sh's cmd may take: Env.PasteWait when a
// test build sets it, else pastePutWait for put and pasteCountWait for
// count and rm.
func (e Paste) pasteWait(cmd string) time.Duration {
	switch {
	case e.PasteWait > 0:
		return e.PasteWait
	case cmd == "put":
		return pastePutWait
	}
	return pasteCountWait
}

// pasteArgv is the command that runs paste.sh, the script, with args
// after its command name: sh -c's script, named devvm-paste.
func pasteArgv(script string, args []string) []string {
	return append([]string{"sh", "-c", script, "devvm-paste"}, args...)
}

// pastePayloadOf is what to send for clip, named at now: an image or a
// PDF as it came, or a copied file, which may be in a project folder, which its VM
// writes. The path is cleaned, so a folder's trailing slash goes, and
// opened as named, following no link but root's (platform.OpenNoLinks:
// /tmp on macOS is one). Nothing of the path is resolved first, because
// a VM can plant a link in the project folder before the copy as well as
// swap one in after it, and either, say to a key of yours, fails rather
// than sends what it points to. The file opened is checked, as every
// file a VM can write is read, by internal/safefile: a regular file
// within pasteMax.
func pastePayloadOf(clip platform.Clip, now time.Time) (pastePayload, error) {
	stem := pasteStem(now)
	if clip.File == "" {
		kind := PasteImage
		if clip.Type == platform.TypePDF {
			kind = PastePDF
		}
		return pastePayload{data: clip.Data, name: stem + "." + platform.DataExt(clip.Type), kind: kind, mime: clip.Type}, nil
	}
	file := filepath.Clean(clip.File)
	opened, links, err := platform.OpenNoLinks(file)
	if errors.Is(err, platform.ErrLinkOnPath) {
		return pastePayload{}, msg.PasteFileLink(file)
	}
	var f *safefile.File
	if err == nil {
		f, err = safefile.Checked(opened, safefile.Opts{})
	}
	if errors.Is(err, safefile.ErrNotRegular) {
		return pastePayload{}, msg.PasteNotFile(file)
	}
	if err != nil {
		return pastePayload{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := f.ReadAll(pasteMax)
	switch {
	case errors.Is(err, safefile.ErrTooLong):
		return pastePayload{}, msg.PasteTooLarge(pasteMaxText())
	case err != nil:
		return pastePayload{}, err
	}
	return pastePayload{data: data, name: stem + pasteExt(filepath.Base(file)), kind: PasteFile, source: file, links: links}, nil
}

// pasteStem is the start of a pasted file's name: paste-YYYYMMDD-HHMMSS
// and 8 hex digits at random, so two pastes in one second differ and the
// name is from a fixed set of characters.
func pasteStem(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "paste-" + now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// pasteExtMax is the longest extension of a copied file's a pasted file
// keeps.
const pasteExtMax = 16

// pasteExt is the end of a pasted file's name for a copied file named
// base: its extension, with its dot, when it's letters and digits alone,
// at most pasteExtMax of them, else nothing. Nothing else of the name is
// kept, because the path goes onto your clipboard, and the name of a file
// in the project folder is the VM's to choose.
func pasteExt(base string) string {
	ext := strings.TrimPrefix(filepath.Ext(base), ".")
	if ext == "" || len(ext) > pasteExtMax {
		return ""
	}
	for _, c := range []byte(ext) {
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum {
			return ""
		}
	}
	return "." + ext
}

// clipError is the message for what reading the clipboard failed with
// (platform.ReadClip's errors), or err as it is.
func clipError(err error) error {
	var many *platform.ClipManyFiles
	var missing *platform.ClipToolMissing
	var failed *platform.ClipToolFailed
	var slow *platform.ClipToolSlow
	switch {
	case errors.As(err, &many):
		return msg.PasteManyFiles(many.N)
	case errors.As(err, &missing):
		return msg.PasteNeedsTool(missing.Tool)
	case errors.As(err, &slow):
		return msg.PasteToolSlow(int(slow.Wait / time.Second)).Because(err)
	case errors.As(err, &failed) && failed.Tool == "osascript":
		return msg.PasteOsascriptFailed(msg.Printable(strings.TrimSpace(failed.Stderr))).Because(err)
	case errors.As(err, &failed):
		return msg.PasteToolFailed(msg.Printable(strings.TrimSpace(failed.Stderr)), failed.Tool).Because(err)
	case errors.Is(err, platform.ErrClipEmpty):
		return msg.PasteNothing()
	case errors.Is(err, platform.ErrClipFileGone):
		return msg.PasteFileGone()
	case errors.Is(err, platform.ErrClipTooLarge):
		return msg.PasteTooLarge(pasteMaxText())
	case errors.Is(err, platform.ErrClipNoSession):
		return msg.PasteNoSession()
	case errors.Is(err, errors.ErrUnsupported):
		return msg.PasteUnsupported()
	}
	return err
}
