package image

import (
	"fmt"
	"io/fs"
	"slices"
)

// preludeFile is devvm's prelude, in its own files: $devvm_arch,
// $devvm_libc and devvm_sha, for the scripts it comes before.
const preludeFile = "prelude.sh"

// OwnScript is devvm's own script name, from own (devvm's files), as an
// image's build runs it with bash: the prelude, then the script. Each of
// devvm's scripts that a build runs with bash is read through it (base.sh,
// apt.sh, the built-in layers and features, shared-tools/build.sh,
// oci/build.sh), so the recipes and keys that hold a script hold the
// prelude too. A user's layer gets it through WithPrelude.
func OwnScript(own fs.FS, name string) ([]byte, error) {
	script, err := fs.ReadFile(own, name)
	if err != nil {
		return nil, fmt.Errorf("reading devvm's %s: %w", name, err)
	}
	return WithPrelude(own, script)
}

// WithPrelude is script as an image's build runs it with bash: the
// prelude from own (devvm's files), then the script, which may be the
// user's own layer. The prelude comes first, so whatever the script sets
// afterwards, a devvm_ name included, is the script's.
func WithPrelude(own fs.FS, script []byte) ([]byte, error) {
	prelude, err := fs.ReadFile(own, preludeFile)
	if err != nil {
		return nil, fmt.Errorf("reading devvm's %s: %w", preludeFile, err)
	}
	return slices.Concat(prelude, script), nil
}

// Recipe is what an image is made of, built once (internal/build): its
// key hashes it (Input), and its backend runs it as it is
// (backend.ImageSpec).
type Recipe struct {
	// Origin is base's first steps, its backend's own
	// (backend.Images's Origin): what the backend makes base's disk from,
	// such as Lima's base.yaml, its pinned Ubuntu image and a --from
	// image's conversion, each run as that backend says. None above base.
	Origin []Step
	// Steps run in order in the image's VM: base.sh and then base's
	// features, or a layer's script.
	Steps []Step
}

// Step is one step of a recipe: a script fed on its standard input to
// bash with Args, as the VM's user, who has sudo, or as root. An origin
// step is the backend's to run as it says.
type Step struct {
	Name string // for status lines and errors: base.sh
	Data []byte
	Args []string
	Root bool
}
