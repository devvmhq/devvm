package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
)

const (
	customListVersion = 1       // Dirs.CustomSecrets' version: a list of another isn't read
	customListMax     = 1 << 20 // the most of Dirs.CustomSecrets read
)

// customList is Dirs.CustomSecrets: your secrets' definitions, in the
// order they were first saved, never their values.
type customList struct {
	Version int          `json:"version"`
	Secrets []customJSON `json:"secrets"`
}

type customJSON struct {
	Name string `json:"name"`
	// ID is the definition's own, made as it's first saved: a VM bound to
	// another one (after devvm secret rm and set) gets nothing of this one.
	ID     string   `json:"id"`
	Hosts  []string `json:"hosts"`
	Header string   `json:"header"`
	Env    string   `json:"env"`
	Prefix string   `json:"prefix,omitempty"`
	Scheme string   `json:"scheme,omitempty"`
	Path   string   `json:"path,omitempty"`
	// PathPattern marks Path as a pattern (proxy.PathOK), in which * is a
	// wildcard: every path devvm writes has it. A path without it was saved
	// as a prefix whose * was itself, so one holding a * is refused, not
	// read as a wider pattern than it was saved as.
	PathPattern bool   `json:"path_pattern,omitempty"`
	Methods     string `json:"methods,omitempty"`
	BasicUser   bool   `json:"basic_user,omitempty"`
}

// errPathPrefix is why a saved path whose * was a literal one isn't read as a pattern.
var errPathPrefix = errors.New("its path was saved as a prefix in which * is itself, not a wildcard")

// Yours are your secrets in the store s: their definitions in
// Dirs.CustomSecrets, their values where the known secrets' are, each
// under its storageKey.
type Yours struct{ s *Store }

// YoursOf is your secrets in s.
func YoursOf(s *Store) Yours { return Yours{s} }

// List returns your secrets, in the order they were first saved: none
// while Dirs.CustomSecrets isn't there. A list that can't be read as the
// proxy reads its own files (proxy.ReadPrivate: no symbolic link, yours,
// only you may write it) is custom_secrets_unreadable, and one that holds
// a secret Define refuses, or one without an ID, is custom_secret_refused.
func (y Yours) List() ([]Secret, error) {
	path := y.s.Dirs.CustomSecrets()
	b, err := proxy.ReadPrivate(path, customListMax, 0o022)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, msg.CustomSecretsUnreadable(path, err.Error()).Because(err)
	}
	var l customList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, msg.CustomSecretsUnreadable(path, err.Error()).Because(err)
	}
	if l.Version != customListVersion {
		err := errors.New("not a list of version 1")
		return nil, msg.CustomSecretsUnreadable(path, err.Error()).Because(err)
	}
	out := make([]Secret, 0, len(l.Secrets))
	for _, c := range l.Secrets {
		sec, err := Define(c.Name, Def{Hosts: c.Hosts, Header: c.Header, Env: c.Env, Prefix: c.Prefix, Scheme: c.Scheme, Path: c.Path, Methods: c.Methods, BasicUser: c.BasicUser})
		switch {
		case err != nil:
		case !idOK(c.ID):
			err = errors.New("no id")
		case strings.Contains(c.Path, "*") && !c.PathPattern:
			err = errPathPrefix
		}
		if err != nil {
			return nil, msg.CustomSecretRefused(path, c.Name).Because(err)
		}
		sec.ID = c.ID
		out = append(out, sec)
	}
	return out, nil
}

// newID is a new definition's ID: 16 random bytes, in hex.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return hex.EncodeToString(b)
}

// idOK reports whether id can be a definition's (newID's).
func idOK(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil
}

// All returns every secret: Known, then yours (List).
func (y Yours) All() ([]Secret, error) {
	cs, err := y.List()
	if err != nil {
		return nil, err
	}
	return slices.Concat(Known, cs), nil
}

// Using returns the secrets that names may name: Known, and yours (All)
// only when one of names isn't a known secret's, so a list of yours that
// can't be read fails only what uses one of yours.
func (y Yours) Using(names []string) ([]Secret, error) {
	for _, n := range names {
		if _, ok := Lookup(n); !ok {
			return y.All()
		}
	}
	return Known, nil
}

// Find returns the secret named name in list.
func Find(list []Secret, name string) (Secret, bool) {
	i := slices.IndexFunc(list, func(s Secret) bool { return s.Name == name })
	if i < 0 {
		return Secret{}, false
	}
	return list[i], true
}

// Lookup returns the secret by that name: a known one, or one of yours.
// A name no secret of yours could have (CheckName) reads nothing.
func (y Yours) Lookup(name string) (Secret, bool, error) {
	if sec, ok := Lookup(name); ok {
		return sec, true, nil
	}
	if !nameOK(name) {
		return Secret{}, false, nil
	}
	cs, err := y.List()
	if err != nil {
		return Secret{}, false, err
	}
	sec, ok := Find(cs, name)
	return sec, ok, nil
}

