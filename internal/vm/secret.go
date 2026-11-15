package vm

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// SecretToSave is the secret devvm secret set NAME saves, checked before
// devvm asks for its value: a known one as devvm has it (which takes no
// change: secret_fixed), or one of yours, with change over it as it's
// saved (secrets.Change.Apply), its variable no other secret's. A new one
// needs its hosts and variable (secret_needs_host), and a name close to a
// secret's is likely that one mistyped (secret_new_like).
func (e Net) SecretToSave(name string, change secrets.Change) (secrets.Secret, error) {
	if sec, ok := secrets.Lookup(name); ok {
		if !change.Empty() {
			return secrets.Secret{}, msg.SecretFixed(name)
		}
		return sec, nil
	}
	yours := secrets.YoursOf(e.Proxy.Secrets)
	cs, err := yours.List()
	if err != nil {
		return secrets.Secret{}, err
	}
	var saved *secrets.Secret
	if sec, ok := secrets.Find(cs, name); ok {
		saved = &sec
	} else if change.Empty() {
		names := make([]string, 0, len(secrets.Known)+len(cs))
		for _, s := range slices.Concat(secrets.Known, cs) {
			names = append(names, s.Name)
		}
		if like := msg.Closest(name, names); like != "" {
			return secrets.Secret{}, msg.SecretNewLike(name, like)
		}
		return secrets.Secret{}, msg.SecretNeedsHost(name)
	}
	sec, err := change.Apply(name, saved)
	if err != nil {
		return secrets.Secret{}, err
	}
	return sec, yours.EnvFree(sec)
}

