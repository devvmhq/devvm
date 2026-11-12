package lima

import (
	"archive/tar"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// writeTar writes a tarball of the files, name and contents, in order.
func writeTar(t *testing.T, path string, files ...[2]string) {
	t.Helper()
	fh, err := os.Create(path) //nolint:gosec // the test's own folder
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(fh)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o600, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

// Docker's own image store names the config, containerd's the manifest.
func TestExportIs(t *testing.T) {
	dir := t.TempDir()
	h, m := strings.Repeat("a", 64), strings.Repeat("b", 64)
	both := filepath.Join(dir, "i.tar")
	writeTar(t, both, [2]string{"blobs/sha256/" + h, "{}"}, [2]string{"manifest.json", `[{"Config":"blobs/sha256/` + h + `"}]`},
		[2]string{"index.json", `{"manifests":[{"digest":"sha256:` + m + `"}]}`})
	old := filepath.Join(dir, "old.tar")
	writeTar(t, old, [2]string{"manifest.json", `[{"Config":"` + h + `.json"}]`})
	for _, c := range []struct {
		file, id string
		want     bool
	}{
		{both, "sha256:" + h, true},
		{both, "sha256:" + m, true},
		{both, "sha256:" + strings.Repeat("c", 64), false},
		{both, "sha256:x", false},
		{both, h, false},
		{old, "sha256:" + h, true},
		{old, "sha256:" + m, false},
		{filepath.Join(dir, "none.tar"), "sha256:" + h, false},
	} {
		if got := exportIs(c.file, c.id); got != c.want {
			t.Errorf("exportIs(%s, %s) = %v", filepath.Base(c.file), c.id, got)
		}
	}
}

// The conversion's steps are written into its own folder, beside vm/,
// owner-only, and devvm's guest/ca.sh only with the user's CAs; nothing
// else of devvm's files, oci/try.sh included.
func TestWriteScripts(t *testing.T) {
	own := fstest.MapFS{
		"oci/build.sh": {Data: []byte("build\n")}, "oci/bootify.sh": {Data: []byte("bootify\n")},
		"oci/kernel.sh": {Data: []byte("kernel\n")}, "oci/try.sh": {Data: []byte("try\n")},
		"oci/sub/x.sh": {Data: []byte("x\n")}, "guest/ca.sh": {Data: []byte("ca\n")},
		"guest/sync.sh": {Data: []byte("sync\n")}, "base.yaml": {Data: []byte("cpus: 4\n")},
		"prelude.sh": {Data: []byte("prelude\n")},
	}
	l, _, _ := testLima(t, "darwin")
	l.Assets = own
	l.User = func() (*user.User, error) { return &user.User{Username: "me", Uid: "501"}, nil }
	steps, err := l.convertSteps(&backend.From{Ref: "debian:12", Digest: "debian@sha256:aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	for _, cas := range []bool{false, true} {
		out := t.TempDir()
		oci, guest, err := writeScripts(own, steps, out, cas)
		if err != nil || oci != filepath.Join(out, "oci") || guest != filepath.Join(out, "guest") {
			t.Fatalf("writeScripts = %q, %q, %v", oci, guest, err)
		}
		var got []string
		err = filepath.WalkDir(out, func(path string, e fs.DirEntry, err error) error {
			if err != nil || path == out {
				return err
			}
			fi, err := e.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(out, path)
			got = append(got, rel+" "+fi.Mode().Perm().String())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"oci -rwx------", "oci/bootify.sh -rw-------", "oci/build.sh -rw-------", "oci/kernel.sh -rw-------"}
		if cas {
			want = append([]string{"guest -rwx------", "guest/ca.sh -rw-------"}, want...)
		}
		if !slices.Equal(got, want) {
			t.Errorf("cas %v: wrote %q, want %q", cas, got, want)
		}
		// build.sh after the prelude's line; the scripts it runs as they are.
		for name, want := range map[string]string{"build.sh": "eval $'prelude\\n'\nbuild\n", "bootify.sh": "bootify\n", "kernel.sh": "kernel\n"} {
			if b, _ := os.ReadFile(filepath.Join(oci, name)); string(b) != want { //nolint:gosec // the test's own folder
				t.Errorf("%s = %q, want %q", name, b, want)
			}
		}
	}
	// devvm's files lacking ca.sh: an error.
	delete(own, "guest/ca.sh")
	if _, _, err := writeScripts(own, steps, t.TempDir(), true); err == nil {
		t.Error("no ca.sh, no error")
	}
}

// writeConverted writes what a good conversion writes into dir.
func writeConverted(t *testing.T, dir string) {
	t.Helper()
	disk := make([]byte, 2048)
	disk[1080], disk[1081] = 0x53, 0xef
	for name, data := range map[string]string{
		"disk.raw": string(disk), "kernel": "k", "initrd": "i",
		"cmdline": "root=/dev/vda rw console=hvc0\n", "libc": "musl\n",
	} {
		writeTestFile(t, filepath.Join(dir, name), data)
	}
}

func TestCheckConverted(t *testing.T) {
	dir := t.TempDir()
	writeConverted(t, dir)
	if c, err := checkConverted(dir, "node:24"); err != nil || c.libc != "musl" || c.glibc != "" || c.cmdline != "root=/dev/vda rw console=hvc0" || c.dir != dir {
		t.Fatalf("checkConverted = %+v, %v", c, err)
	}
	// gnu: with its glibc version.
	gnu := t.TempDir()
	writeConverted(t, gnu)
	writeTestFile(t, filepath.Join(gnu, "libc"), "gnu\n")
	writeTestFile(t, filepath.Join(gnu, "glibc"), "2.36\n")
	if c, err := checkConverted(gnu, "node:24"); err != nil || c.libc != "gnu" || c.glibc != "2.36" {
		t.Fatalf("checkConverted (gnu) = %+v, %v", c, err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	writeTestFile(t, secret, "hostile")
	for _, c := range []struct {
		name, file string
		bad        func(dir string)
	}{
		{"a symlink to a host file", "disk.raw", func(d string) {
			_ = os.Remove(filepath.Join(d, "disk.raw"))
			_ = os.Symlink(secret, filepath.Join(d, "disk.raw"))
		}},
		{"a folder", "kernel", func(d string) {
			_ = os.Remove(filepath.Join(d, "kernel"))
			_ = os.Mkdir(filepath.Join(d, "kernel"), 0o700)
		}},
		{"a missing file", "initrd", func(d string) { _ = os.Remove(filepath.Join(d, "initrd")) }},
		{"an unknown libc", "libc", func(d string) { writeTestFile(t, filepath.Join(d, "libc"), "gnu\nx\n") }},
		{"gnu without its glibc version", "glibc", func(d string) { writeTestFile(t, filepath.Join(d, "libc"), "gnu\n") }},
		{"gnu with a glibc version that isn't one", "glibc", func(d string) {
			writeTestFile(t, filepath.Join(d, "libc"), "gnu\n")
			writeTestFile(t, filepath.Join(d, "glibc"), "glibc 2.36\n")
		}},
		{"gnu with its glibc version a symlink", "glibc", func(d string) {
			writeTestFile(t, filepath.Join(d, "libc"), "gnu\n")
			writeTestFile(t, filepath.Join(d, "v"), "2.36\n")
			_ = os.Symlink(filepath.Join(d, "v"), filepath.Join(d, "glibc"))
		}},
		{"a command line that could change Lima's config", "cmdline", func(d string) {
			writeTestFile(t, filepath.Join(d, "cmdline"), "root=/dev/vda rw console=hvc0\"\ncopyToHost:\n- guest: /etc/hostname\n")
		}},
		{"an empty command line", "cmdline", func(d string) { writeTestFile(t, filepath.Join(d, "cmdline"), "") }},
		{"a qcow2 disk", "disk.raw", func(d string) { writeTestFile(t, filepath.Join(d, "disk.raw"), "QFI\xfb") }},
		{"a qcow2 header before an ext4 superblock", "disk.raw", func(d string) {
			disk := make([]byte, 2048)
			copy(disk, "QFI\xfb")
			disk[1080], disk[1081] = 0x53, 0xef
			writeTestFile(t, filepath.Join(d, "disk.raw"), string(disk))
		}},
		{"a disk too short for a superblock", "disk.raw", func(d string) { writeTestFile(t, filepath.Join(d, "disk.raw"), string(make([]byte, 1081))) }},
		{"gnu with its glibc version a link to a FIFO", "glibc", func(d string) {
			writeTestFile(t, filepath.Join(d, "libc"), "gnu\n")
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Symlink(fifo, filepath.Join(d, "glibc"))
		}},
	} {
		d := t.TempDir()
		writeConverted(t, d)
		c.bad(d)
		_, err := checkConverted(d, "node:24")
		if want := "convert_bad_output file=" + c.file + " from=node:24"; err == nil || msg.NamesOf(err) != want {
			t.Errorf("%s: %v, want %s", c.name, err, want)
		}
	}
	if data, _ := os.ReadFile(secret); string(data) != "hostile" { //nolint:gosec // the test's own file
		t.Error("the host file changed")
	}
}

// Conversions take turns: one waits while another devvm holds the lock,
// saying so, and takes it once that one lets go; a lock file nothing
// holds, as a devvm that crashed leaves it, is taken at once.
func TestLockOCI(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	l.OCINote = 10 * time.Millisecond
	var infos msg.Recorder
	l.Reporter = &msg.Fake{OnInfo: infos.Add}

	writeTestFile(t, l.Dirs.OCIBuildLock(), "")
	unlock, err := l.lockOCI(t.Context())
	if err != nil || len(infos.IDs()) != 0 {
		t.Fatalf("lockOCI on a file nothing holds: %v, infos %q", err, infos.IDs())
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		unlock()
	}()
	again, err := l.lockOCI(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(infos.IDs(), []string{"oci_waiting"}) {
		t.Errorf("infos: %q", infos.IDs())
	}
	again()
}
