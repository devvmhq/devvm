package vm

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
)

// A pasted file's name: paste-YYYYMMDD-HHMMSS-8hex, then the image's
// extension, or the copied file's own extension when it's letters and
// digits alone, at most 16 of them, and nothing else of its name.
func TestPasteNames(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 34, 56, 0, time.Local)
	stem := regexp.MustCompile(`^paste-20261109-123456-[0-9a-f]{8}$`)
	if s := pasteStem(now); !stem.MatchString(s) {
		t.Errorf("stem %q", s)
	}
	if a, b := pasteStem(now), pasteStem(now); a == b {
		t.Errorf("two stems in one second are the same: %s", a)
	}
	for in, want := range map[string]string{
		"shot.png": ".png", "my shot (1).png": ".png", "résumé.pdf": ".pdf", "": "", "archive.tar.gz": ".gz",
		"a;rm -rf /.txt": ".txt", "$HOME": "", "..": "", "-n": "", "Makefile": "", ".bashrc": ".bashrc",
		"x.p$g": "", "x.ünï": "", "x." + strings.Repeat("a", 17): "", "x." + strings.Repeat("a", 16): "." + strings.Repeat("a", 16),
	} {
		if got := pasteExt(in); got != want {
			t.Errorf("pasteExt(%q) = %q, want %q", in, got, want)
		}
	}
	p, err := pastePayloadOf(platform.Clip{Data: []byte("png"), Type: "image/png"}, now)
	if err != nil || !strings.HasSuffix(p.name, ".png") || !stem.MatchString(strings.TrimSuffix(p.name, ".png")) || p.kind != PasteImage || p.mime != "image/png" {
		t.Errorf("an image: %+v, %v", p, err)
	}
	p, err = pastePayloadOf(platform.Clip{Data: []byte("%PDF-1.4"), Type: platform.TypePDF}, now)
	if err != nil || !stem.MatchString(strings.TrimSuffix(p.name, ".pdf")) || p.kind != PastePDF || p.mime != platform.TypePDF || string(p.data) != "%PDF-1.4" {
		t.Errorf("a PDF: %+v, %v", p, err)
	}
}

// paste.sh's put may take 10 minutes, its count and rm 2, and a test
// build's PasteWait stands in for both.
func TestPasteWait(t *testing.T) {
	e := Paste{Env: &Env{}}
	if e.pasteWait("put") != 10*time.Minute || e.pasteWait("count") != 2*time.Minute || e.pasteWait("rm") != 2*time.Minute {
		t.Errorf("put %s, count %s, rm %s", e.pasteWait("put"), e.pasteWait("count"), e.pasteWait("rm"))
	}
	e.PasteWait = time.Second
	if e.pasteWait("put") != time.Second || e.pasteWait("rm") != time.Second {
		t.Errorf("with PasteWait: put %s, rm %s", e.pasteWait("put"), e.pasteWait("rm"))
	}
}

