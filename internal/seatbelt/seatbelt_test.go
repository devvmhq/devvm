package seatbelt

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

func TestString(t *testing.T) {
	t.Parallel()
	if got, want := String(`/a "b" \c`), `"/a \"b\" \\c"`; got != want {
		t.Errorf("String = %s, want %s", got, want)
	}
}

// Links finds each link on the way to a path, the path itself included, and those on the way to
// each link's target, as Seatbelt sees each: its folder's links resolved.
func TestLinks(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real/sub", "other"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// a -> real, real/sub/l -> ../../b, b -> other
	for link, target := range map[string]string{"a": "real", "real/sub/l": "../../b", "b": filepath.Join(dir, "other")} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	got := Links(filepath.Join(dir, "a", "sub", "l", "file"))
	slices.Sort(got)
	want := []string{filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "real", "sub", "l")}
	if !slices.Equal(got, want) {
		t.Errorf("Links = %q, want %q", got, want)
	}
	if got := Links(filepath.Join(dir, "other", "file")); len(got) != 0 {
		t.Errorf("no links: %q", got)
	}
}

// The child's profile denies by default and allows what it needs: devvm's executable to run and
// read, its folders to write regular files in, its files to look at, the links on the way to
// them; and never the Keychain, nor any network out, as the broker opens its connections.
func TestChildProfile(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs, policy, target := filepath.Join(dir, "logs"), filepath.Join(dir, "linked.policy"), filepath.Join(dir, "real.policy")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, policy); err != nil {
		t.Fatal(err)
	}
	p, err := ChildProfile(proxy.ChildNeeds{Exe: "/opt/devvm/bin/devvm", Folders: []string{logs}, Watched: []string{policy}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(version 1)\n(deny default)\n",
		`(allow process-exec (literal "/opt/devvm/bin/devvm"))`,
		`(allow file-read* (literal "/opt/devvm/bin/devvm"))`,
		`(allow file-read-metadata (literal "/opt/devvm/bin"))`,
		"(allow file-read* file-write* (subpath " + String(logs) + "))",
		"(deny file-write-create (require-all (subpath " + String(logs) + ") (require-not (vnode-type REGULAR-FILE))))",
		"(deny file-link)",
		"(allow file-read-metadata (literal " + String(policy) + "))",
		"(allow file-read-metadata (literal " + String(target) + "))",
		`(deny sysctl-read (sysctl-name-prefix "kern.proc"))`,
		"(deny process-info* (target others))\n(deny process-info-listpids)\n",
		"(deny file-write-flags file-write-acl file-write-owner file-write-setugid file-write-xattr)",
		"(deny file-write-unlink (vnode-type DIRECTORY))",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("no %s in\n%s", want, p)
		}
	}
	for _, never := range []string{"SecurityServer", "(allow default)", "process-fork", "network-bind", "network-inbound", "network-outbound", "mDNSResponder", "resolv.conf"} {
		if strings.Contains(p, never) {
			t.Errorf("%s in\n%s", never, p)
		}
	}
	// A path that isn't absolute, or that is the root, would open what it doesn't name.
	if err := os.Symlink("/", filepath.Join(dir, "root")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []proxy.ChildNeeds{
		{Exe: "devvm"},
		{Exe: "/d", Folders: []string{"logs"}},
		{Exe: "/d", Folders: []string{"/"}},
		{Exe: "/d", Folders: []string{filepath.Join(dir, "root")}},
		{Exe: "/d", Watched: []string{"policy"}},
		{Exe: "/"},
	} {
		if _, err := ChildProfile(n); err == nil {
			t.Errorf("%+v: a profile", n)
		}
	}
}

// ProxyChild is sandbox-exec with the child's profile as an argument, fs.ErrNotExist when
// sandbox-exec isn't there, and the profile's error when it can't be made.
func TestProxyChild(t *testing.T) {
	t.Parallel()
	exec := filepath.Join(t.TempDir(), "sandbox-exec")
	if _, err := (Sandbox{Exec: exec}).ProxyChild(proxy.ChildNeeds{Exe: "/d"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if err := os.WriteFile(exec, nil, 0o700); err != nil { //nolint:gosec // G306: a program, as sandbox-exec is
		t.Fatal(err)
	}
	cmd, err := Sandbox{Exec: exec}.ProxyChild(proxy.ChildNeeds{Exe: "/d"})
	if err != nil || len(cmd) != 3 || cmd[0] != exec || cmd[1] != "-p" || !strings.HasPrefix(cmd[2], "(version 1)") {
		t.Errorf("%q, %v", cmd, err)
	}
	// A profile that can't be made, of a path that isn't absolute: no command.
	if cmd, err := (Sandbox{Exec: exec}).ProxyChild(proxy.ChildNeeds{Exe: "/d", Folders: []string{"logs"}}); err == nil || cmd != nil {
		t.Errorf("a relative folder: %q, %v", cmd, err)
	}
}
