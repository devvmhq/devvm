//go:build linux || darwin

package platform

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// clipStderrMax is the most of a clipboard tool's stderr devvm keeps.
const clipStderrMax = 4 << 10

// clipWait is the longest devvm waits for a clipboard tool, which can
// hang on a program that holds the clipboard and doesn't answer.
const clipWait = 30 * time.Second

// tool is the path of the clipboard tool name: in Tools when it's set,
// else in the first of the system's folders, dirs, that has it, and never
// one found on PATH, where a folder you or a VM can write, such as a
// project's, may come first. When none has it, it's the first folder's
// path, which fails as missing (ClipToolMissing).
func (c Clipboard) tool(name string, dirs ...string) string {
	if c.Tools != "" {
		return filepath.Join(c.Tools, name)
	}
	for _, d := range dirs {
		if fi, err := os.Stat(filepath.Join(d, name)); err == nil && fi.Mode().IsRegular() {
			return filepath.Join(d, name)
		}
	}
	return filepath.Join(dirs[0], name)
}

// missing reports whether starting a command failed because its program
// isn't there: not on PATH, or not at its path.
func missing(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// pipesWait is how long a clipboard tool's pipes may stay open once it
// ended or was stopped, as when a child of its keeps them, before devvm
// closes them and goes on.
const pipesWait = time.Second

// runClipTool runs the clipboard tool, a command or its path, with args
// and returns what it printed, at most limit bytes: ErrClipTooLarge past
// it, with the tool stopped, ClipToolMissing when the tool isn't there,
// and ClipToolFailed, with its stderr, when it failed or didn't finish
// within wait, each naming the tool without its folder. Its output is
// read, never logged, so an image and a path stay out of the run log.
func runClipTool(ctx context.Context, tool string, args []string, limit int64, wait time.Duration) ([]byte, error) {
	run, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(run, tool, args...) //nolint:gosec // devvm's own arguments: the tool, its switches and a type it listed
	out := &clipOut{max: limit, cancel: cancel}
	stderr := &capped{max: clipStderrMax}
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = out, stderr, pipesWait
	err := cmd.Run()
	switch {
	case out.over:
		return nil, ErrClipTooLarge
	case err == nil:
		return out.buf.Bytes(), nil
	}
	return nil, clipToolErr(ctx, run, cmd, tool, stderr.String(), err, wait)
}

// copyWith puts text on the clipboard with the copy tool, a command or
// its path, and args, text on its stdin, within wait. It runs without
// pipes on its output, because wl-copy and xclip fork a child that serves
// the clipboard until something else is copied, and Go's Wait would wait
// for that child to close them.
func copyWith(ctx context.Context, tool string, args []string, text string, wait time.Duration) error {
	run, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(run, tool, args...) //nolint:gosec // devvm's own arguments
	cmd.Stdin, cmd.WaitDelay = strings.NewReader(text), pipesWait
	if err := cmd.Run(); err != nil {
		return clipToolErr(ctx, run, cmd, tool, "", err, wait)
	}
	return nil
}

// clipToolErr is a clipboard tool's failure, err from running cmd, the
// tool, on run, a context of ctx with the tool's time limit, wait:
// ClipToolMissing when it isn't there, ctx's error when ctx ended,
// ClipToolSlow when its time ran out, and ClipToolFailed with stderr when
// it failed after it started.
func clipToolErr(ctx, run context.Context, cmd *exec.Cmd, tool, stderr string, err error, wait time.Duration) error {
	name := filepath.Base(tool)
	switch {
	case missing(err):
		return &ClipToolMissing{Tool: name}
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(run.Err(), context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay):
		return &ClipToolSlow{Tool: name, Wait: wait}
	case cmd.ProcessState == nil:
		return err
	}
	return &ClipToolFailed{Tool: name, Stderr: stderr}
}

// clipOut keeps a clipboard tool's output, at most max bytes: past them
// it's over, cancels the tool and fails every write after.
type clipOut struct {
	buf    bytes.Buffer
	max    int64
	over   bool
	cancel func()
}

func (w *clipOut) Write(p []byte) (int, error) {
	if w.over || int64(w.buf.Len()+len(p)) > w.max {
		w.over = true
		w.cancel()
		return 0, ErrClipTooLarge
	}
	return w.buf.Write(p)
}

// capped keeps the first max bytes written to it, and takes the rest
// without keeping it. Its buffer is a field, not embedded, so it has no
// ReadFrom, which io.Copy (os/exec's copy of a command's output) would
// read through without end.
type capped struct {
	buf bytes.Buffer
	max int
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// String is what it kept.
func (w *capped) String() string { return w.buf.String() }
