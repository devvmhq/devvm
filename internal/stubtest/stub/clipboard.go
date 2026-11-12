package main

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
)

// The clipboard stubs stand in for the tools devvm paste reads and writes
// this machine's clipboard with: wl-paste and wl-copy (Wayland), xclip
// (X11), osascript and pbcopy (macOS). None touches the real clipboard.
// What's "on the clipboard" is in $STUB_DIR:
//
//	clip-types   the types the clipboard offers, one a line (none: an empty clipboard)
//	clip         the clipboard's contents for any type devvm asks for: an image's or a PDF's
//	             bytes, or a text/uri-list; on macOS, the PDF or PNG osascript writes
//	clip-file    on macOS, the copied files' paths, one a line, which osascript reports
//	             (none: no file copied)
//	clip-gone    on macOS, the copied file is gone, so osascript has no path for it
//	clip-floods  a read prints without end, as a tool with a huge image would
//	clip-fails   wl-paste, xclip and osascript can't reach the session, as with a
//	             WAYLAND_DISPLAY or DISPLAY from a login that's over, and fail saying so
//	copy-fails   wl-copy, xclip -i and pbcopy fail
//
// What was copied goes to $STUB_DIR/copied, and every call to $STUB_LOG.

// wlPaste stands in for wl-paste: --list-types prints clip-types, and
// fails without it, as wl-paste does on an empty clipboard; --type T
// prints clip when T is listed, else fails.
func wlPaste(c *call) int {
	c.log()
	if c.on("clip-fails") {
		c.say("Failed to connect to a Wayland server")
		return 1
	}
	if c.has("--list-types") {
		return clipTypes(c, "Nothing is copied")
	}
	return clipRead(c, c.after("--type"))
}

// xclip stands in for xclip: -t TARGETS -o prints clip-types, failing in
// xclip's words without it, -t T -o prints clip when T is listed, and -i
// copies stdin.
func xclip(c *call) int {
	c.log()
	if c.on("clip-fails") {
		c.say("xclip: Error: Can't open display: " + os.Getenv("DISPLAY"))
		return 1
	}
	switch t := c.after("-t"); {
	case c.has("-i"):
		return clipWrite(c)
	case t == "TARGETS":
		return clipTypes(c, "xclip: Error: There is no owner for the CLIPBOARD selection")
	default:
		return clipRead(c, t)
	}
}

// osascript stands in for macOS's osascript, for devvm paste's script
// alone (-l JavaScript -e SCRIPT FILE LIMIT), printing what it would, in
// the script's order: with application/pdf in clip-types, clip as a PDF,
// whatever file is copied beside it; else with clip-gone, gone; with
// clip-file, how many files past one, else the one file; else with
// image/png in clip-types, clip as a PNG. Either is written to FILE when
// it's within LIMIT bytes, else it's tooLarge; with none of them, nothing.
// With clip-fails it fails.
func osascript(c *call) int {
	c.log()
	if !strings.Contains(c.joined(), "NSPasteboard") || c.after("-l") != "JavaScript" || len(c.args) < 6 {
		c.say("stub osascript: not devvm paste's script")
		return 1
	}
	if c.on("clip-fails") {
		c.say("execution error: Error: Connection invalid (-2700)")
		return 1
	}
	found := map[string]any{}
	text, copied := c.text("clip-file")
	files := strings.Split(text, "\n")
	data := c.raw("clip", "")
	most, err := strconv.Atoi(c.arg(5))
	typ := ""
	for _, t := range []string{"application/pdf", "image/png"} {
		if typ == "" && clipHas(c, t) {
			typ = t
		}
	}
	file := copied && typ != "application/pdf"
	switch {
	case typ != "application/pdf" && c.on("clip-gone"):
		found["gone"] = true
	case file && len(files) > 1:
		found["files"] = len(files)
	case file:
		found["file"] = files[0]
	case typ == "":
	case err != nil || len(data) > most:
		found["tooLarge"] = true
	default:
		if err := writeFile(c.arg(4), data); err != nil {
			return c.status(err)
		}
		found["type"] = typ
	}
	out, err := json.Marshal(found)
	if err != nil {
		return c.status(err)
	}
	c.print(string(out) + "\n")
	return 0
}

// clipCopy stands in for wl-copy and pbcopy: it copies stdin to
// $STUB_DIR/copied, and fails with copy-fails.
func clipCopy(c *call) int {
	c.log()
	return clipWrite(c)
}

// clipTypes prints clip-types, failing without it, saying empty, the
// tool's words for an empty clipboard.
func clipTypes(c *call, empty string) int {
	t, ok := c.text("clip-types")
	if !ok {
		c.say(empty)
		return 1
	}
	c.print(t + "\n")
	return 0
}

// clipHas reports whether clip-types lists t.
func clipHas(c *call, t string) bool {
	types, _ := c.text("clip-types")
	for _, l := range strings.Split(types, "\n") {
		if strings.TrimSpace(l) == t {
			return true
		}
	}
	return false
}

// clipRead prints clip as type t, when t is listed; with clip-floods it
// prints without end.
func clipRead(c *call, t string) int {
	if !clipHas(c, t) {
		c.say("No suitable type of content copied")
		return 1
	}
	if c.on("clip-floods") {
		text := strings.Repeat("\x89PNG\r\n\x1a\nflood", 1<<12)
		for {
			if _, err := io.WriteString(c.stdout, text); err != nil {
				return 1
			}
		}
	}
	c.print(string(c.raw("clip", "")))
	return 0
}

// clipWrite copies stdin to $STUB_DIR/copied, failing with copy-fails.
func clipWrite(c *call) int {
	if c.on("copy-fails") {
		c.drain()
		return 1
	}
	return c.status(c.copyIn("copied"))
}
