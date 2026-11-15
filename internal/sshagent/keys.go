package sshagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/state"
)

// AddedKey is a key added to devvm's own agent, as agent-keys.json in
// devvm's data folder records it: its file, an absolute path, its
// fingerprint when it was added (SHA256:…), and how it's loaded.
type AddedKey struct {
	File        string `json:"file"`
	Fingerprint string `json:"fingerprint,omitempty"`
	AddOptions
}

// keysFile is agent-keys.json.
type keysFile struct {
	Keys []AddedKey `json:"keys"`
}

// added reads the keys added to the agent. A missing file has none; a key
// devvm can't use (a relative path, a lifetime it doesn't take) is a
// state.CorruptError naming the file.
func (a *Agent) added() ([]AddedKey, error) {
	path := a.Dirs.AgentKeys()
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var f keysFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, &state.CorruptError{Path: path, Reason: err.Error()}
	}
	for i, k := range f.Keys {
		if err := k.check(); err != nil {
			return nil, &state.CorruptError{Path: path, Reason: fmt.Sprintf("keys[%d]: %v", i, err)}
		}
	}
	return f.Keys, nil
}

// saveAdded replaces agent-keys.json with keys.
func (a *Agent) saveAdded(keys []AddedKey) error {
	for _, k := range keys {
		if err := k.check(); err != nil {
			return err
		}
	}
	data, _ := json.MarshalIndent(keysFile{Keys: append([]AddedKey{}, keys...)}, "", "  ")
	return state.WriteFile(a.Dirs.AgentKeys(), append(data, '\n'))
}

// check refuses a key whose file isn't an absolute path, or whose lifetime
// --lifetime doesn't take.
func (k AddedKey) check() error {
	if !filepath.IsAbs(k.File) {
		return fmt.Errorf("key file %q is not an absolute path", k.File)
	}
	if k.Lifetime != "" {
		return CheckLifetime(k.Lifetime)
	}
	return nil
}

// lifetime is how long the agent keeps the key once loaded: its own, else
// DefaultLifetime.
func (k AddedKey) lifetime() string {
	if k.Lifetime != "" {
		return k.Lifetime
	}
	return DefaultLifetime
}
