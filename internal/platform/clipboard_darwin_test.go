package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// setScript puts what a test asks for on the pasteboard named by its
// first argument, a test's own, never the clipboard: its second says
// what, the rest are its files. text TEXT; files PATH... (file URLs, one
// item each, with an icon as com.apple.icns beside them, as Finder
// copies); ref PATH (a file reference URL, with the icon); image UTI FILE;
// tiff PNGFILE (the PNG made a TIFF); textimage TEXT PNGFILE; string UTI
// TEXT (text as any type, such as public.url, as Safari copies a page's
// text/uri-list); pdfpicture PDFFILE PNGFILE (a PDF and a picture of it,
// as Preview copies a selection); thumbnail PDFFILE PNGFILE PATH (a PDF,
// a TIFF of it, a file URL to PATH and empty text in one item, as
// Preview copies a page's thumbnail); photo JPEGFILE PATH (a JPEG and a
// file URL to PATH, as Photos copies); clear (nothing); release (the
// pasteboard deleted).
const setScript = `ObjC.import('AppKit');
function run(argv) {
  const pb = $.NSPasteboard.pasteboardWithName(argv[0]);
  if (argv[1] === 'release') { pb.releaseGlobally; return; }
  pb.clearContents;
  const data = (p) => $.NSData.dataWithContentsOfFile(p);
  const url = (p) => pb.writeObjects($.NSArray.arrayWithObject($.NSURL.fileURLWithPath(p)));
  const icon = () => pb.setDataForType(data('/System/Library/CoreServices/CoreTypes.bundle/Contents/Resources/GenericDocumentIcon.icns'), 'com.apple.icns');
  switch (argv[1]) {
  case 'text': pb.setStringForType(argv[2], 'public.utf8-plain-text'); break;
  case 'files': {
    const urls = $.NSMutableArray.array;
    for (const p of argv.slice(2)) urls.addObject($.NSURL.fileURLWithPath(p));
    pb.writeObjects(urls);
    icon();
    break;
  }
  case 'ref':
    pb.writeObjects($.NSArray.arrayWithObject($.NSURL.fileURLWithPath(argv[2]).fileReferenceURL));
    icon();
    break;
  case 'thumbnail':
    url(argv[4]);
    pb.setDataForType(data(argv[2]), 'com.adobe.pdf');
    pb.setDataForType($.NSBitmapImageRep.imageRepWithData(data(argv[3])).TIFFRepresentation, 'public.tiff');
    pb.setStringForType('', 'public.utf8-plain-text');
    break;
  case 'photo':
    url(argv[3]);
    pb.setDataForType(data(argv[2]), 'public.jpeg');
    break;
  case 'image': pb.setDataForType($.NSData.dataWithContentsOfFile(argv[3]), argv[2]); break;
  case 'tiff': pb.setDataForType($.NSBitmapImageRep.imageRepWithData($.NSData.dataWithContentsOfFile(argv[2])).TIFFRepresentation, 'public.tiff'); break;
  case 'textimage':
    pb.setStringForType(argv[2], 'public.utf8-plain-text');
    pb.setDataForType($.NSData.dataWithContentsOfFile(argv[3]), 'public.png');
    break;
  case 'string': pb.setStringForType(argv[3], argv[2]); break;
  case 'pdfpicture':
    pb.setDataForType($.NSData.dataWithContentsOfFile(argv[2]), 'com.adobe.pdf');
    pb.setDataForType($.NSData.dataWithContentsOfFile(argv[3]), 'public.png');
    break;
  }
}`

// pasteboard is a test's own pasteboard, deleted when the test ends.
type pasteboard struct {
	t    *testing.T
	name string
}

func newPasteboard(t *testing.T) pasteboard {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	p := pasteboard{t: t, name: "devvm-test-" + hex.EncodeToString(b[:])}
	t.Cleanup(func() { p.set("release") })
	return p
}

// set runs setScript on the pasteboard with args, on the test's context
// without its cancel, as the release runs in its cleanup.
func (p pasteboard) set(args ...string) {
	p.t.Helper()
	argv := append([]string{"-l", "JavaScript", "-e", setScript, p.name}, args...)
	ctx := context.WithoutCancel(p.t.Context())
	if out, err := exec.CommandContext(ctx, "/usr/bin/osascript", argv...).CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own script and arguments
		p.t.Fatalf("setting the pasteboard (%q): %v: %s", args, err, out)
	}
}

