package proxy

// confineSelf is how the child is confined once it has confined itself in the process: on Linux,
// where nothing confines it yet, as its broker started it (spawned), which is unconfined.
func confineSelf(spawned Confinement) Confinement { return spawned }
