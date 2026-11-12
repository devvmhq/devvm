package platform

import (
	"bufio"
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Clipboard is this machine's clipboard, which ReadClip reads and
// CopyText writes with the system's clipboard tools.
type Clipboard struct {
	// Tools is the folder the clipboard tools are run from in place of
	// the system's: a test build's stand-ins (DEVVM_CLIP_TOOLS), "" for
	// the system's own.
	Tools string
}

// Clip is what devvm paste takes from the clipboard: an image's or a
// PDF's bytes and type, or one copied file's path. Nothing else is read,
// so text on the clipboard, such as a password, never reaches devvm.
type Clip struct {
	Data []byte
	Type string // application/pdf, image/png, image/jpeg, image/gif or image/webp, with Data
	File string // a copied file's path, instead of Data
}

// TypePDF is a PDF's type, which devvm sends as it is, never as an image.
const TypePDF = "application/pdf"

// ReadClip's errors, which its callers turn into messages.
var (
	// ErrClipEmpty: nothing devvm can send is on the clipboard, neither an
	// image nor a copied file.
	ErrClipEmpty = errors.New("no image, PDF or copied file on the clipboard")
	// ErrClipManyFiles: several files are on the clipboard, and devvm
	// paste sends one. The error carries how many (ClipManyFiles).
	ErrClipManyFiles = errors.New("several files on the clipboard")
	// ErrClipFileGone: on macOS, the copied file is gone, so its path
	// can't be found, and Finder's icon beside it isn't sent instead.
	ErrClipFileGone = errors.New("the copied file is gone")
	// ErrClipTooLarge: what's on the clipboard is over the cap.
	ErrClipTooLarge = errors.New("the clipboard's contents are over the cap")
	// ErrClipNoSession: on Linux, neither WAYLAND_DISPLAY nor DISPLAY is
	// set, so there's no clipboard to read.
	ErrClipNoSession = errors.New("no Wayland or X11 session")
)

// ClipManyFiles is ErrClipManyFiles with how many files were copied.
type ClipManyFiles struct{ N int }

func (e *ClipManyFiles) Error() string { return ErrClipManyFiles.Error() }

// Is makes errors.Is(err, ErrClipManyFiles) true for it.
func (e *ClipManyFiles) Is(err error) bool { return err == ErrClipManyFiles }

// ClipToolMissing is ReadClip's or CopyText's error when the clipboard
// tool it runs isn't there: not on PATH, or not at its path.
type ClipToolMissing struct{ Tool string }

func (e *ClipToolMissing) Error() string { return e.Tool + " isn't installed" }

// ClipToolFailed is ReadClip's error when the clipboard tool failed to
// list the clipboard's types for a reason other than an empty clipboard,
// such as wl-paste with no compositor at WAYLAND_DISPLAY, with what it
// said on stderr, bounded.
type ClipToolFailed struct{ Tool, Stderr string }

func (e *ClipToolFailed) Error() string { return e.Tool + " failed: " + strings.TrimSpace(e.Stderr) }

// ClipToolSlow is ReadClip's or CopyText's error when the clipboard tool
// didn't finish within Wait, as when the app holding the clipboard
// doesn't answer, and was stopped.
type ClipToolSlow struct {
	Tool string
	Wait time.Duration
}

func (e *ClipToolSlow) Error() string { return e.Tool + " didn't finish within " + e.Wait.String() }

// wlPasteEmpty and xclipEmpty are what wl-paste and xclip say on stderr
// when the clipboard is empty or doesn't offer the type asked for, which
// is how they report it: a failure that says anything else, such as a
// display or a compositor that can't be reached, is the tool's own
// (ClipToolFailed). xclip's are its current words, "There is no owner"
// and "cannot convert", and older releases' "not available".
func wlPasteEmpty() []string { return []string{"Nothing is copied", "No selection"} }

func xclipEmpty() []string {
	return []string{"There is no owner", "cannot convert", "not available"}
}

// saysEmpty reports whether stderr, a failed clipboard tool's, says one
// of words, its words for an empty clipboard.
func saysEmpty(words []string, stderr string) bool {
	for _, w := range words {
		if strings.Contains(stderr, w) {
			return true
		}
	}
	return false
}

// The clipboard types devvm reads besides a PDF (TypePDF, first): a
// copied file's reference, because Finder and the Linux file managers put
// the file's icon on the clipboard as an image beside it, then the image
// types, PNG first.
const (
	typeURIList = "text/uri-list"
	typeGnome   = "x-special/gnome-copied-files"
)

// dataType is a type of data devvm sends, with its file extension.
type dataType struct{ mime, ext string }

// dataTypes are the types of data devvm sends, in the order it prefers
// them: a PDF first, as it is, because a PDF viewer puts a picture of the
// selection beside it, then the image types.
func dataTypes() []dataType {
	return []dataType{{TypePDF, "pdf"}, {"image/png", "png"}, {"image/jpeg", "jpg"}, {"image/gif", "gif"}, {"image/webp", "webp"}}
}

// DataExt is the file extension for data of type mime, "" for a type
// devvm doesn't send.
func DataExt(mime string) string {
	for _, t := range dataTypes() {
		if t.mime == mime {
			return t.ext
		}
	}
	return ""
}

// chooseTypes picks, from the types a clipboard offers (one per line, as
// wl-paste --list-types and xclip's TARGETS print them), those devvm
// reads, in the order it tries them: a PDF, as a PDF viewer may name a
// temporary file of its own beside it, then a file reference, then the
// image types it prefers (dataTypes). None means nothing devvm can send
// is there.
func chooseTypes(listed []byte) []string {
	var have []string
	for _, line := range strings.Split(string(listed), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			have = append(have, t)
		}
	}
	var picked []string
	if slices.Contains(have, TypePDF) {
		picked = append(picked, TypePDF)
	}
	for _, t := range []string{typeURIList, typeGnome} {
		if slices.Contains(have, t) {
			picked = append(picked, t)
		}
	}
	for _, t := range dataTypes() {
		if t.mime != TypePDF && slices.Contains(have, t.mime) {
			picked = append(picked, t.mime)
		}
	}
	return picked
}

// uriListPaths are the paths of the file: URIs in a text/uri-list (RFC
// 2483: one URI per line, CRLF or LF, # lines comments), or in GNOME's
// x-special/gnome-copied-files, whose first line is copy or cut. A URI
// that isn't a file on this machine, such as a web address, is left out.
func uriListPaths(data []byte, gnome bool) []string {
	var paths []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if gnome && first {
			first = false
			if line == "copy" || line == "cut" {
				continue
			}
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !strings.HasPrefix(u.Path, "/") {
			continue
		}
		paths = append(paths, u.Path)
	}
	return paths
}

// dataOf checks that data is of type want, an image or a PDF, by its
// bytes (http.DetectContentType: a PDF's start with %PDF-), and returns
// it as a Clip; false when it isn't, such as a type a program claims and
// bytes that aren't its.
func dataOf(data []byte, want string) (Clip, bool) {
	if http.DetectContentType(data) != want || DataExt(want) == "" {
		return Clip{}, false
	}
	return Clip{Data: data, Type: want}, true
}
