package secrets

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
)

// storeFixture is a secret a release's store on Linux holds as a file, in
// its frozen state (testdata/v<release>/linux, which that release itself
// made: never regenerated), and the VM whose bindings ask for it.
type storeFixture struct {
	release, vmHash, secret, value string
}

// storeFixtures are every release's: one for each release with a frozen
// state.
func storeFixtures() []storeFixture {
	return []storeFixture{
		// 0.1.0's app VM and its github secret.
		{release: "0.1.0", vmHash: "6a14d8553d070e", secret: "github", value: "ghp_testfakev010"},
	}
}

// Every release with a frozen state has its secret in storeFixtures.
func TestStoreFixturesListed(t *testing.T) {
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v*", "linux"))
	if err != nil || len(found) == 0 {
		t.Fatalf("no release's state on Linux: %v", err)
	}
	for _, f := range found {
		release := strings.TrimPrefix(filepath.Base(filepath.Dir(f)), "v")
		if !slices.ContainsFunc(storeFixtures(), func(s storeFixture) bool { return s.release == release }) {
			t.Errorf("%s: not in storeFixtures", f)
		}
	}
}

// A release's store on Linux, where a secret is a file, gives a VM's
// proxy the value its binding asks for: the binding that release wrote,
// without the ID a secret of yours has, still fits the secret as it's
// defined today. The copy is the test's own, its files private as the
// release left them (the fixture's modes, which git doesn't keep).
func TestFixtureStoreGet(t *testing.T) {
	for _, f := range storeFixtures() {
		t.Run("v"+f.release, func(t *testing.T) { checkStoreFixture(t, f) })
	}
}

// checkStoreFixture checks f's secret, as TestFixtureStoreGet says.
func checkStoreFixture(t *testing.T, f storeFixture) {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "v"+f.release, "linux")
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	d := paths.Dirs{
		Home: filepath.Join(dst, "home"), Data: filepath.Join(dst, "data", "devvm"),
		Config: filepath.Join(dst, "config", "devvm"),
	}
	bindings := filepath.Join(d.Data, "vms", f.vmHash, "run", "secrets.json")
	for _, file := range []string{filepath.Join(d.Secrets(), f.secret), bindings} {
		if err := os.Chmod(file, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := proxy.ReadBindings(bindings)
	if err != nil {
		t.Fatalf("ReadBindings(%s) = %v", f.vmHash, err)
	}
	s := NewStore(d, &msg.Fake{OnWarn: func(m msg.M) { t.Log(m.Names()) }})
	s.Keychain = false
	s.Getenv = func(string) string { return "" }
	var found bool
	for _, b := range list.Bindings {
		if b.Name != f.secret {
			continue
		}
		found = true
		if v, err := s.Get(t.Context(), b); v.Reveal() != f.value || err != nil {
			t.Errorf("Get(%+v) = %q, %v", b, v.Reveal(), err)
		}
	}
	if !found {
		t.Fatalf("%s's bindings have no %s: %+v", f.vmHash, f.secret, list.Bindings)
	}
	// The secret's file, read and saved again, has the release's bytes.
	file := filepath.Join(d.Secrets(), f.secret)
	before, err := os.ReadFile(file) //nolint:gosec // the test's own copy of the fixture
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Value(t.Context(), f.secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(t.Context(), f.secret, v); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(file) //nolint:gosec // the test's own copy of the fixture
	if string(after) != string(before) {
		t.Errorf("%s saved again as %q, the release wrote %q", f.secret, after, before)
	}
}