// --send-file answers for a file opened in the folder through no link:
// never for one a followed link reached, even one whose path is in the
// folder, as a root-owned link below it can lead anywhere, and never for
// a clone VM, which mounts nothing of the folder.
func TestSendFileAnswers(t *testing.T) {
	for _, c := range []struct {
		name  string
		clone bool
		links int
		file  string
		want  bool
	}{
		{"in the folder", false, 0, "/home/me/web/shot.png", true},
		{"in the folder through a followed link", false, 1, "/home/me/web/data/link/shot.png", false},
		{"outside", false, 0, "/home/me/.ssh/id_ed25519", false},
		{"a clone VM's", true, 0, "/home/me/web/shot.png", false},
	} {
		if got := sendFileAnswers(c.clone, c.links, "/home/me/web", c.file); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A file is in the folder only below it: not the folder itself, not a
// sibling whose name starts the same, and not a path through a system
// link to it.
func TestInFolder(t *testing.T) {
	for _, c := range []struct {
		file string
		want bool
	}{
		{"/Users/me/web/shot.png", true},
		{"/Users/me/web/docs/a b.pdf", true},
		{"/Users/me/web", false},
		{"/Users/me/webapp/x.png", false},
		{"/Users/me/.ssh/id_ed25519", false},
		{"/etc/hosts", false},
		{"/tmp/web/shot.png", false},
	} {
		if got := inFolder("/Users/me/web", c.file); got != c.want {
			t.Errorf("inFolder(%q) = %v, want %v", c.file, got, c.want)
		}
	}
}

// ids reports whether err is the message want.
func ids(err error, want msg.M) bool {
	var m msg.M
	return errors.As(err, &m) && m.ID() == want.ID()
}

// A copied file is read as named, and refused when it's a link, when a
// folder on its way is one (planted before the copy or swapped in after
// it), when it's a link that a folder's trailing slash hid, when it
// isn't a regular file, when it's past the cap, and when it's gone.
func TestPastePayloadOfFile(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	project := func(parts ...string) string { return filepath.Join(append([]string{dir, "project"}, parts...)...) }
	must(os.MkdirAll(project("shots"), 0o700))
	must(os.WriteFile(project("shots", "shot.png"), []byte("png"), 0o600))
	// A key of yours, which the VM aims three links in the project folder at.
	must(os.MkdirAll(filepath.Join(dir, "home", ".ssh"), 0o700))
	must(os.WriteFile(filepath.Join(dir, "home", ".ssh", "id_ed25519"), []byte("secret"), 0o600))
	must(os.Symlink(filepath.Join(dir, "home", ".ssh", "id_ed25519"), project("shots", "planted.png")))
	must(os.Symlink(filepath.Join(dir, "home", ".ssh"), project("docs")))
	must(os.Symlink(filepath.Join(dir, "home", ".ssh"), project("id_ed25519")))
	now := time.Now()

	p, err := pastePayloadOf(platform.Clip{File: project("shots", "shot.png")}, now)
	if err != nil || string(p.data) != "png" || !regexp.MustCompile(`^paste-[0-9]{8}-[0-9]{6}-[0-9a-f]{8}\.png$`).MatchString(p.name) || p.kind != PasteFile || p.mime != "" {
		t.Errorf("a file: %+v, %v", p, err)
	}
	for what, file := range map[string]string{
		"a link in the file's place":                                   project("shots", "planted.png"),
		"a folder's link planted before the copy":                      project("docs", "id_ed25519"),
		"a folder reference, with its trailing slash, at a link":       project("id_ed25519") + "/",
		"the link a folder reference's trailing slash was on, cleaned": project("id_ed25519"),
	} {
		p, err := pastePayloadOf(platform.Clip{File: file}, now)
		if !ids(err, msg.PasteFileLink("")) || p.data != nil {
			t.Errorf("%s: %+v, %v", what, p, err)
		}
	}
	for what, file := range map[string]string{
		"a folder":                       project("shots"),
		"a folder with a trailing slash": project("shots") + "/",
	} {
		if p, err := pastePayloadOf(platform.Clip{File: file}, now); !ids(err, msg.PasteNotFile("")) || p.data != nil {
			t.Errorf("%s: %+v, %v", what, p, err)
		}
	}
	if _, err := pastePayloadOf(platform.Clip{File: project("shots", "gone.png")}, now); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := pastePayloadOf(platform.Clip{File: "project/shots/shot.png"}, now); err == nil {
		t.Error("a relative path: sent")
	}
	// The VM swaps the folder for a link to one of yours between the copy and devvm paste.
	must(os.Rename(project("shots"), project("shots.was")))
	must(os.MkdirAll(filepath.Join(dir, "yours"), 0o700))
	must(os.WriteFile(filepath.Join(dir, "yours", "shot.png"), []byte("secret"), 0o600))
	must(os.Symlink(filepath.Join(dir, "yours"), project("shots")))
	if p, err := pastePayloadOf(platform.Clip{File: project("shots", "shot.png")}, now); !ids(err, msg.PasteFileLink("")) || p.data != nil {
		t.Errorf("a folder swapped for a link after the copy: %+v, %v", p, err)
	}
}

// sync.sh's paste folder block makes a missing DEVVM_PASTE_DIR, mode 700,
// and where neither the user nor sudo can make it, under a file say, it
// warns and the setup goes on, as it does for a link in its place, so a
// paste-dir that can't be made never fails a launch.
func TestSyncPasteDir(t *testing.T) {
	block := between(t, "guest/sync.sh", `if [[ -n "${DEVVM_PASTE_DIR:-}" ]]; then`, "fi")
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // the test's stand-in for sudo, to run
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "file"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	run := func(dir string) (string, error) {
		t.Helper()
		script := "set -euo pipefail\nif [[ -n \"${DEVVM_PASTE_DIR:-}\" ]]; then" + block + "\nfi\necho went on"
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script) //nolint:gosec // sync.sh's own block, on the test's files
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DEVVM_PASTE_DIR="+dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run(filepath.Join(root, "pastes"))
	if err != nil || !strings.HasSuffix(out, "went on\n") || strings.Contains(out, "devvm:") {
		t.Errorf("a folder to make: %v\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(root, "pastes")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the folder made: %v, %v", fi, err)
	}
	for want, dir := range map[string]string{
		"can't be made":      filepath.Join(root, "file", "pastes"),
		"is a symbolic link": filepath.Join(root, "link"),
		"isn't a folder":     filepath.Join(root, "file"),
	} {
		out, err := run(dir)
		if err != nil || !strings.Contains(out, "devvm: "+dir+" "+want) || !strings.HasSuffix(out, "went on\n") {
			t.Errorf("%s: %v\n%s", want, err, out)
		}
	}
}

// pasteScript runs paste.sh (assets/guest/paste.sh) with sh in dir, as
// the VM runs it, with args after its command name and stdin as its
// input, and returns what it printed and its failure.
func pasteScript(t *testing.T, dir string, stdin string, args ...string) (string, string, error) {
	t.Helper()
	script, err := fs.ReadFile(assets.FS, "guest/paste.sh")
	if err != nil {
		t.Fatal(err)
	}
	argv := pasteArgv(string(script), args)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...) //nolint:gosec // devvm's own script, on the test's files
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err = cmd.Run()
	return out.String(), errs.String(), err
}

// paste.sh's put writes stdin to DIR/NAME, mode 600, making DIR mode 700,
// and refuses a DIR that's a link. count and rm see devvm's files and
// .part leftovers alone: not a file of the user's in DIR, not one whose
// name has a newline in it past devvm's prefix, which read as two lines
// would have rm delete a file of that second line's name in the working
// directory, and nothing in the working directory.
func TestPasteScript(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	dir := filepath.Join(root, "pastes")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(cwd, 0o700))
	must(os.WriteFile(filepath.Join(cwd, ".bashrc"), []byte("mine"), 0o600))
	must(os.Symlink(cwd, filepath.Join(root, "linked")))

	if _, errs, err := pasteScript(t, cwd, "image", "put", filepath.Join(root, "linked"), "paste-20261109-123456-deadbeef.png"); err == nil || !strings.Contains(errs, "symbolic link") {
		t.Errorf("put into a linked DIR: %v, %q", err, errs)
	}
	if _, errs, err := pasteScript(t, cwd, "image", "put", dir, "paste-20261109-123456-deadbeef.png"); err != nil {
		t.Fatalf("put: %v, %q", err, errs)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("DIR: %v, %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "paste-20261109-123456-deadbeef.png")); err != nil || string(b) != "image" { //nolint:gosec // the test's own file
		t.Errorf("the file put: %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "paste-20261109-123456-deadbeef.png")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode: %v, %v", fi, err)
	}
	kept := []string{"notes.txt", "paste-20261109-123456-deadbeef\n.bashrc", "paste-20261109-123456-deadbeef .bashrc"}
	gone := []string{"paste-20261109-123456-deadbeef.png", "paste-20261109-123456-cafef00d-doc.txt.part"}
	for _, name := range append(kept, gone[1:]...) {
		must(os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	if out, errs, err := pasteScript(t, cwd, "", "count", dir); err != nil || out != "2\n" {
		t.Errorf("count: %q, %v, %q", out, err, errs)
	}
	if out, errs, err := pasteScript(t, cwd, "", "rm", dir); err != nil || out != "2\n" {
		t.Errorf("rm: %q, %v, %q", out, err, errs)
	}
	for _, name := range gone {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: still there, %v", name, err)
		}
	}
	for _, name := range append(kept, "../cwd/.bashrc") {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	if out, _, err := pasteScript(t, cwd, "", "count", filepath.Join(root, "none")); err != nil || out != "0\n" {
		t.Errorf("count of a missing DIR: %q, %v", out, err)
	}
}

// Reading the clipboard's failures each have their message, and another
// error passes as it is.
func TestClipError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want msg.M
	}{
		{platform.ErrClipEmpty, msg.PasteNothing()},
		{platform.ErrClipFileGone, msg.PasteFileGone()},
		{&platform.ClipManyFiles{N: 3}, msg.PasteManyFiles(3)},
		{&platform.ClipToolMissing{Tool: "wl-paste"}, msg.PasteNeedsTool("wl-paste")},
		{&platform.ClipToolFailed{Tool: "wl-paste", Stderr: "Failed to connect to a Wayland server\n"}, msg.PasteToolFailed("Failed to connect to a Wayland server", "wl-paste")},
		{platform.ErrClipTooLarge, msg.PasteTooLarge(pasteMaxText())},
		{platform.ErrClipNoSession, msg.PasteNoSession()},
		{&platform.ClipToolSlow{Tool: "xclip", Wait: 30 * time.Second}, msg.PasteToolSlow(30)},
		{&platform.ClipToolFailed{Tool: "osascript", Stderr: "execution error: Error: Connection invalid (-2700)\n"}, msg.PasteOsascriptFailed("execution error: Error: Connection invalid (-2700)")},
		{errors.ErrUnsupported, msg.PasteUnsupported()},
	} {
		var m msg.M
		if err := clipError(c.err); !errors.As(err, &m) || m.Names() != c.want.Names() {
			t.Errorf("%v: %v, want %s", c.err, err, c.want.Names())
		}
	}
	other := errors.New("other")
	if err := clipError(other); err != other { //nolint:errorlint // the same error, as it was
		t.Errorf("another error: %v", err)
	}
}
