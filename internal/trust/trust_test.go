package trust

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// cert returns a new self-signed certificate, a CA's when ca, PEM.
func cert(t *testing.T, name string, ca bool) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: ca,
	}
	if ca {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func key(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	a, b := cert(t, "a", true), cert(t, "b", true)
	cases := []struct {
		name  string
		files map[string][]byte
		want  [][]byte
		err   string // the message's ID
	}{
		{"no folder", nil, nil, ""},
		{"empty", map[string][]byte{}, nil, ""},
		{"a CA", map[string][]byte{"a.pem": a}, [][]byte{a}, ""},
		{"two files, by name", map[string][]byte{"z.crt": a, "b.pem": b}, [][]byte{b, a}, ""},
		{"a bundle with comments", map[string][]byte{"c.pem": slicesJoin([]byte("# Company root\n"), a, []byte("# next\n"), b)}, [][]byte{a, b}, ""},
		{"each once", map[string][]byte{"a.pem": a, "b.crt": a}, [][]byte{a}, ""},
		{"other names left alone", map[string][]byte{"a.pem": a, "key.key": key(t), "notes.txt": []byte("x")}, [][]byte{a}, ""},
		{"a key", map[string][]byte{"a.pem": slicesJoin(a, key(t))}, nil, "ca_not_certificate"},
		{"not PEM", map[string][]byte{"a.crt": []byte("hello")}, nil, "ca_not_certificate"},
		{"not a CA", map[string][]byte{"a.pem": cert(t, "leaf", false)}, nil, "ca_not_ca"},
		{"unparseable", map[string][]byte{"a.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})}, nil, "ca_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ca")
			if c.files != nil {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				for n, d := range c.files {
					write(t, dir, n, d)
				}
			}
			got, err := Read(dir)
			if c.err != "" {
				if msg.IDOf(err) != c.err {
					t.Fatalf("err = %v, want %s", err, c.err)
				}
				if !strings.Contains(msg.NamesOf(err), "file="+dir) {
					t.Errorf("message %s doesn't name the file", msg.NamesOf(err))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d CAs, want %d", len(got), len(c.want))
			}
			for i := range got {
				if !bytes.Equal(got[i], c.want[i]) {
					t.Errorf("CA %d differs", i)
				}
			}
		})
	}
}

func slicesJoin(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// Links are never followed: a file that's a link (into a project folder,
// whose VM could write it), a dangling one, or a CA folder that's a link
// fail the read, named. So do a file and a folder it can't read, with
// the error carried.
func TestReadRefuses(t *testing.T) {
	a := cert(t, "a", true)
	elsewhere := t.TempDir()
	write(t, elsewhere, "project.pem", a)
	link := func(target, name string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name   string
		folder bool // the CA folder itself, else a file in it
		setup  func(t *testing.T, dir string)
		want   string // the message's names, %s the path
		cause  bool   // it carries an error
	}{
		{"a file's link", false, link(filepath.Join(elsewhere, "project.pem"), "a.pem"), "ca_link file=%s/a.pem", false},
		{"a dangling link", false, link(filepath.Join(elsewhere, "gone.pem"), "a.crt"), "ca_link file=%s/a.crt", false},
		{"an unreadable file", false, func(t *testing.T, dir string) {
			write(t, dir, "a.pem", a)
			if err := os.Chmod(filepath.Join(dir, "a.pem"), 0); err != nil {
				t.Fatal(err)
			}
		}, "ca_unreadable file=%s/a.pem", true},
		{"an unreadable folder", true, func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // the test's own folder
		}, "ca_folder_unreadable dir=%s", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if os.Geteuid() == 0 && c.cause {
				t.Skip("root reads anything")
			}
			dir := filepath.Join(t.TempDir(), "ca")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			c.setup(t, dir)
			_, err := Read(dir)
			var m msg.M
			if !errors.As(err, &m) || m.Names() != fmt.Sprintf(c.want, dir) {
				t.Fatalf("err = %v, want %s", err, fmt.Sprintf(c.want, dir))
			}
			if got := errors.Unwrap(m) != nil; got != c.cause {
				t.Errorf("carries an error: %v, want %v", got, c.cause)
			}
		})
	}
	// The folder a link: refused, even to a folder of good CAs.
	parent := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(parent, "ca")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "ca")
	_, err := Read(dir)
	if msg.NamesOf(err) != "ca_folder_link dir="+dir {
		t.Errorf("a linked folder: %v", err)
	}
}

// Only what's parsed leaves: text around a certificate isn't sent.
func TestReadSendsOnlyTheCertificate(t *testing.T) {
	a := cert(t, "a", true)
	dir := t.TempDir()
	write(t, dir, "a.pem", slicesJoin([]byte("secret notes\n"), a, []byte("more\n")))
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(got[0], a) {
		t.Fatalf("got %q", got)
	}
}

