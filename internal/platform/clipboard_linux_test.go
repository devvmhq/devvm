package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePasteTool is a Clipboard whose tools' folder holds a script named
// tool that says text on stderr and exits 1, as the clipboard tools do
// when they fail.
func fakePasteTool(t *testing.T, tool, text string) Clipboard {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho " + text + " >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o700); err != nil { //nolint:gosec // the test's own script, to run
		t.Fatal(err)
	}
	return Clipboard{Tools: dir}
}

// A listing wl-paste fails with its word for an empty clipboard is an
// empty clipboard; one it fails otherwise, as without a compositor at
// WAYLAND_DISPLAY, is the tool's failure, with what it said; and the same
// for xclip, whose display that's gone is its failure.
func TestReadClipToolFailed(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-9")
	clip := fakePasteTool(t, "wl-paste", "'Nothing is copied'")
	if _, err := clip.ReadClip(t.Context(), 1<<20); !errors.Is(err, ErrClipEmpty) {
		t.Errorf("wl-paste on an empty clipboard: %v", err)
	}
	clip = fakePasteTool(t, "wl-paste", "'Failed to connect to a Wayland server'")
	var failed *ClipToolFailed
	if _, err := clip.ReadClip(t.Context(), 1<<20); !errors.As(err, &failed) || failed.Tool != "wl-paste" || !strings.Contains(failed.Stderr, "Failed to connect") {
		t.Errorf("wl-paste without a compositor: %v", err)
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":9")
	clip = fakePasteTool(t, "xclip", "'xclip: Error: There is no owner for the CLIPBOARD selection'")
	if _, err := clip.ReadClip(t.Context(), 1<<20); !errors.Is(err, ErrClipEmpty) {
		t.Errorf("xclip on an empty clipboard: %v", err)
	}
	clip = fakePasteTool(t, "xclip", "\"xclip: Error: Can't open display: :9\"")
	if _, err := clip.ReadClip(t.Context(), 1<<20); !errors.As(err, &failed) || failed.Tool != "xclip" || !strings.Contains(failed.Stderr, "Can't open display") {
		t.Errorf("xclip without a display: %v", err)
	}
}

// The Linux tools come from the system's folders, never PATH: a wl-paste
// first on PATH isn't the one devvm runs.
func TestLinuxToolsNotOnPath(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-9")
	planted := fakePasteTool(t, "wl-paste", "planted")
	t.Setenv("PATH", planted.Tools+":"+os.Getenv("PATH"))
	tool, err := (Clipboard{}).clipTool()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(tool.paste) || filepath.Dir(tool.paste) == planted.Tools {
		t.Errorf("wl-paste is %s", tool.paste)
	}
}
