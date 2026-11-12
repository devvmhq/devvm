package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// clipScript is the JavaScript for Automation that osascript runs to read
// the clipboard, through NSPasteboard, with its arguments: the file to
// write a PDF or an image to, the most bytes it writes, and a
// pasteboard's name, which only tests give (their own), for the
// clipboard's in its place.
//
// It takes a PDF first (com.adobe.pdf), as it is, never made an image: a
// selection copied in Preview has a picture of it beside it, and a page's
// thumbnail a file URL to a temporary file of Preview's too. Then file
// URLs alone (NSPasteboardURLReadingFileURLsOnlyKey), which only a copied
// file puts there, as Finder and Photos do: text, a path or a file:// URL
// included, is never one. Several are counted, never read. One is printed
// as its path, a file reference URL (file:///.file/id=) resolved, or as
// gone when it can't be. Then an image: PNG, JPEG, GIF or WebP as it is,
// else TIFF, as macOS apps copy one, made a PNG. A PDF or an image is
// written only within the cap, so a huge one never reaches the disk. It
// prints one JSON object: files (how many, past one), file, gone, type
// (what it wrote), tooLarge, or none of them for nothing devvm sends.
// Text is never asked for.
const clipScript = `ObjC.import('AppKit');
function run(argv) {
  const pb = argv[2] ? $.NSPasteboard.pasteboardWithName(argv[2]) : $.NSPasteboard.generalPasteboard;
  const write = (d, type) => {
    if (Number(d.length) > Number(argv[1])) return JSON.stringify({tooLarge: true});
    if (!d.writeToFileAtomically(argv[0], false)) throw new Error('writing what was copied failed');
    return JSON.stringify({type: type});
  };
  const pdf = pb.dataForType('com.adobe.pdf');
  if (!pdf.isNil()) return write(pdf, 'application/pdf');
  const only = $.NSDictionary.dictionaryWithObjectForKey($.NSNumber.numberWithBool(true), $.NSPasteboardURLReadingFileURLsOnlyKey);
  const urls = pb.readObjectsForClassesOptions($.NSArray.arrayWithObject($.NSURL), only);
  const n = urls.isNil() ? 0 : Number(urls.count);
  if (n > 1) return JSON.stringify({files: n});
  if (n === 1) {
    const u = urls.objectAtIndex(0).filePathURL;
    return JSON.stringify(u.isNil() ? {gone: true} : {file: u.path.js});
  }
  const types = [['public.png', 'image/png'], ['public.jpeg', 'image/jpeg'], ['com.compuserve.gif', 'image/gif'],
    ['org.webmproject.webp', 'image/webp'], ['public.tiff', '']];
  for (const [uti, mime] of types) {
    let d = pb.dataForType(uti);
    if (d.isNil()) continue;
    let type = mime;
    if (type === '') {
      const rep = $.NSBitmapImageRep.imageRepWithData(d);
      if (rep.isNil()) continue;
      d = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $.NSDictionary.dictionary);
      if (d.isNil()) continue;
      type = 'image/png';
    }
    return write(d, type);
  }
  return '{}';
}`

// clipFound is what clipScript printed.
type clipFound struct {
	Files    int    `json:"files"`
	File     string `json:"file"`
	Gone     bool   `json:"gone"`
	Type     string `json:"type"`
	TooLarge bool   `json:"tooLarge"`
}

// clipFoundMax bounds what clipScript prints: one path at most.
const clipFoundMax = 64 << 10

// macTools is the folder of macOS's clipboard tools, osascript and
// pbcopy, which System Integrity Protection keeps as Apple shipped them.
const macTools = "/usr/bin"

// ReadClip reads the clipboard through osascript (clipScript): one copied
// file's path, else a PDF or an image, written to a file in a private folder of
// devvm's, read, and deleted, at most limit bytes (ErrClipTooLarge past
// it). A copied file wins over an image, because Finder puts the file's
// icon on the clipboard as an image beside it; several are
// ClipManyFiles, and one that's gone is ErrClipFileGone, never the icon
// sent instead. A script that fails is ClipToolFailed. Text is never
// asked for.
func (c Clipboard) ReadClip(ctx context.Context, limit int64) (Clip, error) {
	return c.readClip(ctx, limit, "")
}

// readClip is ReadClip on the pasteboard named pasteboard, "" for the
// clipboard: tests read their own.
func (c Clipboard) readClip(ctx context.Context, limit int64, pasteboard string) (Clip, error) {
	dir, err := os.MkdirTemp("", "devvm-paste-")
	if err != nil {
		return Clip{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "clip")
	args := []string{"-l", "JavaScript", "-e", clipScript, file, strconv.FormatInt(limit, 10)}
	if pasteboard != "" {
		args = append(args, pasteboard)
	}
	out, err := runClipTool(ctx, c.tool("osascript", macTools), args, clipFoundMax, clipWait)
	if err != nil {
		return Clip{}, err
	}
	var found clipFound
	if err := json.Unmarshal(out, &found); err != nil {
		return Clip{}, fmt.Errorf("osascript's answer: %w", err)
	}
	switch {
	case found.Files > 1:
		return Clip{}, &ClipManyFiles{N: found.Files}
	case found.File != "":
		return Clip{File: found.File}, nil
	case found.Gone:
		return Clip{}, ErrClipFileGone
	case found.TooLarge:
		return Clip{}, ErrClipTooLarge
	case found.Type == "":
		return Clip{}, ErrClipEmpty
	}
	f, err := os.Open(file) //nolint:gosec // devvm's own file, in a folder of mode 700 it just made
	if err != nil {
		return Clip{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	switch {
	case err != nil:
		return Clip{}, err
	case int64(len(data)) > limit:
		return Clip{}, ErrClipTooLarge
	}
	if clip, ok := dataOf(data, found.Type); ok {
		return clip, nil
	}
	return Clip{}, ErrClipEmpty
}

// CopyText puts text on the clipboard with pbcopy.
func (c Clipboard) CopyText(ctx context.Context, text string) error {
	return copyWith(ctx, c.tool("pbcopy", macTools), nil, text, clipWait)
}
