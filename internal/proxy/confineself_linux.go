package proxy

import (
	"errors"

	"github.com/devvmhq/devvm/internal/landlock"
)

// confineSelf confines the child in the process, to the folders it needs (n): on Linux with
// Landlock and the seccomp filter (landlock.Self), which every thread of it gets, so it's
// confined as it says (landlockConfinement). A build that can't reach every thread is as its
// broker started it (spawned), unconfined; anything else that fails is an error.
func confineSelf(spawned Confinement, n ChildNeeds) (Confinement, error) {
	r, err := landlock.Self(n.Folders)
	switch {
	case errors.Is(err, landlock.ErrUnsupported):
		return spawned, nil
	case err != nil:
		return Confinement{}, err
	}
	return landlockConfinement(r), nil
}
