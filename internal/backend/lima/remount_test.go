package lima

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkOfP is the guest's check of the folder /p, as the fake runner
// sees its command line.
const checkOfP = `limactl shell --start=false --workdir / v sh -c if mountpoint -q '/p'; then P='/p' ` + lastMount +
	` /proc/self/mountinfo; else echo devvm-not-mounted; fi`

// mountinfoOfP is /p's line of mountinfo, a 9p share with the file
// system options opts.
func mountinfoOfP(opts string) string {
	return "1109 29 0:61 / /p rw,relatime shared:604 - 9p mount0 " + opts
}

// A mountinfo line's options are the mount's and its file system's,
// whatever optional fields come between, and a line that isn't one is
// refused. Lines as a Lima VM's guest has them: Ubuntu's (a shared peer
// group) and Alpine's (no optional field).
func TestOptionsOfMountinfo(t *testing.T) {
	for _, tc := range []struct {
		line, opts, cache string
		ok                bool
	}{
		{
			"1109 29 0:61 / /home/u/src/app rw,relatime shared:604 - 9p mount0 rw,cache=0x5,access=client,msize=131072,trans=virtio",
			"rw,relatime,rw,cache=0x5,access=client,msize=131072,trans=virtio", "mmap", true,
		},
		{
			"1109 29 0:61 / /home/u/src/app rw,relatime shared:604 - 9p mount0 rw,access=client,msize=131072,trans=virtio",
			"rw,relatime,rw,access=client,msize=131072,trans=virtio", "none", true,
		},
		{
			"35 21 0:31 / /home/u/src/app rw,relatime - 9p mount0 rw,cache=0x5,access=client,msize=131072,trans=virtio",
			"rw,relatime,rw,cache=0x5,access=client,msize=131072,trans=virtio", "mmap", true,
		},
		{
			"40 21 0:33 / /home/u/my\\040app rw,relatime shared:9 master:1 - 9p mount1 rw,access=client,trans=virtio",
			"rw,relatime,rw,access=client,trans=virtio", "none", true,
		},
		{"rw,cache=0x5,trans=virtio", "", "", false},
		{"1109 29 0:61 / /p rw,relatime shared:604 9p mount0 rw,trans=virtio", "", "", false},
		{"1109 29 0:61 / /p rw,relatime shared:604 - 9p mount0", "", "", false},
	} {
		opts, ok := optionsOfMountinfo(tc.line)
		if opts != tc.opts || ok != tc.ok {
			t.Errorf("%q: %q, %v; want %q, %v", tc.line, opts, ok, tc.opts, tc.ok)
		}
		if tc.ok && cacheOfOptions(opts) != tc.cache {
			t.Errorf("%q: cache %q, want %q", tc.line, cacheOfOptions(opts), tc.cache)
		}
	}
}

// A path is written as mountinfo writes a mount point.
func TestMountinfoPath(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/src/app": "/home/u/src/app",
		"/home/u/my app":  `/home/u/my\040app`,
		"/a\tb\nc\\d":     `/a\011b\012c\134d`,
		"/home/u/café/x":  "/home/u/café/x",
	} {
		if got := mountinfoPath(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The guest's awk prints the last line whose mount point is the folder,
// exactly: of stacked mounts, the one visible there; never a folder that
// only starts with it; a path with a space by its escape.
func TestLastMount(t *testing.T) {
	info := strings.Join([]string{
		"22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw",
		"1109 29 0:61 / /home/u/src/app rw,relatime shared:604 - 9p mount0 rw,cache=0x5,access=client,trans=virtio",
		"1110 29 0:62 / /home/u/src/app2 rw,relatime shared:605 - 9p mount1 rw,trans=virtio",
		"1111 1109 0:63 / /home/u/src/app rw,relatime shared:606 - 9p mount0 rw,access=client,trans=virtio",
		"40 21 0:33 / /home/u/my\\040app rw,relatime - 9p mount2 rw,cache=0x5,trans=virtio",
	}, "\n") + "\n"
	f := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(f, []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		"/home/u/src/app": "1111 1109 0:63 / /home/u/src/app rw,relatime shared:606 - 9p mount0 rw,access=client,trans=virtio",
		"/home/u/my app":  "40 21 0:33 / /home/u/my\\040app rw,relatime - 9p mount2 rw,cache=0x5,trans=virtio",
		"/home/u/src":     "",
	} {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", lastMount+` "$1"`, "sh", f) //nolint:gosec // G204: sh with the test's own program and file
		cmd.Env = append(os.Environ(), "P="+mountinfoPath(dir))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
}
