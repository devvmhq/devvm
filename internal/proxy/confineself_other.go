//go:build !linux

package proxy

// confineSelf is how the child is confined once it has confined itself in the process: here it
// can't, so it's as its broker started it (spawned), under Seatbelt on macOS.
func confineSelf(spawned Confinement) Confinement { return spawned }
