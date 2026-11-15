package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The setup's /etc/resolv.conf, as sync.sh's resolver_conf writes it: a
// confined VM's programs ask its one name server directly, past
// systemd-resolved's stub, and keep the stub's search line; an open VM's
// file goes back to the stub's link. A file that is neither the stub's
// link nor devvm's own is left as it is, and so is a guest without
// systemd, but for devvm's own file.
func TestResolverConf(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "run", "systemd", "resolve", "stub-resolv.conf")
	conf := filepath.Join(dir, "etc", "resolv.conf")
	for _, d := range []string{filepath.Dir(stub), filepath.Dir(conf)} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // the test's own folders
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(stub, []byte("nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch corp.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", conf); err != nil {
		t.Fatal(err)
	}
	fn := "resolver_conf() {\n" + between(t, "guest/sync.sh", "resolver_conf() {", "}") + "\n}\n"
	calls := filepath.Join(dir, "systemctl")
	run := func(systemd, resolver string) string {
		t.Helper()
		script := "set -euo pipefail\nsudo() { \"$@\"; }\ncalls=$5\nsystemctl() { echo \"$*\" >>\"$calls\"; }\nsystemd=$1 DEVVM_RESOLVER=$2\n" +
			fn + "resolver_conf \"$3\" \"$4\"\n"
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", systemd, resolver, conf, stub, calls) //nolint:gosec // sync.sh's own function, on the test's files
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("resolver_conf: %v\n%s", err, out)
		}
		return string(out)
	}
	own := "# devvm: a confined VM asks its one name server directly (DEVVM_RESOLVER)\n" +
		"nameserver 127.0.0.1\nsearch corp.example\noptions timeout:1 attempts:2\n"
	check := func(step, want string, link bool) {
		t.Helper()
		fi, err := os.Lstat(conf)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode()&os.ModeSymlink != 0; got != link {
			t.Errorf("%s: a link is %v, want %v", step, got, link)
		}
		b, err := os.ReadFile(conf) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s: resolv.conf is %q, want %q", step, b, want)
		}
	}
	stubText := "nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch corp.example\n"

	run("false", "127.0.0.1")
	check("without systemd", stubText, true)
	run("true", "127.0.0.1")
	check("confined", own, false)
	run("true", "127.0.0.1")
	check("confined again", own, false)
	run("true", "")
	check("open", stubText, true)
	// systemd-resolved restarts once, as the file goes back to its link,
	// so it forgets the servers and search domain it read from devvm's.
	run("true", "")
	if b, _ := os.ReadFile(calls); string(b) != "try-restart systemd-resolved\n" { //nolint:gosec // the test's own file
		t.Errorf("systemctl was run with %q, want one try-restart of systemd-resolved", b)
	}

	// A file of the user's own stays, on either tier.
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("nameserver 10.0.0.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("true", "127.0.0.1")
	check("the user's own, confined", "nameserver 10.0.0.53\n", false)
	run("true", "")
	check("the user's own, open", "nameserver 10.0.0.53\n", false)

	// A link to systemd-resolved's other file, which lists its servers
	// rather than the stub, stays as it is.
	full := filepath.Join(filepath.Dir(stub), "resolv.conf")
	if err := os.WriteFile(full, []byte("nameserver 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relink(t, conf, full)
	run("true", "127.0.0.1")
	if to, _ := os.Readlink(conf); to != full {
		t.Errorf("a link to resolved's own resolv.conf leads to %q, want %q", to, full)
	}

	// Without the stub's file (no systemd-resolved), a link elsewhere
	// stays, even one that leads nowhere.
	if err := os.Remove(stub); err != nil {
		t.Fatal(err)
	}
	relink(t, conf, "/run/resolvconf/resolv.conf")
	run("true", "127.0.0.1")
	if to, err := os.Readlink(conf); err != nil || to != "/run/resolvconf/resolv.conf" {
		t.Errorf("without the stub: resolv.conf leads to %q (%v), want the link it had", to, err)
	}

	// devvm's file alone is enough to go back, on an open VM without the
	// stub's file, which says names may not resolve, and on a guest
	// without systemd, which restarts nothing.
	for _, tc := range []struct{ systemd, resolver string }{{"true", ""}, {"false", "127.0.0.1"}} {
		if err := os.Remove(conf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(conf, []byte(own), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out := run(tc.systemd, tc.resolver)
		if to, _ := os.Readlink(conf); to != stub {
			t.Errorf("systemd=%s: devvm's file without the stub leads to %q, want %q", tc.systemd, to, stub)
		}
		if !strings.Contains(out, "systemd-resolved hasn't made") {
			t.Errorf("systemd=%s: no warning that names may not resolve: %q", tc.systemd, out)
		}
		b, _ := os.ReadFile(calls) //nolint:gosec // the test's own file
		if want := map[string]string{"true": "try-restart systemd-resolved\n", "false": ""}[tc.systemd]; string(b) != want {
			t.Errorf("systemd=%s: systemctl was run with %q, want %q", tc.systemd, b, want)
		}
	}

	// A write that fails leaves the link, takes its temporary file away,
	// and says so, and the setup goes on.
	if err := os.WriteFile(stub, []byte(stubText), 0o600); err != nil {
		t.Fatal(err)
	}
	relink(t, conf, stub)
	if err := os.Chmod(filepath.Dir(conf), 0o500); err != nil { //nolint:gosec // the test's own folder, made read-only
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(conf), 0o700) //nolint:errcheck,gosec // TempDir's cleanup needs it writable again
	out := run("true", "127.0.0.1")
	if to, _ := os.Readlink(conf); to != stub || !strings.Contains(out, "couldn't write") {
		t.Errorf("a failed write: resolv.conf leads to %q, said %q", to, out)
	}
	if _, err := os.Lstat(conf + ".devvm"); !os.IsNotExist(err) {
		t.Errorf("a failed write left %s.devvm (%v)", conf, err)
	}
}

// relink makes link a link to target, in place of whatever it was.
func relink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
