package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The event log: past the limit it's renamed to <log>.1, replacing the
// one there, and a new one starts; no line spans two files, and every
// file is the user's alone, whatever mode the log had.
func TestRotateRenames(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "vm.jsonl")
	if err := os.WriteFile(path, []byte("old 0123456789\n"), 0o644); err != nil { //nolint:gosec // a log left open to others
		t.Fatal(err)
	}
	r, err := openRotating(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if m := modeOf(t, path); m != 0o600 {
		t.Errorf("opened: mode %v", m)
	}
	write := func(s string) {
		t.Helper()
		if n, err := r.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	write("one 0123456789\n") // 30 bytes: at the limit, not past it
	if got := readLog(t, path); got != "old 0123456789\none 0123456789\n" || readLog(t, path+".1") != "" {
		t.Fatalf("at the limit: %q", got)
	}
	write("two\n")
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "old 0123456789\none 0123456789\n" || b != "two\n" {
		t.Errorf("rotated: .1 %q, log %q", a, b)
	}
	write("three 0123456789 01234\n")
	write("four\n")
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "two\nthree 0123456789 01234\n" || b != "four\n" {
		t.Errorf("rotated again, the oldest dropped: .1 %q, log %q", a, b)
	}
	for _, p := range []string{path, path + ".1"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Errorf("%s: mode %v", filepath.Base(p), m)
		}
	}
	// A line longer than the limit goes whole into a new log.
	long := strings.Repeat("x", 40) + "\n"
	write(long)
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "four\n" || b != long {
		t.Errorf("a long line: .1 %q, log %q", a, b)
	}
	// A log deleted while it's written starts again at its next line.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write("five\n")
	if got := readLog(t, path); got != "five\n" {
		t.Errorf("deleted: log %q", got)
	}
}

// The event log's other writer rotated it: the next line goes into the
// new log, whose size counts from its own, and the next rotation keeps
// the renamed one's lines.
func TestRotateFollowsTheOtherWriter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "vm.jsonl")
	r, err := openRotating(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	write := func(s string) {
		t.Helper()
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("proxy 0123456789 01234\n") // 23 bytes
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("proxy two\n") // 15 bytes with the other writer's line
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "proxy 0123456789 01234\n" || b != "link\nproxy two\n" {
		t.Errorf("rotated by the other writer: .1 %q, log %q", a, b)
	}
	// The other writer's line counts toward the limit too.
	if err := os.WriteFile(path, []byte("link 0123456789 0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("three\n")
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "link 0123456789 0123456789\n" || b != "three\n" {
		t.Errorf("rotated past the other writer's line: .1 %q, log %q", a, b)
	}
}

// Writes from many connections at once during a rotation: each line
// whole, once, in one of the two files.
func TestRotateConcurrent(t *testing.T) {
	t.Parallel()
	const writers, each = 16, 50
	line := func(w, i int) string { return fmt.Sprintf(`{"writer":%02d,"line":%03d,"pad":"0123456789"}`+"\n", w, i) }
	total := int64(writers * each * len(line(0, 0)))
	// One rotation: the first file takes most of the lines.
	path := filepath.Join(t.TempDir(), "vm.jsonl")
	r, err := openRotating(path, total*3/4)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				if _, err := r.Write([]byte(line(w, i))); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	old, cur := readLog(t, path+".1"), readLog(t, path)
	if old == "" || cur == "" {
		t.Fatalf("no rotation: .1 %d bytes, log %d bytes", len(old), len(cur))
	}
	seen := map[string]int{}
	for _, l := range strings.SplitAfter(old+cur, "\n") {
		if l != "" {
			seen[l]++
		}
	}
	for w := range writers {
		for i := range each {
			if n := seen[line(w, i)]; n != 1 {
				t.Errorf("%q: %d times", line(w, i), n)
			}
			delete(seen, line(w, i))
		}
	}
	if len(seen) > 0 {
		t.Errorf("lines split or not written: %v", seen)
	}
}

// The proxy's own log is also its stderr, which devvm opened and which
// its last error goes to: it's copied to <log>.1 and emptied, so writes
// that bypass the rotation still reach the current log.
func TestRotateCopies(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "vm.proxy.log")
	if err := os.WriteFile(path, []byte("started 0123\n"), 0o644); err != nil { //nolint:gosec // a log left open to others
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	r, err := copyRotating(stderr, path, stderr, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("loaded\n")); err != nil { // 20 bytes: at the limit
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("failed upstream\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.WriteString("devvm: the last error\n"); err != nil {
		t.Fatal(err)
	}
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "started 0123\nloaded\n" || b != "failed upstream\ndevvm: the last error\n" {
		t.Errorf(".1 %q, log %q", a, b)
	}
	for _, p := range []string{path, path + ".1"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Errorf("%s: mode %v", filepath.Base(p), m)
		}
	}
	// What reached the log as stderr counts toward its limit: the next
	// line, which its own lines alone would leave under it, rotates it.
	if _, err := r.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "failed upstream\ndevvm: the last error\n" || b != "x\n" {
		t.Errorf("after stderr: .1 %q, log %q", a, b)
	}
}

// The broker and its child both write the proxy's log, through the descriptor devvm gave them:
// past the limit, they rotate it once between them, each line whole in one of the two files.
func TestRotateCopiesWithTwoWriters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "proxy.log")
	fd, err := OpenLog(path)
	must(t, err)
	defer fd.Close()
	broker, err := copyRotating(fd, path, fd, 200)
	must(t, err)
	child, err := copyRotating(fd, path, fd, 200)
	must(t, err)
	defer broker.Close()
	defer child.Close()
	var wg sync.WaitGroup
	for i, w := range []*rotating{broker, child} {
		wg.Go(func() {
			for j := range 10 {
				_, _ = fmt.Fprintf(w, "writer %d line %02d\n", i, j)
			}
		})
	}
	wg.Wait()
	lines := strings.Count(readLog(t, path+".1"), "\n") + strings.Count(readLog(t, path), "\n")
	if lines != 20 || len(readLog(t, path)) > 200 {
		t.Errorf("%d lines kept, the log %d bytes", lines, len(readLog(t, path)))
	}
}

// A child that a VM took over can hold the log's lock for ever, from a description of its own: a
// line still goes in, within logLockWait or so, and the rotation is skipped rather than waited
// for; and the event log's rotation is skipped the same way.
func TestRotateLockHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.log")
	fd, err := OpenLog(path)
	must(t, err)
	defer fd.Close()
	_, err = fd.WriteString(strings.Repeat("x", 30) + "\n")
	must(t, err)
	r, err := copyRotating(fd, path, fd, 20)
	must(t, err)
	defer r.Close()
	child, err := os.Open(path) //nolint:gosec // the test's own file
	must(t, err)
	defer child.Close()
	must(t, syscall.Flock(int(child.Fd()), syscall.LOCK_EX))
	start := time.Now()
	_, err = r.Write([]byte("a line\n"))
	if took := time.Since(start); err != nil || took > 5*logLockWait {
		t.Errorf("a line with the lock held: %v after %v", err, took)
	}
	if got := readLog(t, path); !strings.HasSuffix(got, "a line\n") || !strings.HasPrefix(got, "xxx") {
		t.Errorf("the log: %q (the line in it, no rotation)", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotated under a held lock: %v", err)
	}
	// The event log.
	events := filepath.Join(dir, "network.jsonl")
	ev, err := openRotating(events, 20)
	must(t, err)
	defer ev.Close()
	_, err = ev.Write([]byte(strings.Repeat("y", 30) + "\n"))
	must(t, err)
	held, err := os.Open(events) //nolint:gosec // the test's own file
	must(t, err)
	defer held.Close()
	must(t, syscall.Flock(int(held.Fd()), syscall.LOCK_EX))
	start = time.Now()
	if _, err := ev.Write([]byte("z\n")); err != nil || time.Since(start) > 5*logLockWait {
		t.Errorf("the event log with its lock held: %v after %v", err, time.Since(start))
	}
}

// A copied log far past its limit, such as a sparse file a child made huge, is copied no further
// back than twice its limit, from a line's start, and emptied.
func TestRotateCopiesAHugeLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "proxy.log")
	fd, err := OpenLog(path)
	must(t, err)
	defer fd.Close()
	must(t, fd.Truncate(1<<40)) // a terabyte, sparse
	_, err = fd.WriteString("\nthe end of a line\nthe last line\n")
	must(t, err)
	r, err := copyRotating(fd, path, fd, 20)
	must(t, err)
	defer r.Close()
	_, err = r.Write([]byte("next\n"))
	must(t, err)
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "the end of a line\nthe last line\n" || b != "next\n" {
		t.Errorf(".1 %q, log %q", a, b)
	}
}

