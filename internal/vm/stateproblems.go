package vm

import (
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// StateProblems are devvm doctor's warnings for the VMs whose state devvm can't read: one for
// each VM whose vm.json can't be read, or has a section that its package can't read, as every
// command checks it as it finds the folder's VM (checkState), naming the VM and why, which names
// the file. A value that reads but that a launch refuses, such as a size, isn't checked. It only
// reads. A VM whose folder has no state, or anything else in the VMs' folder, is
// state.VMFolderProblems'.
func StateProblems(d paths.Dirs) []msg.M {
	inv, err := ReadStates(d)
	if err != nil {
		return nil // the VMs' folder itself, which state.VMFolderProblems warns of
	}
	var ms []msg.M
	for _, k := range inv.VMs {
		name := k.Hash
		if k.Name != "" {
			name = k.Name.String()
		}
		err := k.Err
		if err == nil {
			err = checkState(k.State)
		}
		if err != nil {
			ms = append(ms, msg.VMStateBroken(name, err.Error()).Because(err))
		}
	}
	return ms
}