// SecretSave saves a token for sec (SecretToSave's), given as pasted or on
// stdin: a known secret's value, or one of yours with its definition,
// naming the VMs that use it when its hosts change. Proxies running put
// the new value in at once.
func (e Net) SecretSave(ctx context.Context, sec secrets.Secret, given string) error {
	name := sec.Name
	v, err := secrets.Token(name, given)
	if err != nil {
		return err
	}
	store := e.Proxy.Secrets
	if sec.Custom {
		err = e.saveCustom(ctx, sec, v)
	} else {
		err = store.Save(ctx, name, v)
	}
	if err != nil {
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

// saveCustom saves a secret of yours and its value, and when its hosts
// changed, tells the VMs that use it: hosts it lost go from them at once,
// and hosts it gained reach a VM only once devvm secret use binds it again
// (secrets.UsedAs), so a host you add never gets the value from a VM you
// didn't give it to.
func (e Net) saveCustom(ctx context.Context, sec secrets.Secret, value string) error {
	yours := secrets.YoursOf(e.Proxy.Secrets)
	old, had, err := yours.Lookup(sec.Name)
	if err != nil {
		return err
	}
	if err := yours.Save(ctx, sec, value); err != nil {
		return err
	}
	if !had {
		return nil
	}
	gained := slices.ContainsFunc(sec.Hosts, func(h string) bool { return !slices.Contains(old.Hosts, h) })
	lost := slices.ContainsFunc(old.Hosts, func(h string) bool { return !slices.Contains(sec.Hosts, h) })
	if !gained && !lost {
		return nil
	}
	users, err := usersOf(e.Dirs, sec.Name)
	if err != nil || len(users) == 0 {
		return err
	}
	vms := vmname.Join(namesOf(users), ", ")
	if lost {
		e.Info(msg.SecretHostsChanged(len(users), strings.Join(sec.Hosts, ", "), sec.Name, vms))
	}
	if gained {
		e.Info(msg.SecretHostsGrown(len(users), sec.Name, vms))
	}
	return nil
}

// usersOf are the VMs in d bound to the secret.
func usersOf(d paths.Dirs, name string) ([]*state.VM, error) {
	inv, err := ReadStates(d)
	if err != nil {
		return nil, err
	}
	var users []*state.VM
	for _, st := range inv.States() {
		if slices.Contains(secrets.Names(st), name) {
			users = append(users, st)
		}
	}
	return users, nil
}

// SecretRm deletes a secret's saved value, after asking unless force: a
// known one's, or one of yours with its definition (rmYours). Proxies
// running stop putting it in at once.
func (e Net) SecretRm(ctx context.Context, name string, force bool) error {
	store := e.Proxy.Secrets
	sec, ok, err := secrets.YoursOf(store).Lookup(name)
	switch {
	case err != nil:
		return err
	case !ok || sec.Custom:
		return rmYours(ctx, e, name, force)
	}
	if _, err := store.Value(ctx, name); errors.Is(err, secrets.ErrNone) {
		return msg.NoSecret(name)
	} else if err != nil {
		return err
	}
	if ok, err := confirmRm(e, name, nil, force); !ok {
		return err
	}
	if err := store.Delete(ctx, name); errors.Is(err, secrets.ErrNone) {
		return msg.NoSecret(name)
	} else if err != nil {
		return err
	}
	return removed(ctx, e, name)
}

// rmYours is SecretRm for a secret of yours, or a value of one left
// without its definition: the VMs using it stop using it, then its value
// and definition go, all under the lock of your secrets
// (secrets.Yours.Delete), so a failure part way leaves it saved, never a
// VM bound to a name that a secret saved later may take, and no devvm
// secret use can bind a VM meanwhile.
func rmYours(ctx context.Context, e Net, name string, force bool) error {
	yours := secrets.YoursOf(e.Proxy.Secrets)
	if _, ok, _ := yours.Lookup(name); !ok && !yours.HasValue(ctx, name) {
		return msg.NoSecret(name)
	}
	users, err := usersOf(e.Dirs, name)
	if err != nil {
		return err
	}
	if ok, err := confirmRm(e, name, users, force); !ok {
		return err
	}
	err = yours.Delete(ctx, name, func() error {
		users, err := usersOf(e.Dirs, name)
		if err != nil {
			return err
		}
		for _, st := range users {
			if err := st.Update(ctx, func(st *state.VM) error { return secrets.Bind(st, name, false) }); err != nil {
				return err
			}
			e.Info(msg.SecretUnused(name, st.Name()))
		}
		return nil
	})
	if errors.Is(err, secrets.ErrNone) {
		return msg.NoSecret(name)
	} else if err != nil {
		return err
	}
	return removed(ctx, e, name)
}

// confirmRm asks whether to delete the secret, naming the VMs that use it
// (users), unless devvm secret rm has --force; no deletes nothing.
func confirmRm(e Net, name string, users []*state.VM, force bool) (bool, error) {
	if force {
		return true, nil
	}
	q := msg.SecretRmConfirm(name).AnsweredBy("--force")
	if len(users) > 0 {
		q = q.About(msg.SecretRmUsers(len(users), name, vmname.Join(namesOf(users), ", ")))
	}
	ok, err := e.Ask(q)
	if err == nil && !ok {
		e.Info(msg.NothingDeleted())
	}
	return ok, err
}

// removed tells the proxies running that the secret went, and says so.
func removed(ctx context.Context, e Net, name string) error {
	if err := e.Proxy.RefreshSecrets(ctx); err != nil {
		return err
	}
	e.Info(msg.SecretRemoved(name))
	return nil
}

// namesOf are the VMs' names, in order.
func namesOf(sts []*state.VM) []vmname.VM {
	out := make([]vmname.VM, len(sts))
	for i, st := range sts {
		out[i] = st.Name()
	}
	return out
}

// SecretListed is a secret as devvm secret lists it: never its value.
type SecretListed struct {
	secrets.Secret
	// Saved is whether a value is saved (or, for claude, in devvm's
	// environment).
	Saved  bool
	UsedBy []vmname.VM // the VMs using it, by name
}

// Secrets returns every secret, the known ones then yours, each with
// whether a value is saved and the VMs using it.
func (e Net) Secrets(ctx context.Context) ([]SecretListed, error) {
	inv, err := ReadStates(e.Dirs)
	if err != nil {
		return nil, err
	}
	users := map[string][]vmname.VM{}
	for _, st := range inv.States() {
		for _, s := range secrets.Names(st) {
			users[s] = append(users[s], st.Name())
		}
	}
	all, err := secrets.YoursOf(e.Proxy.Secrets).All()
	if err != nil {
		return nil, err
	}
	out := make([]SecretListed, 0, len(all))
	for _, s := range all {
		saved := true
		if _, err := e.Proxy.Secrets.Value(ctx, s.Name); errors.Is(err, secrets.ErrNone) {
			saved = false
		} else if err != nil {
			return nil, err
		}
		out = append(out, SecretListed{Secret: s, Saved: saved, UsedBy: users[s.Name]})
	}
	return out, nil
}

// SecretUse binds a secret to the folder's VM (use), or unbinds it: a
// known one, or one of yours (useYours). unuse also takes a name the VM is
// bound to whose secret is gone. Its proxy puts the token in or stops at
// once; the VM gets its placeholder from its next launch.
func (e Net) SecretUse(ctx context.Context, name string, use bool) error {
	sec, ok, err := secrets.YoursOf(e.Proxy.Secrets).Lookup(name)
	switch {
	case err != nil:
		return err
	case !ok && use:
		return msg.NoSecret(name)
	case sec.HostOnly:
		return msg.SecretHostOnly(name)
	}
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	switch {
	case !use && !ok && !slices.Contains(secrets.Names(st), name):
		return msg.NoSecret(name)
	case !use && secrets.FromTools(st, name):
		return msg.SecretWithClaude(vm)
	case use:
		if ok, err := t.widen("devvm secret use"); !ok {
			return err
		}
	}
	if use && sec.Custom {
		if sec, err = useYours(ctx, e, st, name); err != nil {
			return err
		}
	} else if err := st.Update(ctx, func(st *state.VM) error { return secrets.Bind(st, name, use) }); err != nil {
		return err
	}
	if err := e.Proxy.RefreshSecrets(ctx, vm); err != nil {
		return err
	}
	switch {
	case !use:
		e.Info(msg.SecretUnused(name, vm))
		return nil
	case sec.Custom:
		e.Info(msg.CustomSecretUsed(strings.Join(sec.Hosts, ", "), name, vm))
	default:
		e.Info(msg.SecretUsed(name, vm))
	}
	return e.warnUnsaved(ctx, name, vm)
}

// useYours binds the VM st to the secret of yours named name as it's saved
// now, under the lock of your secrets (secrets.Yours.Use), so no devvm
// secret rm or set changes it between the two: its definition by its ID,
// and its hosts now, all its proxy ever puts it in for.
func useYours(ctx context.Context, e Net, st *state.VM, name string) (secrets.Secret, error) {
	var sec secrets.Secret
	err := secrets.YoursOf(e.Proxy.Secrets).Use(ctx, name, func(cur secrets.Secret) error {
		sec = cur
		return st.Update(ctx, func(st *state.VM) error { return secrets.BindYours(st, cur) })
	})
	return sec, err
}

// warnUnsaved warns when the VM uses a secret that has no value saved:
// claude (unless devvm keeps a Claude Code login), github or one of yours.
func (e Net) warnUnsaved(ctx context.Context, name string, vm vmname.VM) error {
	if l, err := secrets.ClaudeOf(e.Proxy.Secrets).Login(ctx); name == secrets.ClaudeToken && err == nil && l != "" {
		return nil
	}
	if _, err := e.Proxy.Secrets.Value(ctx, name); errors.Is(err, secrets.ErrNone) {
		switch name {
		case secrets.ClaudeToken:
			e.Warn(msg.NoClaudeToken(vm))
		case "github":
			e.Warn(msg.NoGithubToken(vm))
		default:
			e.Warn(msg.SecretNotSaved(name, vm))
		}
	} else if err != nil {
		return err
	}
	return nil
}