// HasValue reports whether a value is saved for the secret of yours named
// name, with its definition or without one (a save whose definition was
// never written, which devvm secret rm deletes).
func (y Yours) HasValue(ctx context.Context, name string) bool {
	if !nameOK(name) {
		return false
	}
	_, err := y.s.saved(ctx, name)
	return err == nil
}

// locked runs do while it holds CustomSecretsLock, with your secrets as
// they are then: what changes them, or binds a VM to one, does so under
// it, so what it read can't change meanwhile.
func (y Yours) locked(ctx context.Context, do func(cs []Secret) error) error {
	if err := state.PrivateDir(y.s.Dirs.Data); err != nil {
		return err
	}
	unlock, err := platform.LockFile(ctx, y.s.Dirs.CustomSecretsLock())
	if err != nil {
		return err
	}
	defer unlock()
	cs, err := y.List()
	if err != nil {
		return err
	}
	return do(cs)
}

// Save saves a secret of yours, sec (Define's), and its value (Token's),
// under CustomSecretsLock: the value where the known secrets' are
// (Store.save), then its definition, in place of one by its name, whose ID
// it keeps; a new one gets an ID of its own. When the definition of a new
// one can't be written, its value goes again. A variable another secret
// of yours gives VMs is secret_env_taken.
func (y Yours) Save(ctx context.Context, sec Secret, value redact.Secret) error {
	return y.locked(ctx, func(cs []Secret) error {
		if err := envFree(cs, sec); err != nil {
			return err
		}
		i := slices.IndexFunc(cs, func(c Secret) bool { return c.Name == sec.Name })
		if i >= 0 {
			sec.ID = cs[i].ID
		} else {
			sec.ID = newID()
		}
		if err := y.s.save(ctx, sec.Name, value); err != nil {
			return err
		}
		if i >= 0 {
			cs[i] = sec
		} else {
			cs = append(cs, sec)
		}
		if err := y.write(cs); err != nil {
			if i < 0 {
				_ = y.s.Delete(ctx, sec.Name)
			}
			return err
		}
		return nil
	})
}

// EnvFree checks, before devvm asks for sec's value, that no other secret
// of yours gives VMs sec's variable (secret_env_taken); Save checks it
// again under the lock.
func (y Yours) EnvFree(sec Secret) error {
	cs, err := y.List()
	if err != nil {
		return err
	}
	return envFree(cs, sec)
}

// envFree is EnvFree's check against cs.
func envFree(cs []Secret, sec Secret) error {
	for _, c := range cs {
		if c.Name != sec.Name && strings.EqualFold(c.Env, sec.Env) {
			return msg.SecretEnvTaken(sec.Env, c.Name)
		}
	}
	return nil
}

// Use runs bind under CustomSecretsLock with the secret of yours named
// name as it's saved then (no_secret when it isn't), so a VM is bound to
// the definition that's there as it binds.
func (y Yours) Use(ctx context.Context, name string, bind func(Secret) error) error {
	return y.locked(ctx, func(cs []Secret) error {
		sec, ok := Find(cs, name)
		if !ok {
			return msg.NoSecret(name)
		}
		return bind(sec)
	})
}

// Delete deletes a secret of yours, under CustomSecretsLock: first what
// before does (the VMs that use it stop), then its value (Store.Delete),
// a value without a definition too, then its definition. ErrNone when
// neither is there.
func (y Yours) Delete(ctx context.Context, name string, before func() error) error {
	if !nameOK(name) {
		return ErrNone
	}
	return y.locked(ctx, func(cs []Secret) error {
		if before != nil {
			if err := before(); err != nil {
				return err
			}
		}
		gone := y.s.Delete(ctx, name)
		if gone != nil && !errors.Is(gone, ErrNone) {
			return gone
		}
		n := len(cs)
		if cs = slices.DeleteFunc(cs, func(c Secret) bool { return c.Name == name }); len(cs) == n {
			return gone
		}
		return y.write(cs)
	})
}

// write writes Dirs.CustomSecrets whole, only yours to read.
func (y Yours) write(cs []Secret) error {
	l := customList{Version: customListVersion, Secrets: make([]customJSON, 0, len(cs))}
	for _, c := range cs {
		l.Secrets = append(l.Secrets, customJSON{
			Name: c.Name, ID: c.ID, Hosts: c.Hosts, Header: c.Header, Env: c.Env, Prefix: c.Prefix,
			Scheme: c.Scheme, Path: c.Path, PathPattern: c.Path != "", Methods: c.Methods, BasicUser: c.BasicUser,
		})
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	return state.WriteFile(y.s.Dirs.CustomSecrets(), append(b, '\n'))
}
