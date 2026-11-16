package cli

import (
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/lima"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/testbuild"
	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/packaging"
)

// A release, as this test binary is (built without the test build's
// flag), reads none of the test overrides, whatever its environment
// says: its mise is the pinned one, its backends check the real
// /dev/kvm and its host is this machine. The CLI tests' test build reads
// them all (the scripts).
func TestReleaseReadsNoOverrides(t *testing.T) {
	if testbuild.On() {
		t.Skip("a test build")
	}
	if o, err := readOverrides(func(string) string { return "/x" }); o != (Overrides{}) || err != nil {
		t.Errorf("readOverrides = %+v, %v", o, err)
	}
	for _, k := range []string{
		"DEVVM_HOST_MISE", "DEVVM_KVM", "DEVVM_UBUNTU_URL", "DEVVM_UNREVIEWED",
		"DEVVM_REPO_URL", "DEVVM_RELEASE_KEY", "DEVVM_RELEASE_CA", "DEVVM_TEST_VERSION", "DEVVM_HISTORY_MAX",
		platform.TestCPUs, platform.TestMemoryGiB, platform.TestFreeGiB, platform.TestListWait,
	} {
		t.Setenv(k, "1")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DEVVM_LIMA_HOME", t.TempDir())
	u, _, _ := testUI()
	a := newApp(u)
	tl, err := a.tools()
	if err != nil {
		t.Fatal(err)
	}
	if tl.Mise != "" {
		t.Errorf("mise = %q", tl.Mise)
	}
	r, err := a.registry()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Lookup("lima")
	if l, _ := backend.As[*lima.Lima](b); l.KVM != "/dev/kvm" || len(r.Unreviewed) != 0 {
		t.Errorf("KVM %q, let through %v", l.KVM, r.Unreviewed)
	}
	if got, want := a.host().CPUs(), (platform.Host{}).CPUs(); got != want {
		t.Errorf("the host's CPUs = %d, want %d", got, want)
	}
	// devvm upgrade's releases are GitHub's, checked with the key built in.
	up, err := a.upgrader()
	if err != nil {
		t.Fatal(err)
	}
	if up.Source.URL != upgrade.Repo || up.Source.Key != packaging.MinisignKey() || up.Current != devvmVersion() {
		t.Errorf("upgrade's source %q, key %q, version %q", up.Source.URL, up.Source.Key, up.Current)
	}
}