func TestGuestEnv(t *testing.T) {
	got := GuestEnv(map[string]string{"NO_PROXY": "x"})
	if got["NO_PROXY"] != "x" || got["NODE_EXTRA_CA_CERTS"] != Bundle || got["SSL_CERT_FILE"] != System {
		t.Errorf("GuestEnv = %v", got)
	}
	if got := GuestEnv(nil); len(got) != len(Env) {
		t.Errorf("GuestEnv(nil) = %v", got)
	}
}

func TestBuild(t *testing.T) {
	assets := fstest.MapFS{"guest/ca.sh": {Data: []byte("echo ca.sh\n")}}
	none, err := CAs(nil).Build(assets)
	if err != nil || !none.None() || len(none.Remove.Data) != 0 || none.Env != nil {
		t.Fatalf("no CAs: %+v, %v", none, err)
	}
	a := cert(t, "a", true)
	cas := CAs{a, cert(t, "b", true)}
	tr, err := cas.Build(assets)
	if err != nil {
		t.Fatal(err)
	}
	add := string(tr.Add.Data)
	for _, want := range []string{"/" + Name(0) + "\" <<'DEVVM_CA'\n" + string(a) + "DEVVM_CA\n", Name(1), "set -- \"$d\"\necho ca.sh\n"} {
		if !strings.Contains(add, want) {
			t.Errorf("Add lacks %q:\n%s", want, add)
		}
	}
	if string(tr.Remove.Data) != "set --\necho ca.sh\n" {
		t.Errorf("Remove = %q", tr.Remove.Data)
	}
	env := string(tr.Env)
	// On one line, which the script's first shares.
	if !strings.HasPrefix(env, "export ") || !strings.HasSuffix(env, "; ") || strings.Contains(env, "\n") ||
		!strings.Contains(env, "export NODE_EXTRA_CA_CERTS="+Bundle+"; ") {
		t.Errorf("Env = %q", env)
	}
}

