package vmlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
)

// The caps of the logs a VM can drive: each also keeps one .1 file of as much.
const (
	// VMMax is the cap of each log that a VM's backend writes as the VM
	// runs (KeepVM's): its serial console's, and its host process's own.
	VMMax = 10 << 20
	// OwnMax is the cap of the log of one of devvm's background
	// processes for a VM (Capped's): the port forwarder's and the link
	// session's.
	OwnMax = 1 << 20
)

// KeepEvery is how often KeepVM looks at its logs: a VM writing its
// serial console as fast as it can gets this long past the cap.
const KeepEvery = time.Second

// openLog opens the log at path to redact and empty it in place: a
// regular file, not one a symbolic link at its path leads to, with no
// other hard link, and never waiting on a FIFO (safefile), as a VM's
// proxy's child can write the folder of a VM's links.log and ports.log.
func openLog(path string) (*safefile.File, error) {
	return safefile.Open(path, safefile.Opts{Write: true, OneLink: true})
}

// dataStart is where f's data starts, at most size: the later of where
// the file system says it does (platform.DataStart, 0 on one without
// holes) and hole, the log's length when its keeper last emptied it,
// which its writer goes on past at its own offset. On a file system
// without holes, the emptied part reads as zeros and still takes disk.
func dataStart(f *os.File, size, hole int64) int64 {
	return min(max(platform.DataStart(f), hole), size)
}

// appendedFrom0 reports whether f, emptied by its keeper, holds data from
// its start again, as a writer that appends writes it: the file system
// says its data starts at 0, and its first block isn't all zeros, which
// a file system without holes, whose DataStart is always 0, reads there
// past a writer at its own offset.
func appendedFrom0(f *os.File) bool {
	if platform.DataStart(f) != 0 {
		return false
	}
	b := make([]byte, 4096)
	n, _ := f.ReadAt(b, 0)
	return len(bytes.Trim(b[:n], "\x00")) > 0
}

