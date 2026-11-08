package vm

import (
	"context"
	"errors"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// SecretSave saves a token for a known secret, given as pasted or on
// stdin. Proxies running put the new value in at once.
func (e Net) SecretSave(ctx context.Context, name, given string) error {
	v, err := secrets.Token(name, given)
	if err != nil {
		return err
	}
	store := e.Proxy.Secrets
	if err := store.Save(ctx, name, v); err != nil {
		return err
	}
	if err := e.Proxy.RefreshSecrets(ctx); err != nil {
		return err
	}
	if store.Keychain {
		e.Info(msg.SecretSavedKeychain(name))
	} else {
		e.Info(msg.SecretSaved(store.File(name), name))
	}
	return nil
}

// SecretRm deletes a known secret's saved value. Proxies running stop
// putting it in at once.
func (e Net) SecretRm(ctx context.Context, name string) error {
	if err := e.Proxy.Secrets.Delete(ctx, name); errors.Is(err, secrets.ErrNone) {
		return msg.NoSecret(name)
	} else if err != nil {
		return err
	}
	if err := e.Proxy.RefreshSecrets(ctx); err != nil {
		return err
	}
	e.Info(msg.SecretRemoved(name))
	return nil
}

// SecretListed is a known secret as devvm secret lists it: never its
// value.
type SecretListed struct {
	Name string
	// Saved is whether a value is saved (or, for claude, in devvm's
	// environment).
	Saved  bool
	UsedBy []string // the VMs using it, by name
}

// Secrets returns the known secrets, each with whether a value is saved
// and the VMs using it.
func (e Net) Secrets(ctx context.Context) ([]SecretListed, error) {
	inv, err := ReadStates(e.Dirs)
	if err != nil {
		return nil, err
	}
	users := map[string][]string{}
	for _, st := range inv.States() {
		for _, s := range secrets.Names(st) {
			users[s] = append(users[s], st.Name())
		}
	}
	out := make([]SecretListed, 0, len(secrets.Known))
	for _, s := range secrets.Known {
		saved := true
		if _, err := e.Proxy.Secrets.Value(ctx, s.Name); errors.Is(err, secrets.ErrNone) {
			saved = false
		} else if err != nil {
			return nil, err
		}
		out = append(out, SecretListed{Name: s.Name, Saved: saved, UsedBy: users[s.Name]})
	}
	return out, nil
}

// SecretUse binds a known secret to the folder's VM (use), or unbinds it.
// Its proxy puts the token in or stops at once; the VM gets its
// placeholder from its next launch.
func (e Net) SecretUse(ctx context.Context, name string, use bool) error {
	sec, ok := secrets.Lookup(name)
	if !ok {
		return msg.NoSecret(name)
	}
	if sec.HostOnly {
		return msg.SecretHostOnly(name)
	}
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	if !use && secrets.FromTools(st, name) {
		return msg.SecretWithClaude(vm)
	}
	if use {
		if ok, err := t.widen("devvm secret use"); !ok {
			return err
		}
	}
	if err := st.Update(ctx, func(st *state.VM) error { return secrets.Bind(st, name, use) }); err != nil {
		return err
	}
	if err := e.Proxy.RefreshSecrets(ctx, vm); err != nil {
		return err
	}
	if !use {
		e.Info(msg.SecretUnused(name, vm))
		return nil
	}
	e.Info(msg.SecretUsed(name, vm))
	if l, err := secrets.ClaudeOf(e.Proxy.Secrets).Login(ctx); name == secrets.ClaudeToken && err == nil && l != "" {
		return nil
	}
	if _, err := e.Proxy.Secrets.Value(ctx, name); errors.Is(err, secrets.ErrNone) {
		if name == secrets.ClaudeToken {
			e.Warn(msg.NoClaudeToken(vm))
		} else {
			e.Warn(msg.NoGithubToken(vm))
		}
	} else if err != nil {
		return err
	}
	return nil
}
