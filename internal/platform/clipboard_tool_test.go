//go:build linux || darwin

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A clipboard tool is run from the test build's folder when it's given,
// else from the first of the system's folders that has it, else from the
// first folder, where it's missing; never by its name alone, found on
// PATH. A tool missing from its path is ClipToolMissing, named without
// its folder.
func TestClipboardTool(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(b, "wl-paste"), nil, 0o700); err != nil { //nolint:gosec // a stand-in that's never run
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a, "xclip"), 0o700); err != nil { // a folder isn't the tool
		t.Fatal(err)
	}
	for _, c := range []struct {
		clip Clipboard
		name string
		dirs []string
		want string
	}{
		{Clipboard{}, "wl-paste", []string{a, b}, filepath.Join(b, "wl-paste")},
		{Clipboard{}, "xclip", []string{a, b}, filepath.Join(a, "xclip")},
		{Clipboard{}, "wl-copy", []string{a, b}, filepath.Join(a, "wl-copy")},
		{Clipboard{Tools: "/stubs"}, "wl-paste", []string{a, b}, "/stubs/wl-paste"},
	} {
		if got := c.clip.tool(c.name, c.dirs...); got != c.want {
			t.Errorf("%+v.tool(%q, %q) = %q, want %q", c.clip, c.name, c.dirs, got, c.want)
		}
	}
	gone := filepath.Join(t.TempDir(), "pbcopy")
	var missing *ClipToolMissing
	if err := copyWith(t.Context(), gone, nil, "x", clipWait); !errors.As(err, &missing) || missing.Tool != "pbcopy" {
		t.Errorf("a copier missing from its path: %v", err)
	}
	if _, err := runClipTool(t.Context(), gone, nil, 1, clipWait); !errors.As(err, &missing) || missing.Tool != "pbcopy" {
		t.Errorf("a reader missing from its path: %v", err)
	}
}

// A clipboard tool's output is kept to its limit, the tool stopped past
// it; a tool that isn't there is ClipToolMissing; one that fails is
// ClipToolFailed with its stderr, at most clipStderrMax of it; one that
// doesn't finish in time is stopped and ClipToolSlow, with the tool and
// its limit, as is one whose child keeps its output open after it ends,
// soon after; and a cancelled run is the context's error.
func TestRunClipTool(t *testing.T) {
	slow := func(err error) bool {
		var s *ClipToolSlow
		return errors.As(err, &s) && s.Tool == "sh" && s.Wait == 200*time.Millisecond
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name   string
		ctx    context.Context //nolint:containedctx // each case's own
		tool   string
		script string
		out    string
		check  func(error) bool
	}{
		{"its output", t.Context(), "sh", "printf hello", "hello", func(err error) bool { return err == nil }},
		{"past the limit", t.Context(), "sh", "while :; do echo flood; done", "", func(err error) bool { return errors.Is(err, ErrClipTooLarge) }},
		{"missing", t.Context(), "devvm-no-such-clipboard-tool", "", "", func(err error) bool {
			var missing *ClipToolMissing
			return errors.As(err, &missing) && missing.Tool == "devvm-no-such-clipboard-tool"
		}},
		{"failing", t.Context(), "sh", "echo nope >&2; exit 3", "", func(err error) bool {
			var failed *ClipToolFailed
			return errors.As(err, &failed) && failed.Tool == "sh" && strings.TrimSpace(failed.Stderr) == "nope"
		}},
		{"failing at length", t.Context(), "sh", "i=0; while [ $i -lt 2000 ]; do printf 'xxxxx' >&2; i=$((i+1)); done; exit 1", "", func(err error) bool {
			var failed *ClipToolFailed
			return errors.As(err, &failed) && len(failed.Stderr) == clipStderrMax
		}},
		{"cancelled", cancelled, "sh", "sleep 5", "", func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"hanging", t.Context(), "sh", "sleep 5; echo late", "", slow},
		{"its child holding its output", t.Context(), "sh", "sleep 5 & echo early", "", slow},
	} {
		start := time.Now()
		out, err := runClipTool(c.ctx, c.tool, []string{"-c", c.script}, 64, 200*time.Millisecond)
		if string(out) != c.out || !c.check(err) {
			t.Errorf("%s: %q, %v", c.name, out, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %s", c.name, d)
		}
	}
	// A copier that hangs is stopped too.
	start := time.Now()
	if err := copyWith(t.Context(), "sh", []string{"-c", "sleep 5"}, "x", 200*time.Millisecond); !slow(err) || time.Since(start) > 3*time.Second {
		t.Errorf("a hanging copier: %v after %s", err, time.Since(start))
	}
}