// A copied log whose path isn't its file any more is locked through its descriptor, and still
// rotated once past its limit; a folder in a log's place is never taken, nor removed.
func TestRotateCopiesWithoutItsPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.log")
	fd, err := OpenLog(path)
	must(t, err)
	defer fd.Close()
	must(t, os.Rename(path, path+".moved"))
	r, err := copyRotating(fd, path, fd, 20)
	must(t, err)
	defer r.Close()
	if r.lock != nil {
		t.Error("a lock on another file than the log's")
	}
	for _, l := range []string{"0123456789\n", "0123456789\n"} {
		_, err := r.Write([]byte(l))
		must(t, err)
	}
	if got := readLog(t, path+".moved"); got != "0123456789\n" {
		t.Errorf("the log after its rotation: %q", got)
	}
	must(t, os.Mkdir(filepath.Join(dir, "folder.log"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "folder.log", "x"), nil, 0o600))
	if _, err := OpenLog(filepath.Join(dir, "folder.log")); err == nil {
		t.Error("a folder taken as a log")
	}
	if _, err := OpenEventLog(filepath.Join(dir, "folder.log")); err == nil {
		t.Error("a folder taken as the event log")
	}
}

// What a VM's proxy's child could leave in a log's place goes, its name alone, and a new log
// starts there: a link to a file of yours, which stays as it was, a FIFO, never waited on, and a
// hard link of another file.
func TestOpenLogReplacesWhatIsPlanted(t *testing.T) {
	t.Parallel()
	dir, mine := t.TempDir(), t.TempDir()
	keep := filepath.Join(mine, "keep")
	must(t, os.WriteFile(keep, []byte("mine"), 0o600))
	plant := map[string]func(string) error{
		"link":      func(p string) error { return os.Symlink(keep, p) },
		"fifo":      func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"hard link": func(p string) error { return os.Link(keep, p) },
	}
	for name, fn := range plant {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".log")
		must(t, fn(p))
		f, err := OpenLog(p)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_, _ = f.WriteString("a line\n")
		_ = f.Close()
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || readLog(t, p) != "a line\n" {
			t.Errorf("%s: not a log of its own: %v", name, err)
		}
	}
	if readLog(t, keep) != "mine" {
		t.Errorf("the planted links' target: %q", readLog(t, keep))
	}
}