// Rotate copies the end of the log at path, at most keep bytes, to
// <log>.1, replacing the one there, and empties the log, with each
// password and proxy key in the copy replaced (rotate).
func Rotate(path string, keep int64) error {
	f, err := openLog(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	size := f.Info.Size()
	_, err = rotate(f.File, path, size, dataStart(f.File, size, 0), keep, redact.Look{})
	return err
}

// rotate copies the end of the log f at path, size bytes long and holding
// data from start, at most keep bytes, to <log>.1, replacing the one
// there, and empties the log. The copy starts at a line's start unless it
// holds all of the log's data or one line alone, and leaves out the zeros
// a hole reads as. What f gained past where look stopped (all of it for
// the zero Look) has each password and proxy key in it replaced
// (redact.In); what's before was replaced in f already. A secret cut at
// the copy's end is masked (redact.Unended), and masked reports it, as
// the rest of it is the next the writer writes. The copy is written
// whole, the user's alone. What the log's writer adds between the copy
// and the emptying is lost.
func rotate(f *os.File, path string, size, start, keep int64, look redact.Look) (masked bool, err error) {
	// Past keep, the byte before the copy too: whether it starts a line.
	from := max(size-keep-1, start)
	cut := from > start
	data := make([]byte, size-from)
	n, err := f.ReadAt(data, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	data = data[:n]
	if held := bytes.TrimLeft(data, "\x00"); len(held) < len(data) {
		from += int64(len(data) - len(held))
		data = held
	} else if cut && len(data) > 0 {
		skip := 1
		if i := bytes.IndexByte(data, '\n'); i >= 0 && i+1 < len(data) {
			skip = i + 1
		}
		from += int64(skip)
		data = data[skip:]
	}
	at := redact.Look{Line: max(look.Line-from, 0), Read: max(look.Read-from, 0)}
	if _, err := redact.In(redact.Bytes(data), int64(len(data)), at, int64(len(data))); err != nil {
		return false, err
	}
	masked = redact.Unended(data)
	if err := state.WriteFile(path+".1", data); err != nil {
		return false, err
	}
	return masked, f.Truncate(0)
}

// redactMax is as much of a log as Redact reads: a hostagent's log right
// after its start is far smaller.
const redactMax = 64 << 20

// Redact overwrites each proxy key and URL password in the log at path
// with as many "*", in place (redact.In), in its first redactMax bytes:
// the file keeps its length and every other byte, so a program still
// writing to it at its own offset (Lima's hostagent, which opened it
// without O_APPEND) goes on where it was. A missing log has nothing to
// redact.
func Redact(path string) error {
	f, err := openLog(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = redact.In(f.File, f.Info.Size(), redact.Look{}, redactMax)
	return errors.Join(err, f.Close())
}

// cleanTemps removes what a rotation of the log at path left when it was
// killed while writing <log>.1: its temporary files (state.WriteFile's).
func cleanTemps(path string) {
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".1.tmp-*"))
	for _, p := range old {
		_ = os.Remove(p)
	}
}

// Trim rotates the log at path (Rotate) once it holds more than limit
// past the hole at its start, keeping limit, and reports whether it did,
// after removing what an earlier rotation killed midway left. A missing
// log has nothing to trim.
func Trim(path string, limit int64) (bool, error) {
	cleanTemps(path)
	f, err := openLog(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	size := f.Info.Size()
	start := dataStart(f.File, size, 0)
	if size-start <= limit {
		return false, nil
	}
	_, err = rotate(f.File, path, size, start, limit, redact.Look{})
	return true, err
}

// Keep holds each log at paths to limit until ctx ends, each in a
// goroutine of its own, so one that a VM floods never holds up the
// others, looking every every (kept.look).
func Keep(ctx context.Context, paths []string, limit int64, every time.Duration) {
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Go(func() {
			cleanTemps(p)
			k := &kept{path: p, limit: limit}
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				k.look()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		})
	}
	wg.Wait()
}

// KeepVM holds the logs that a VM's backend writes as it runs
// (backend.Run.Logs) to VMMax until ctx ends, looking every KeepEvery.
func KeepVM(ctx context.Context, paths []string) { Keep(ctx, paths, VMMax, KeepEvery) }

// kept is a log Keep holds: how far it's redacted, its length at the last
// look, and its length when Keep last emptied it, where its writer's data
// starts again.
type kept struct {
	path  string
	limit int64
	at    redact.Look
	size  int64
	hole  int64
}

// look redacts in place what the log gained since the last look, at most
// limit bytes (redact.In), so a password the VM's boot writes to its
// console again, as a reboot that QEMU carries out in place does, is
// replaced within a look, not only at the VM's next start. A log that
// hasn't changed length costs nothing. Then one that holds more than
// limit past the hole at its start is rotated (rotate), its copy
// redacted only past what the look reached. A log that's gone, or that
// got shorter than this keeper left it (made again), starts afresh.
func (k *kept) look() {
	f, err := openLog(k.path)
	if err != nil {
		*k = kept{path: k.path, limit: k.limit}
		return
	}
	defer func() { _ = f.Close() }()
	size := f.Info.Size()
	if size == k.size {
		return
	}
	// Shorter than this keeper left it, or, after a rotation, with data
	// from its start (a writer that appends, not one at its own offset):
	// a log to read afresh.
	if size < k.size || size < k.hole || k.hole > 0 && appendedFrom0(f.File) {
		*k = kept{path: k.path, limit: k.limit}
	}
	start := dataStart(f.File, size, k.hole)
	k.at.Line, k.at.Read = max(k.at.Line, start), max(k.at.Read, start)
	if at, err := redact.In(f.File, size, k.at, k.limit); err == nil {
		k.at = at
	}
	k.size = size
	if size-start <= k.limit {
		return
	}
	// A secret the copy's end cut has its rest at the start of what the
	// writer writes next (Look.Cut).
	if cut, err := rotate(f.File, k.path, size, start, k.limit, k.at); err == nil {
		k.hole, k.size, k.at = size, 0, redact.Look{Line: size, Read: size, Cut: cut}
	}
}

// Capped is w, which appends to the log at path (a background process's
// output, which devvm opened for it), held to limit: before a write that
// would take the log past it, the log is rotated (Rotate). It takes the
// log's size from the file, so what reaches the log another way, such as
// the process's warnings and its last error, counts too. A failed
// rotation goes on writing: a line kept beats a log of the right size.
type Capped struct {
	w     io.Writer
	path  string
	limit int64

	mu sync.Mutex
}

// NewCapped holds w, which appends to path, to limit.
func NewCapped(w io.Writer, path string, limit int64) *Capped {
	return &Capped{w: w, path: path, limit: limit}
}

func (c *Capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fi, err := os.Lstat(c.path); err == nil && fi.Size() > 0 && fi.Size()+int64(len(p)) > c.limit {
		_ = Rotate(c.path, c.limit)
	}
	return c.w.Write(p)
}
