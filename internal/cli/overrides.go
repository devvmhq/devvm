package cli

import (
	"strconv"

	"github.com/devvmhq/devvm/internal/backend/registry"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/testbuild"
)

// Overrides are the stand-ins the tests put in a test build's
// environment, in place of what devvm would find on this machine:
// the backends' (registry.Overrides) and the host's mise, and the way
// the CLI tests answer questions. readOverrides is the only place devvm
// reads them, and only a test build (testbuild.On) does: a release has
// none, whatever its environment says. A run reads them once, through
// its app.
type Overrides struct {
	registry.Overrides
	// HostMise (DEVVM_HOST_MISE) stands in for devvm's pinned mise.
	HostMise string
	// PipedAnswers (DEVVM_PIPED_ANSWERS=1) answers questions from input
	// that isn't a terminal, a line each (ui.Flags), where devvm would
	// refuse them.
	PipedAnswers bool
	// PickOnPipe (DEVVM_PICK_ON_PIPE=1) shows the picker with a terminal
	// on stdin alone (ui.Flags), as testscript's ttyin gives.
	PickOnPipe bool
	// Release is devvm upgrade's release source and this devvm's version.
	Release ReleaseOverrides
	// HistoryMax (DEVVM_HISTORY_MAX) is the most of an export of agents'
	// history devvm keeps, in bytes (vm.Env.HistoryMax), in place of 2 GiB.
	HistoryMax int
	// ClipTools (DEVVM_CLIP_TOOLS) is the folder devvm paste runs the
	// clipboard tools from (platform.Clipboard's Tools), in place of the
	// system's, so the tests' stand-ins never reach the real clipboard.
	ClipTools string
	// PasteWait (DEVVM_PASTE_WAIT) is how long devvm paste waits for
	// paste.sh in the VM, in seconds (vm.Env.PasteWait), in place of 10
	// minutes for a write and 2 for a count or a delete.
	PasteWait int
}

// ReleaseOverrides are devvm upgrade's stand-ins: a fake release server
// in place of GitHub's, which a CA of the test's own signs, a test key in
// place of the release key, and a release's version in place of a test
// build's own.
type ReleaseOverrides struct {
	// URL (DEVVM_REPO_URL) is the repository whose releases are read, as
	// install.sh takes it.
	URL string
	// Key (DEVVM_RELEASE_KEY) is the minisign public key releases are
	// checked against.
	Key string
	// CA (DEVVM_RELEASE_CA) is a PEM file of the CA the release server's
	// certificate is signed by.
	CA string
	// Version (DEVVM_TEST_VERSION) is the version this devvm says it is.
	Version string
}

// readOverrides reads a test build's overrides with getenv; a release's
// are empty.
func readOverrides(getenv func(string) string) Overrides {
	if !testbuild.On() {
		return Overrides{}
	}
	historyMax, _ := strconv.Atoi(getenv("DEVVM_HISTORY_MAX"))
	pasteWait, _ := strconv.Atoi(getenv("DEVVM_PASTE_WAIT"))
	return Overrides{
		Overrides: registry.Overrides{
			Host: platform.Overrides{
				CPUs: getenv(platform.TestCPUs), MemoryGiB: getenv(platform.TestMemoryGiB),
				FreeGiB: getenv(platform.TestFreeGiB), ListTimeout: getenv(platform.TestListWait),
			},
			Unreviewed:    getenv("DEVVM_UNREVIEWED"),
			KVM:           getenv("DEVVM_KVM"),
			UbuntuURL:     getenv("DEVVM_UBUNTU_URL"),
			UbuntuRelease: getenv("DEVVM_UBUNTU_RELEASE"),
			Pins:          getenv("DEVVM_PINS"),
		},
		HostMise:     getenv("DEVVM_HOST_MISE"),
		PipedAnswers: getenv("DEVVM_PIPED_ANSWERS") == "1",
		PickOnPipe:   getenv("DEVVM_PICK_ON_PIPE") == "1",
		Release: ReleaseOverrides{
			URL: getenv("DEVVM_REPO_URL"), Key: getenv("DEVVM_RELEASE_KEY"),
			CA: getenv("DEVVM_RELEASE_CA"), Version: getenv("DEVVM_TEST_VERSION"),
		},
		HistoryMax: historyMax,
		ClipTools:  getenv("DEVVM_CLIP_TOOLS"),
		PasteWait:  pasteWait,
	}
}
