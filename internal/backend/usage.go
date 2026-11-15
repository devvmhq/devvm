package backend

import "github.com/devvmhq/devvm/internal/vmname"

// Usage is the disk a backend's VMs take on this machine, and its own
// caches, as devvm space shows them (Housekeeping.Usage).
type Usage struct {
	// VMs is the disk each of devvm's VMs takes, by name. A VM it can't
	// tell is missing.
	VMs map[vmname.VM]int64
	// Caches are the backend's own, which devvm's builds fill but which
	// can also hold the user's own downloads.
	Caches []Cache
}

// Cache is one of a backend's caches.
type Cache struct {
	Name  string // what it holds, as devvm space shows it
	Path  string // where it is, "" for one in the backend's engine
	Count int    // its entries
	Size  int64  // the disk it takes, when Sized
	Sized bool
}