// caSh runs guest/ca.sh in root, a folder standing for /, with args, and
// the commands on PATH that a VM has; stubs holds update-ca-certificates
// when it's to have one.
func caSh(t *testing.T, root, stubs string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{filepath.Join("..", "..", "assets", "guest", "ca.sh")}, args...)...) //nolint:gosec // devvm's own script, the test's folders
	cmd.Env = []string{"DEVVM_CA_ROOT=" + root, "PATH=" + stubs + ":/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ca.sh %v: %v\n%s", args, err, out)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own files
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Without update-ca-certificates, the bundle itself gets them, and is put
// back as it was once they go.
func TestCAShBundleOnly(t *testing.T) {
	root, src, stubs := t.TempDir(), t.TempDir(), t.TempDir()
	certs := filepath.Join(root, "etc", "ssl", "certs")
	if err := os.MkdirAll(certs, 0o700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(certs, "ca-certificates.crt")
	write(t, certs, "ca-certificates.crt", []byte("SYSTEM\n"))
	write(t, src, "devvm-extra-1.crt", []byte("ONE\n"))
	write(t, src, "devvm-proxy.crt", []byte("PROXY\n"))

	caSh(t, root, stubs, src)
	if got := read(t, bundle); got != "SYSTEM\nONE\nPROXY\n" {
		t.Errorf("bundle = %q", got)
	}
	if got := read(t, filepath.Join(root, "etc", "ssl", "devvm.pem")); got != "ONE\nPROXY\n" {
		t.Errorf("devvm.pem = %q", got)
	}
	// Again: the same, the bundle kept once.
	caSh(t, root, stubs, src)
	if got := read(t, bundle); got != "SYSTEM\nONE\nPROXY\n" {
		t.Errorf("bundle again = %q", got)
	}
	// One gone.
	if err := os.Remove(filepath.Join(src, "devvm-proxy.crt")); err != nil {
		t.Fatal(err)
	}
	caSh(t, root, stubs, src)
	if got := read(t, bundle); got != "SYSTEM\nONE\n" {
		t.Errorf("bundle with one = %q", got)
	}
	if exists(filepath.Join(root, "usr", "local", "share", "ca-certificates", "devvm-proxy.crt")) {
		t.Error("devvm-proxy.crt stayed")
	}
	// None, as an image keeps it.
	caSh(t, root, stubs)
	if got := read(t, bundle); got != "SYSTEM\n" {
		t.Errorf("bundle after = %q", got)
	}
	for _, p := range []string{"etc/ssl/devvm.pem", "var/lib/devvm-ca", "usr/local/share/ca-certificates/devvm-extra-1.crt"} {
		if exists(filepath.Join(root, p)) {
			t.Errorf("%s stayed", p)
		}
	}
}

// A build's scripts, fed to sh as backends feed them: the image is as it
// was once Remove ran.
func TestBuildScriptsRun(t *testing.T) {
	ca, err := os.ReadFile(filepath.Join("..", "..", "assets", "guest", "ca.sh"))
	if err != nil {
		t.Fatal(err)
	}
	a := cert(t, "a", true)
	tr, err := CAs{a}.Build(fstest.MapFS{"guest/ca.sh": {Data: ca}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	run := func(s backend.Script) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "sh", "-s")
		cmd.Stdin = bytes.NewReader(s.Data)
		cmd.Env = []string{"DEVVM_CA_ROOT=" + root, "PATH=" + t.TempDir() + ":/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	run(tr.Add)
	if got := read(t, filepath.Join(root, "usr", "local", "share", "ca-certificates", Name(0))); got != string(a) {
		t.Errorf("installed %q", got)
	}
	run(tr.Remove)
	for _, p := range []string{"etc/ssl/devvm.pem", "etc/ssl/certs/ca-certificates.crt", "usr/local/share/ca-certificates/" + Name(0)} {
		if exists(filepath.Join(root, p)) {
			t.Errorf("%s stayed", p)
		}
	}
}

// A container image with no bundle at all: none again once they go.
func TestCAShNoBundle(t *testing.T) {
	root, src, stubs := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, src, "devvm-extra-1.crt", []byte("ONE\n"))
	caSh(t, root, stubs, src)
	bundle := filepath.Join(root, "etc", "ssl", "certs", "ca-certificates.crt")
	if got := read(t, bundle); got != "ONE\n" {
		t.Errorf("bundle = %q", got)
	}
	caSh(t, root, stubs)
	if exists(bundle) {
		t.Error("the bundle stayed")
	}
}

// With update-ca-certificates, it's run from scratch when devvm's change,
// and not otherwise; devvm.pem is there, empty, with a DIR of none.
func TestCAShUpdate(t *testing.T) {
	root, src, stubs := t.TempDir(), t.TempDir(), t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	write(t, stubs, "update-ca-certificates", []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"))
	if err := os.Chmod(filepath.Join(stubs, "update-ca-certificates"), 0o700); err != nil { //nolint:gosec // a stub the test runs
		t.Fatal(err)
	}
	calls := func() string {
		b, _ := os.ReadFile(log) //nolint:gosec // the test's own file
		return string(b)
	}
	caSh(t, root, stubs, src)
	if calls() != "" {
		t.Errorf("nothing changed, but: %q", calls())
	}
	if got := read(t, filepath.Join(root, "etc", "ssl", "devvm.pem")); got != "" {
		t.Errorf("devvm.pem = %q", got)
	}
	write(t, src, "devvm-extra-1.crt", []byte("ONE\n"))
	caSh(t, root, stubs, src)
	caSh(t, root, stubs, src)
	if calls() != "--fresh\n" {
		t.Errorf("calls = %q", calls())
	}
	caSh(t, root, stubs)
	if calls() != "--fresh\n--fresh\n" {
		t.Errorf("calls after = %q", calls())
	}
	if exists(filepath.Join(root, "usr", "local", "share", "ca-certificates", "devvm-extra-1.crt")) {
		t.Error("devvm-extra-1.crt stayed")
	}
}

// update-ca-certificates tells its hooks nothing of a removed file, so
// ca.sh tells each one (run-parts' names only) of the CAs it removed, a
// "-/etc/ssl/certs/NAME.pem" line each, as Debian's does; a CA added or
// kept tells them nothing, and a hook that fails stops nothing.
func TestCAShHooks(t *testing.T) {
	root, src, stubs := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, stubs, "update-ca-certificates", []byte("#!/bin/sh\n"))
	hooks := filepath.Join(root, "etc", "ca-certificates", "update.d")
	log := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"jks-keystore":     "sed 's/^/jks /' >> " + log,
		"mono-keystore":    "sed 's/^/mono /' >> " + log + "; exit 1",
		"skipped.dpkg-old": "sed 's/^/old /' >> " + log,
	} {
		write(t, hooks, name, []byte("#!/bin/sh\n"+body+"\n"))
	}
	for _, d := range []string{stubs, hooks} {
		es, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if err := os.Chmod(filepath.Join(d, e.Name()), 0o700); err != nil { //nolint:gosec // stubs the test runs
				t.Fatal(err)
			}
		}
	}
	got := func() string {
		b, _ := os.ReadFile(log) //nolint:gosec // the test's own file
		return string(b)
	}
	write(t, src, "devvm-extra-0.crt", []byte("ZERO\n"))
	write(t, src, "devvm-proxy.crt", []byte("PROXY\n"))
	caSh(t, root, stubs, src)
	if got() != "" {
		t.Errorf("hooks told of an add: %q", got())
	}
	if err := os.Remove(filepath.Join(src, "devvm-extra-0.crt")); err != nil {
		t.Fatal(err)
	}
	caSh(t, root, stubs, src)
	if want := "jks -/etc/ssl/certs/devvm-extra-0.pem\nmono -/etc/ssl/certs/devvm-extra-0.pem\n"; got() != want {
		t.Errorf("hooks got %q, want %q", got(), want)
	}
	caSh(t, root, stubs)
	if want := "jks -/etc/ssl/certs/devvm-extra-0.pem\nmono -/etc/ssl/certs/devvm-extra-0.pem\n" +
		"jks -/etc/ssl/certs/devvm-proxy.pem\nmono -/etc/ssl/certs/devvm-proxy.pem\n"; got() != want {
		t.Errorf("hooks after removing all got %q, want %q", got(), want)
	}
}
