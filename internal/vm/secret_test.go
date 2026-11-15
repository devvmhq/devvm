package vm

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// toSave is the secret devvm secret set NAME saves, without flags.
func toSave(t *testing.T, n Net, name string) secrets.Secret {
	t.Helper()
	sec, err := n.SecretToSave(name, secrets.Change{})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	return sec
}

// secretList is devvm secret's list, a line each: name, saved, users.
func secretList(t *testing.T, n Net) string {
	t.Helper()
	ss, err := n.Secrets(t.Context())
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	var b strings.Builder
	for _, s := range ss {
		saved := "no"
		if s.Saved {
			saved = "yes"
		}
		b.WriteString(s.Name + " " + saved + " " + vmname.Join(s.UsedBy, ",") + "\n")
	}
	return b.String()
}

// devvm secret: saved and removed tokens, listed with the VMs using them;
// use and unuse on the folder's VM, claude staying with an image that has
// it.
func TestSecrets(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	n := Net{e}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	ctx := t.Context()
	if got, want := secretList(t, n), "claude no \ngithub no \nproxy no \n"; got != want {
		t.Errorf("Secrets = %q, want %q", got, want)
	}
	// The company proxy's password: saved as given, spaces inside kept, and
	// never a VM's.
	if err := n.SecretSave(ctx, toSave(t, n, "proxy"), redact.NewSecret(" pa55 word\n")); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if v, err := e.Proxy.Secrets.Value(ctx, "proxy"); err != nil || v.Reveal() != "pa55 word" {
		t.Errorf("proxy = %q, %v", v.Reveal(), err)
	}
	for _, use := range []bool{true, false} {
		if err := n.SecretUse(ctx, "proxy", use); msg.NamesOf(err) != "secret_host_only secret=proxy" {
			t.Errorf("SecretUse(proxy, %v) = %v", use, msg.NamesOf(err))
		}
	}
	if err := n.SecretRm(ctx, "proxy", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.SecretSave(ctx, toSave(t, n, "github"), redact.NewSecret(" ghp_testfake\n")); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.SecretUse(ctx, "github", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := secretList(t, n); !strings.Contains(got, "github yes "+vm.String()+"\n") {
		t.Errorf("Secrets = %q", got)
	}
	if err := n.SecretSave(ctx, toSave(t, n, "claude"), redact.NewSecret("not-a-setup-token")); msg.NamesOf(err) != "not_setup_token" {
		t.Errorf("SecretSave(claude) = %v", msg.NamesOf(err))
	}
	if err := n.SecretRm(ctx, "github", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.SecretRm(ctx, "github", true); msg.NamesOf(err) != "no_secret secret=github" {
		t.Errorf("SecretRm again = %v", msg.NamesOf(err))
	}
	if err := n.SecretUse(ctx, "github", false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, keySecrets) != "" {
		t.Errorf("secrets=%q", get(st, keySecrets))
	}
	// A VM with Claude Code always uses the Claude Code token.
	if err := set(t, st, keyToolNames, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := n.SecretUse(ctx, "claude", false); msg.NamesOf(err) != "secret_with_claude vm="+vm.String() {
		t.Errorf("SecretUse(claude, false) = %v", msg.NamesOf(err))
	}
	if got := secretList(t, n); !strings.Contains(got, "claude no "+vm.String()+"\n") {
		t.Errorf("Secrets = %q", got)
	}
}

// A secret of yours: saved with its definition, listed after devvm's own,
// used by the VMs you use it in alone, its hosts named as it's used; set
// again without flags, it keeps its definition; deleted, after a question
// that names its VMs, it goes from them too.
func TestCustomSecrets(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	n := Net{e}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	ctx := t.Context()
	for _, tc := range []struct {
		name   string
		change secrets.Change
		want   string
	}{
		{"stripe", secrets.Change{}, "secret_needs_host secret=stripe"},
		{"stripe", secrets.Change{Hosts: []string{"api.stripe.com"}}, "secret_needs_env secret=stripe"},
		{"githb", secrets.Change{}, "secret_new_like given=githb like=github"},
		{"github", secrets.Change{Env: "X_KEY"}, "secret_fixed secret=github"},
	} {
		if _, err := n.SecretToSave(tc.name, tc.change); msg.NamesOf(err) != tc.want {
			t.Errorf("SecretToSave(%s, %+v) = %v, want %s", tc.name, tc.change, msg.NamesOf(err), tc.want)
		}
	}
	sec, err := n.SecretToSave("stripe", secrets.Change{Hosts: []string{"api.stripe.com"}, Env: "STRIPE_API_KEY", Prefix: "sk_"})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.SecretSave(ctx, sec, redact.NewSecret("sk_test_e2efake\n")); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := secretList(t, n); !strings.HasSuffix(got, "proxy no \nstripe yes \n") {
		t.Errorf("Secrets = %q", got)
	}
	var infos []string
	fake(e).OnInfo = func(m msg.M) { infos = append(infos, m.Names()) }
	if err := n.SecretUse(ctx, "stripe", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "custom_secret_used hosts=api.stripe.com secret=stripe vm=" + vm.String(); !strings.Contains(strings.Join(infos, "\n"), want) {
		t.Errorf("use said %q, want %q", infos, want)
	}
	// Saved again without flags: the same definition, a new value.
	again := toSave(t, n, "stripe")
	if again.Env != "STRIPE_API_KEY" || again.Prefix != "sk_" || again.Hosts[0] != "api.stripe.com" {
		t.Errorf("saved again as %+v", again)
	}
	if err := n.SecretSave(ctx, again, redact.NewSecret("sk_test_e2efake2")); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if v, err := e.Proxy.Secrets.Value(ctx, "stripe"); v.Reveal() != "sk_test_e2efake2" || err != nil {
		t.Errorf("stripe = %q, %v", v.Reveal(), err)
	}
	if got := secretList(t, n); !strings.Contains(got, "stripe yes "+vm.String()+"\n") {
		t.Errorf("Secrets = %q", got)
	}
	// A new host alone keeps the rest, and names the VMs it now reaches
	// from; another variable that one of yours has is refused before it asks.
	moved, err := n.SecretToSave("stripe", secrets.Change{Hosts: []string{"files.stripe.com"}})
	if err != nil || moved.Env != "STRIPE_API_KEY" || moved.Prefix != "sk_" || moved.Header != "Authorization" {
		t.Fatalf("a new host: %+v, %v", moved, msg.NamesOf(err))
	}
	infos = nil
	if err := n.SecretSave(ctx, moved, redact.NewSecret("sk_test_e2efake2")); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for _, want := range []string{
		"secret_hosts_changed count=1 hosts=files.stripe.com secret=stripe vms=" + vm.String(), // api.stripe.com went
		"secret_hosts_grown count=1 secret=stripe vms=" + vm.String(),                          // files.stripe.com waits for use
	} {
		if !strings.Contains(strings.Join(infos, "\n"), want) {
			t.Errorf("the new host said %q, want %q", infos, want)
		}
	}
	// The VM keeps the hosts it was bound to, none of which are left:
	// devvm secret use gives it the new one.
	boundHosts := func() []string {
		st, _ := state.ReadVM(e.Dirs, vm)
		b, _ := secrets.Of(st)
		if len(b.Yours) != 1 {
			return nil
		}
		return b.Yours[0].Hosts
	}
	if got := boundHosts(); !slices.Equal(got, []string{"api.stripe.com"}) {
		t.Errorf("bound hosts = %q", got)
	}
	if err := n.SecretUse(ctx, "stripe", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := boundHosts(); !slices.Equal(got, []string{"files.stripe.com"}) {
		t.Errorf("bound again, hosts = %q", got)
	}
	if _, err := n.SecretToSave("other", secrets.Change{Hosts: []string{"api.example.com"}, Env: "STRIPE_API_KEY"}); msg.NamesOf(err) != "secret_env_taken env=STRIPE_API_KEY secret=stripe" {
		t.Errorf("a variable taken: %v", msg.NamesOf(err))
	}
	// The question names the VMs that use it; no deletes nothing.
	f := fake(e)
	f.Terminal = true
	var about []string
	f.OnNote = func(m msg.M) { about = append(about, m.Names()) }
	f.OnAsk = func(msg.M) bool { return false }
	if err := n.SecretRm(ctx, "stripe", false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "secret_rm_users count=1 secret=stripe vms=" + vm.String(); !strings.Contains(strings.Join(about, "\n"), want) {
		t.Errorf("the question's context %q, want %q", about, want)
	}
	if _, err := os.Stat(e.Dirs.CustomSecrets()); err != nil {
		t.Fatal("no deletes the definition")
	}
	f.OnAsk = func(msg.M) bool { return true }
	if err := n.SecretRm(ctx, "stripe", false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, keySecrets) != "" {
		t.Errorf("secrets=%q after the secret went", get(st, keySecrets))
	}
	if got, want := secretList(t, n), "claude no \ngithub no \nproxy no \n"; got != want {
		t.Errorf("Secrets = %q, want %q", got, want)
	}
	if err := n.SecretUse(ctx, "stripe", true); msg.NamesOf(err) != "no_secret secret=stripe" {
		t.Errorf("SecretUse of a deleted secret = %v", msg.NamesOf(err))
	}
	if err := n.SecretRm(ctx, "stripe", true); msg.NamesOf(err) != "no_secret secret=stripe" {
		t.Errorf("SecretRm again = %v", msg.NamesOf(err))
	}
	// A VM left bound to a name whose secret went (its state written
	// before the secret did) is stale once another is saved by the name,
	// and devvm secret unuse takes the name away whether a secret has it
	// or not.
	if err := st.Update(ctx, func(st *state.VM) error {
		return secrets.BindYours(st, secrets.Secret{Name: "stripe", ID: strings.Repeat("cd", 16), Hosts: []string{"api.stripe.com"}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.SecretUse(ctx, "stripe", false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, keySecrets) != "" {
		t.Errorf("secrets=%q after unuse", get(st, keySecrets))
	}
	if err := n.SecretUse(ctx, "stripe", false); msg.NamesOf(err) != "no_secret secret=stripe" {
		t.Errorf("unuse of a name the VM isn't bound to = %v", msg.NamesOf(err))
	}
}
