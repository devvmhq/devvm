package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// watchedText is a watched file read as readPrivate reads the bindings, its T its text, counting
// failures logged and changes seen.
func watchedText(path string, keep bool) (w *watched[string], fails, changes *int) {
	fails, changes = new(int), new(int)
	w = &watched[string]{
		path: path, keep: keep,
		read: func() (fileRead, error) {
			f, err := openPrivate(path, 0o077)
			if errors.Is(err, os.ErrNotExist) {
				return fileRead{missing: true}, nil
			}
			if err != nil {
				return fileRead{}, err
			}
			defer f.Close()
			b, err := f.ReadAll(64)
			return fileRead{data: b, st: stampOf(f.Info)}, err
		},
		parse:   func(b []byte) (string, error) { return string(b), nil },
		onFail:  func(error) { *fails++ },
		changed: func() { *changes++ },
	}
	return w, fails, changes
}

func writeAt(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(text), 0o600))
	must(t, os.Chtimes(path, at, at))
}

// Two writes of the same size within one mtime tick, on the same inode, are both seen: a file
// whose mtime is that recent is read again at every call.
func TestWatchedSameTick(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	w, _, changes := watchedText(path, false)
	at := time.Now()
	writeAt(t, path, "one", at)
	if got := w.get(); got != "one" {
		t.Fatalf("get = %q", got)
	}
	writeAt(t, path, "two", at)
	if got := w.get(); got != "two" {
		t.Errorf("a second write in the same tick: get = %q", got)
	}
	if w.get(); *changes != 2 {
		t.Errorf("changes = %d, want 2: an unchanged file read again isn't a change", *changes)
	}
}

// A file whose mtime is older than a tick is read again only when its stamp changes: a write that
// kept its mtime but not its size is seen, a touch alone runs changed without parsing.
func TestWatchedSettled(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	w, _, changes := watchedText(path, false)
	parses := 0
	parse := w.parse
	w.parse = func(b []byte) (string, error) { parses++; return parse(b) }
	old := time.Now().Add(-time.Hour)
	writeAt(t, path, "one", old)
	w.get()
	w.get()
	if parses != 1 || *changes != 1 {
		t.Fatalf("parses, changes = %d, %d; want 1, 1", parses, *changes)
	}
	writeAt(t, path, "longer", old)
	if got := w.get(); got != "longer" {
		t.Errorf("a new size with the same mtime: get = %q", got)
	}
	writeAt(t, path, "longer", old.Add(time.Minute))
	if w.get(); parses != 2 || *changes != 3 {
		t.Errorf("after a touch: parses, changes = %d, %d; want 2, 3", parses, *changes)
	}
}

// What a failure does: with keep, the last good T stays; without, there's none. Each failure is
// logged once, however many calls see it; a file that's gone is none, silently, and is read again
// when it's back.
func TestWatchedFailures(t *testing.T) {
	t.Parallel()
	for _, keep := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "f")
		w, fails, _ := watchedText(path, keep)
		writeAt(t, path, "good", time.Now().Add(-time.Hour))
		w.get()
		must(t, os.Chmod(path, 0o644)) //nolint:gosec // G302: a file others may read is what's tested
		later := time.Now().Add(-time.Minute)
		must(t, os.Chtimes(path, later, later))
		want := ""
		if keep {
			want = "good"
		}
		for range 3 {
			if got := w.get(); got != want {
				t.Errorf("keep %v: an unreadable file gave %q, want %q", keep, got, want)
			}
		}
		if *fails != 1 {
			t.Errorf("keep %v: %d failures logged, want 1", keep, *fails)
		}
		must(t, os.Remove(path))
		if got := w.get(); got != "" || *fails != 1 {
			t.Errorf("keep %v: a file that's gone gave %q, %d failures logged", keep, got, *fails)
		}
		writeAt(t, path, "good", time.Now().Add(-time.Hour))
		if got := w.get(); got != "good" {
			t.Errorf("keep %v: a file back gave %q", keep, got)
		}
	}
}

// A change of mode alone leaves the stamp as it was, so a settled file made readable by others
// after a good read keeps its contents until it's written again, which then refuses it.
func TestWatchedChmodOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	w, fails, _ := watchedText(path, false)
	old := time.Now().Add(-time.Hour)
	writeAt(t, path, "good", old)
	w.get()
	must(t, os.Chmod(path, 0o644)) //nolint:gosec // G302: a file others may read is what's tested
	if got := w.get(); got != "good" || *fails != 0 {
		t.Errorf("after a chmod alone: get = %q, %d failures", got, *fails)
	}
	must(t, os.Chtimes(path, old.Add(time.Minute), old.Add(time.Minute)))
	if got := w.get(); got != "" || *fails != 1 {
		t.Errorf("after its next change: get = %q, %d failures", got, *fails)
	}
}

// A parse that fails is a failure as a read's is, and a symbolic link is refused (readPrivate).
func TestWatchedRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target, path := filepath.Join(dir, "target"), filepath.Join(dir, "f")
	writeAt(t, target, "x", time.Now())
	must(t, os.Symlink(target, path))
	w, fails, _ := watchedText(path, false)
	if got := w.get(); got != "" || *fails != 1 {
		t.Errorf("a symbolic link gave %q, %d failures", got, *fails)
	}
	must(t, os.Remove(path))
	writeAt(t, path, "bad", time.Now())
	w.parse = func([]byte) (string, error) { return "", errors.New("bad") }
	if got := w.get(); got != "" || *fails != 2 {
		t.Errorf("a parse that failed gave %q, %d failures", got, *fails)
	}
}

// A stat refused for want of permission, as a sandbox refuses the child one, reads the file each
// time, as the broker can, rather than fail: its folder here is one the test can't search.
func TestWatchedStatRefused(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root searches any folder")
	}
	dir := filepath.Join(t.TempDir(), "closed")
	must(t, os.Mkdir(dir, 0o700))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a folder, searchable again for its cleanup
	must(t, os.Chmod(dir, 0))
	reads := 0
	text := "one"
	w := &watched[string]{
		path: filepath.Join(dir, "f"),
		read: func() (fileRead, error) {
			reads++
			return fileRead{data: []byte(text), st: stamp{size: int64(len(text))}}, nil
		},
		parse:  func(b []byte) (string, error) { return string(b), nil },
		onFail: func(err error) { t.Errorf("a failure: %v", err) },
	}
	if got := w.get(); got != "one" {
		t.Errorf("get = %q", got)
	}
	text = "two"
	if got := w.get(); got != "two" || reads != 2 {
		t.Errorf("get = %q after %d reads, want two after 2", got, reads)
	}
}
