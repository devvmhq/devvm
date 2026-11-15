package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fuzzTop is the top-level folder the fuzzed archives' release has.
const fuzzTop = "devvm-1.0.0-darwin-arm64"

// tarOf is a tar of hs, each regular file's body as many x's as its size.
func tarOf(tb testing.TB, hs ...*tar.Header) []byte {
	tb.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		if err := tw.WriteHeader(h); err != nil {
			tb.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return b.Bytes()
}

// FuzzEntryName: an archive's entry is taken only as its top-level folder, a folder, or as a
// regular file right in it, whose name, joined to the folder unpack makes, is a file in that
// folder and nowhere else.
func FuzzEntryName(f *testing.F) {
	for _, n := range []string{
		fuzzTop, fuzzTop + "/", "./" + fuzzTop + "/devvm", fuzzTop + "/devvm", fuzzTop + "/a/b", fuzzTop + "/..",
		fuzzTop + "/.", "/" + fuzzTop + "/devvm", "../" + fuzzTop + "/devvm", fuzzTop + `/a\b`, fuzzTop + "//devvm", fuzzTop + "/README.md/",
	} {
		f.Add(n, byte(tar.TypeReg))
		f.Add(n, byte(tar.TypeDir))
		f.Add(n, byte(tar.TypeSymlink))
	}
	f.Fuzz(func(t *testing.T, name string, flag byte) {
		rest, ok := entryName(&tar.Header{Name: name, Typeflag: flag}, fuzzTop)
		switch {
		case !ok:
		case rest == "":
			if flag != tar.TypeDir {
				t.Fatalf("%q of type %q taken as the top-level folder", name, flag)
			}
		default:
			const dir = "/v"
			p := filepath.Join(dir, rest)
			if flag != tar.TypeReg || filepath.Dir(p) != dir || filepath.Base(p) != rest || !filepath.IsLocal(rest) {
				t.Fatalf("%q of type %q taken as the file %q", name, flag, p)
			}
		}
	})
}

// FuzzUnpack: whatever a release's archive holds, unpack writes only regular files right in the
// folder it makes, devvm among them, each as the archive has it, devvm executable and the rest
// not, at most unpackCap in all, and nothing outside that folder; an archive it refuses leaves
// no folder.
func FuzzUnpack(f *testing.F) {
	dir := &tar.Header{Name: fuzzTop + "/", Typeflag: tar.TypeDir, Mode: 0o755}
	file := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: fuzzTop + "/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: size}
	}
	f.Add(tarOf(f, dir, file("devvm", 10), file("README.md", 3)))
	f.Add(tarOf(f, file("devvm", 1)))
	f.Add(tarOf(f, dir, file("README.md", 3)))
	f.Add(tarOf(f, dir, file("devvm", 1), file("devvm", 2)))
	f.Add(tarOf(f, dir, file("devvm", 1), &tar.Header{Name: fuzzTop + "/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	f.Add(tarOf(f, dir, file("devvm", 1), file("../evil", 1)))
	f.Add(tarOf(f, dir, file("devvm", 1), &tar.Header{Name: fuzzTop + "/sub/", Typeflag: tar.TypeDir, Mode: 0o755}, file("sub/x", 1)))
	f.Fuzz(func(t *testing.T, tarball []byte) {
		// A sparse entry's holes need no bytes in the archive, so a tiny one can unpack hundreds
		// of MiB, within unpackCap: past a MiB, the fuzzer would spend its time writing zeros.
		tr := tar.NewReader(bytes.NewReader(tarball))
		for h, err := tr.Next(); err == nil; h, err = tr.Next() {
			if h.Size > 1<<20 {
				return
			}
		}
		root := t.TempDir()
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(tarball)
		_ = zw.Close()
		archive, out := filepath.Join(root, "a.tar.gz"), filepath.Join(root, "v")
		if err := os.WriteFile(archive, gz.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		err := unpack(archive, fuzzTop, out)
		if names := dirNames(t, root); !slices.Equal(names, []string{"a.tar.gz", "v"}) && !slices.Equal(names, []string{"a.tar.gz"}) {
			t.Fatalf("unpack left %q", names)
		}
		if err != nil {
			if _, serr := os.Lstat(out); !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf("refused (%v), and left its folder: %v", err, serr)
			}
			return
		}
		checkUnpacked(t, out, filesOf(tarball))
	})
}

// filesOf is the regular files right in fuzzTop of the tar b, by name: its own reading of it.
func filesOf(b []byte) map[string][]byte {
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			return files
		}
		if rest, ok := entryName(h, fuzzTop); ok && rest != "" {
			files[rest], _ = io.ReadAll(tr)
		}
	}
}

// checkUnpacked checks the folder out unpack made against want, the archive's files.
func checkUnpacked(t *testing.T, out string, want map[string][]byte) {
	t.Helper()
	var total int64
	names := dirNames(t, out)
	if !slices.Contains(names, "devvm") || len(names) != len(want) {
		t.Fatalf("unpacked %q, from an archive of %d files", names, len(want))
	}
	for _, n := range names {
		p := filepath.Join(out, n)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s: %v, %v", n, fi.Mode(), err)
		}
		if perm := fi.Mode().Perm(); n == "devvm" && perm != 0o755 || n != "devvm" && perm != 0o644 {
			t.Fatalf("%s: mode %v", n, perm)
		}
		b, err := os.ReadFile(p) //nolint:gosec // the test's own folder
		if err != nil || !bytes.Equal(b, want[n]) {
			t.Fatalf("%s: %d bytes, want %d: %v", n, len(b), len(want[n]), err)
		}
		total += fi.Size()
	}
	if total > unpackCap {
		t.Fatalf("unpacked %d bytes", total)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Name()
	}
	return names
}