// The clipboard's reader on a real pasteboard, a test's own: a copied
// file alone is read as one, Finder's icon beside it ignored, and text is
// never one, whatever path or URL it says, nor is a file:// URL a web
// page wrote as public.url; several files are counted; a copied file
// that's gone fails rather than sending its icon; an image is read as
// PNG, a TIFF made one, beside text too; a PDF is read as it is, before
// the picture beside it and before a file it names, as Preview's
// thumbnails do, while a file beside an image wins, as in Photos; bytes
// that aren't a PDF are nothing; one past the cap is refused before it's
// written.
func TestReadClipPasteboard(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b, gone := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b c.txt"), filepath.Join(dir, "gone.txt")
	for _, f := range []string{a, b, gone} {
		if err := os.WriteFile(f, []byte("text"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	pngFile := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(pngFile, img.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	pb := newPasteboard(t)
	read := func(limit int64) (Clip, error) {
		t.Helper()
		return Clipboard{}.readClip(t.Context(), limit, pb.name)
	}
	// Text is never a file, nor is a URL that isn't a file URL.
	for _, text := range []string{a, "file://" + a, "hello world"} {
		pb.set("text", text)
		if c, err := read(1 << 20); !errors.Is(err, ErrClipEmpty) {
			t.Errorf("text %q: %+v, %v", text, c, err)
		}
	}
	for _, uti := range []string{"public.url", "Apple URL pasteboard type"} {
		pb.set("string", uti, "file://"+a)
		if c, err := read(1 << 20); !errors.Is(err, ErrClipEmpty) {
			t.Errorf("a file URL as %s: %+v, %v", uti, c, err)
		}
	}
	// A copied file, its icon beside it, and one by its file reference URL.
	for _, kind := range []string{"files", "ref"} {
		pb.set(kind, b)
		if c, err := read(1 << 20); err != nil || c.File != b || c.Data != nil {
			t.Errorf("%s: %+v, %v", kind, c, err)
		}
	}
	// Several files.
	pb.set("files", a, b)
	var many *ClipManyFiles
	if c, err := read(1 << 20); !errors.As(err, &many) || many.N != 2 {
		t.Errorf("two files: %+v, %v", c, err)
	}
	// A copied file that's gone: a failure, never its icon.
	pb.set("ref", gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if c, err := read(1 << 20); !errors.Is(err, ErrClipFileGone) {
		t.Errorf("a file that's gone: %+v, %v", c, err)
	}
	// Images.
	pb.set("image", "public.png", pngFile)
	if c, err := read(1 << 20); err != nil || !bytes.Equal(c.Data, img.Bytes()) || c.Type != "image/png" {
		t.Errorf("a PNG: %+v, %v", c.Type, err)
	}
	pb.set("tiff", pngFile)
	if c, err := read(1 << 20); err != nil || c.Type != "image/png" || len(c.Data) == 0 {
		t.Errorf("a TIFF: %+v, %v", c.Type, err)
	}
	// PDFs, as they are, before the picture beside one.
	pdf := []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")
	pdfFile, notPDF := filepath.Join(dir, "page.pdf"), filepath.Join(dir, "not.pdf")
	if err := os.WriteFile(pdfFile, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notPDF, []byte("not a PDF at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	pb.set("image", "com.adobe.pdf", pdfFile)
	if c, err := read(1 << 20); err != nil || !bytes.Equal(c.Data, pdf) || c.Type != TypePDF {
		t.Errorf("a PDF: %+v, %v", c.Type, err)
	}
	pb.set("pdfpicture", pdfFile, pngFile)
	if c, err := read(1 << 20); err != nil || !bytes.Equal(c.Data, pdf) || c.Type != TypePDF {
		t.Errorf("a PDF beside its picture: %+v, %v", c.Type, err)
	}
	// A page's thumbnail copied in Preview: the PDF, never the temporary
	// file Preview names beside it. A copy in Photos: the file it names,
	// which devvm asks about, not the JPEG beside it.
	pb.set("thumbnail", pdfFile, pngFile, a)
	if c, err := read(1 << 20); err != nil || !bytes.Equal(c.Data, pdf) || c.Type != TypePDF || c.File != "" {
		t.Errorf("a thumbnail in Preview: %+v, %v", c, err)
	}
	pb.set("photo", pngFile, b)
	if c, err := read(1 << 20); err != nil || c.File != b || c.Data != nil {
		t.Errorf("a copy in Photos: %+v, %v", c, err)
	}
	pb.set("image", "com.adobe.pdf", notPDF)
	if c, err := read(1 << 20); !errors.Is(err, ErrClipEmpty) {
		t.Errorf("bytes that aren't a PDF: %+v, %v", c.Type, err)
	}
	pb.set("image", "com.adobe.pdf", pdfFile)
	if c, err := read(int64(len(pdf)) - 1); !errors.Is(err, ErrClipTooLarge) {
		t.Errorf("a PDF past the cap: %+v, %v", c.Type, err)
	}
	pb.set("textimage", a, pngFile)
	if c, err := read(1 << 20); err != nil || c.Type != "image/png" || c.File != "" {
		t.Errorf("text and an image: %+v, %v", c, err)
	}
	pb.set("image", "public.png", pngFile)
	if c, err := read(int64(img.Len()) - 1); !errors.Is(err, ErrClipTooLarge) {
		t.Errorf("past the cap: %+v, %v", c.Type, err)
	}
	pb.set("clear")
	if c, err := read(1 << 20); !errors.Is(err, ErrClipEmpty) {
		t.Errorf("nothing: %+v, %v", c, err)
	}
}
