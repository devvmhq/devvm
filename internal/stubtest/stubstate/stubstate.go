// Package stubstate is the state the test stubs keep between their calls
// (internal/stubtest/stub): Lima's instances, the Keychain's items, the
// SSH agent's keys and Time Machine's exclusions. It's one typed JSON
// file in the stubs' folder, STUB_DIR, read and written under a lock, so
// stubs running at once never lose each other's changes. Tests read and
// change it through internal/stubtest, and scripts through the stub
// command.
package stubstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// File and lock are the state and its lock, in the stubs' folder.
const (
	File = "state.json"
	lock = "state.lock"
)

// The size an instance has until create or edit sets one: Lima's
// defaults, 4 CPUs, 8 GiB of memory and a 100 GiB disk.
const (
	DefaultCPUs   = 4
	DefaultMemory = 8 << 30
	DefaultDisk   = 100 << 30
)

// State is everything the stubs keep.
type State struct {
	// Instances are Lima's, by name.
	Instances map[string]*Instance `json:"instances,omitempty"`
	// ToolsOut is the folder the shared tools' build VM mounts, where its
	// build.sh writes the images.
	ToolsOut string `json:"toolsOut,omitempty"`
	// Keychain is the Keychain's generic passwords, by account.
	Keychain map[string]string `json:"keychain,omitempty"`
	// Agent is the SSH agent ssh-agent runs, nil when none does.
	Agent *Agent `json:"agent,omitempty"`
	// Excluded are the folders Time Machine leaves out.
	Excluded []string `json:"excluded,omitempty"`
}

// An Instance is one of Lima's.
type Instance struct {
	Status string `json:"status"`
	CPUs   int    `json:"cpus"`
	Memory int64  `json:"memory"` // bytes
	Disk   int64  `json:"disk"`   // bytes
	// HostAgentPID and DriverPID are the pids list --json shows, 0 for
	// none: a broken instance's say which of its processes run.
	HostAgentPID int `json:"hostAgentPID,omitempty"`
	DriverPID    int `json:"driverPID,omitempty"`
	// Config is what create's, clone's and edit's --set put in its
	// config: each key's JSON, as --set gave it, by its dotted path
	// (ssh.forwardAgent for ssh's forwardAgent).
	Config map[string]string `json:"config,omitempty"`
}

// NewInstance is an instance in status with Lima's default size.
func NewInstance(status string) *Instance {
	return &Instance{Status: status, CPUs: DefaultCPUs, Memory: DefaultMemory, Disk: DefaultDisk}
}

// An Agent is a running SSH agent: its socket, and the keys it holds, as
// ssh-add -l lists them ("256 SHA256:fp-KEY KEY (ED25519)").
type Agent struct {
	Socket string   `json:"socket"`
	Keys   []string `json:"keys,omitempty"`
}

// Read returns the state in dir, empty when there's none yet.
func Read(dir string) (*State, error) {
	var st *State
	err := Update(dir, func(s *State) error { st = s; return errUnchanged })
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return st, err
}

// errUnchanged ends an update without writing.
var errUnchanged = errors.New("unchanged")

// Update calls fn with the state in dir, then writes what fn left, all
// under the state's lock; when fn fails, nothing is written.
func Update(dir string, fn func(*State) error) error {
	l, err := os.OpenFile(filepath.Join(dir, lock), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the stubs' own folder
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	st := &State{}
	data, err := os.ReadFile(filepath.Join(dir, File)) //nolint:gosec // the stubs' own folder
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, st); err != nil {
			return err
		}
	}
	if st.Instances == nil {
		st.Instances = map[string]*Instance{}
	}
	if st.Keychain == nil {
		st.Keychain = map[string]string{}
	}
	if err := fn(st); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(dir, File+".new")
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, File))
}
