package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
)

// stripeDef is a secret of yours, as devvm secret set defines it.
func stripeDef(t *testing.T, d Def) Secret {
	t.Helper()
	if d.Hosts == nil {
		d.Hosts = []string{"api.stripe.com"}
	}
	if d.Env == "" {
		d.Env = "STRIPE_API_KEY"
	}
	sec, err := Define("stripe", d)
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	return sec
}

// Your secrets in the store: the value where the known secrets' are, under
// a key of its own (custom.NAME), the definition with an ID of its own in
// its list, only yours to read; each variable once; saved again, the same
// ID; deleted, both go, and a value left without its definition too.
func TestCustomStore(t *testing.T) {
	for _, keychain := range []bool{true, false} {
		s, kc := testStore(t, keychain, nil)
		y := YoursOf(s)
		ctx := t.Context()
		if cs, err := y.List(); err != nil || len(cs) != 0 {
			t.Fatalf("no list: %v, %v", cs, err)
		}
		if err := y.Save(ctx, stripeDef(t, Def{Header: "x-api-key", Prefix: "sk_"}), redact.NewSecret("sk_test_e2efake")); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		other, _ := Define("other", Def{Hosts: []string{"api.example.com"}, Env: "stripe_api_key"})
		if err := y.Save(ctx, other, redact.NewSecret("x")); msg.NamesOf(err) != "secret_env_taken env=stripe_api_key secret=stripe" {
			t.Errorf("a variable taken: %v", msg.NamesOf(err))
		}
		got, ok, err := y.Lookup("stripe")
		if err != nil || !ok || got.Header != "X-Api-Key" || got.Env != "STRIPE_API_KEY" || got.Prefix != "sk_" || !got.Custom || !idOK(got.ID) {
			t.Errorf("Lookup = %+v, %v, %v", got, ok, err)
		}
		if err := y.Save(ctx, got, redact.NewSecret("sk_test_e2efake2")); err != nil {
			t.Fatal(err)
		}
		if again, _, _ := y.Lookup("stripe"); again.ID != got.ID {
			t.Errorf("saved again, its ID %q, was %q", again.ID, got.ID)
		}
		if v, err := s.Value(ctx, "stripe"); v.Reveal() != "sk_test_e2efake2" || err != nil {
			t.Errorf("Value = %q, %v", v.Reveal(), err)
		}
		list, err := os.ReadFile(s.Dirs.CustomSecrets())
		if err != nil || strings.Contains(string(list), "e2efake") || !strings.Contains(string(list), `"id": "`+got.ID+`"`) {
			t.Errorf("the list holds a value, or not the ID: %s, %v", list, err)
		}
		if fi, err := os.Stat(s.Dirs.CustomSecrets()); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the list's mode: %v, %v", fi, err)
		}
		// The value's key is never a known secret's.
		if keychain && (kc["devvm/custom.stripe"] != "sk_test_e2efake2" || kc["devvm/stripe"] != "") {
			t.Errorf("Keychain = %v", kc)
		}
		if !keychain && filepath.Base(s.File("stripe")) != "custom.stripe" {
			t.Errorf("File = %s", s.File("stripe"))
		}
		if filepath.Base(s.File("github")) != "github" {
			t.Errorf("a known secret's file = %s", s.File("github"))
		}
		ran := false
		if err := y.Delete(ctx, "stripe", func() error { ran = true; return nil }); err != nil || !ran {
			t.Fatal(msg.NamesOf(err), ran)
		}
		if _, ok, _ := y.Lookup("stripe"); ok || y.HasValue(ctx, "stripe") {
			t.Error("deleted, still defined or saved")
		}
		if err := y.Delete(ctx, "stripe", nil); !errors.Is(err, ErrNone) {
			t.Errorf("Delete again: %v", err)
		}
		// A value whose definition was never written: rm takes it away.
		if err := s.save(ctx, "stripe", redact.NewSecret("orphan")); err != nil {
			t.Fatal(err)
		}
		if !y.HasValue(ctx, "stripe") {
			t.Fatal("no orphan value")
		}
		if err := y.Delete(ctx, "stripe", nil); err != nil || y.HasValue(ctx, "stripe") {
			t.Errorf("the orphan value: %v", err)
		}
		if len(kc) != 0 {
			t.Errorf("deleted, the Keychain has %v", kc)
		}
	}
}

// A value past ValueMax is refused before the saved one is touched, so a
// long token never cuts the Keychain's line.
func TestValueMax(t *testing.T) {
	s, kc := testStore(t, true, nil)
	if err := s.Save(t.Context(), "github", redact.NewSecret("ghp_old")); err != nil {
		t.Fatal(err)
	}
	long := redact.NewSecret(strings.Repeat("a", ValueMax+1))
	if _, err := Token("github", long); msg.NamesOf(err) != "token_too_long most=3800" {
		t.Errorf("Token(long) = %v", msg.NamesOf(err))
	}
	if err := s.Save(t.Context(), "github", long); msg.NamesOf(err) != "token_too_long most=3800" || kc["devvm/github"] != "ghp_old" {
		t.Errorf("Save(long) = %v, the Keychain has %q", msg.NamesOf(err), kc["devvm/github"])
	}
	if _, err := Token("github", redact.NewSecret(strings.Repeat("a", ValueMax))); err != nil {
		t.Errorf("Token(ValueMax) = %v", err)
	}
}

// The proxy's lookup gives a value only to a binding that's the secret's
// as it's saved now: within its hosts, with its header, scheme, path, way
// into Basic credentials, ID and placeholder.
func TestGetChecksTheBinding(t *testing.T) {
	s, _ := testStore(t, true, nil)
	ctx := t.Context()
	y := YoursOf(s)
	if err := y.Save(ctx, stripeDef(t, Def{Hosts: []string{"api.stripe.com", "files.stripe.com"}, Prefix: "sk_", Path: "/v1/"}), redact.NewSecret("sk_test_e2efake")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "github", redact.NewSecret("ghp_x")); err != nil {
		t.Fatal(err)
	}
	sec, _, _ := y.Lookup("stripe")
	salt := strings.Repeat("ab", 16)
	good := proxy.Binding{
		Name: "stripe", Kind: proxy.KindSecret, Hosts: []string{"api.stripe.com"}, Header: "Authorization",
		Placeholder: sec.Placeholder(salt), Path: "/v1/", ID: sec.ID,
	}
	if v, err := s.Get(ctx, good); v.Reveal() != "sk_test_e2efake" || err != nil {
		t.Fatalf("Get = %q, %v", v.Reveal(), msg.NamesOf(err))
	}
	gh, _ := Lookup("github")
	ghb := proxy.Binding{Name: "github", Kind: proxy.KindSecret, Hosts: gh.Hosts, Header: "Authorization", Placeholder: gh.Placeholder(salt)}
	if v, err := s.Get(ctx, ghb); v.Reveal() != "ghp_x" || err != nil {
		t.Errorf("github's Get = %q, %v", v.Reveal(), msg.NamesOf(err))
	}
	for name, change := range map[string]func(*proxy.Binding){
		"another host":      func(b *proxy.Binding) { b.Hosts = append(b.Hosts, "evil.example.com") },
		"another header":    func(b *proxy.Binding) { b.Header = "X-Api-Key" },
		"a scheme":          func(b *proxy.Binding) { b.Scheme = "ApiKey" },
		"no path":           func(b *proxy.Binding) { b.Path = "" },
		"Basic's username":  func(b *proxy.Binding) { b.BasicUser = true },
		"another ID":        func(b *proxy.Binding) { b.ID = strings.Repeat("cd", 16) },
		"no ID":             func(b *proxy.Binding) { b.ID = "" },
		"another prefix":    func(b *proxy.Binding) { b.Placeholder = "x" + b.Placeholder },
		"no salt":           func(b *proxy.Binding) { b.Placeholder = sec.Placeholder("") },
		"a salt not in hex": func(b *proxy.Binding) { b.Placeholder = sec.Placeholder("zz") },
	} {
		b := good
		b.Hosts = append([]string{}, good.Hosts...)
		change(&b)
		if _, err := s.Get(ctx, b); msg.NamesOf(err) != "secret_binding_changed secret=stripe" {
			t.Errorf("%s: %v", name, msg.NamesOf(err))
		}
	}
	ghb.Hosts = append(ghb.Hosts, "evil.example.com")
	if _, err := s.Get(ctx, ghb); msg.NamesOf(err) != "secret_binding_changed secret=github" {
		t.Errorf("github's binding with another host: %v", msg.NamesOf(err))
	}
	// The company proxy's password: the proxy's own lookup, without hosts,
	// gets it; a binding with hosts never does.
	if err := s.Save(ctx, "proxy", redact.NewSecret("pa55")); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, proxy.Binding{Name: "proxy", Kind: proxy.KindSecret}); v.Reveal() != "pa55" || err != nil {
		t.Errorf("the proxy's own lookup = %q, %v", v.Reveal(), msg.NamesOf(err))
	}
	bad := proxy.Binding{Name: "proxy", Kind: proxy.KindSecret, Hosts: []string{"evil.example.com"}, Header: "Authorization", Placeholder: "x"}
	if _, err := s.Get(ctx, bad); msg.NamesOf(err) != "secret_binding_changed secret=proxy" {
		t.Errorf("a binding of the proxy's password: %v", msg.NamesOf(err))
	}
}

// A VM that uses a secret of yours gets its binding, in its header with
// its placeholder, scheme, path and ID and never its value, and its hosts
// on 443; a VM that doesn't, nothing of it; a VM bound to an earlier
// definition by the name, nothing either (Stale); hosts the secret gained
// after a VM was bound reach it only once it's bound again, and hosts it
// lost go at once.
func TestWriteFilesCustom(t *testing.T) {
	s, _ := testStore(t, true, nil)
	ctx := t.Context()
	y := YoursOf(s)
	if err := y.Save(ctx, stripeDef(t, Def{Header: "Authorization", Prefix: "sk_", Scheme: "ApiKey", Path: "/v1/", BasicUser: true}), redact.NewSecret("sk_test_e2efake")); err != nil {
		t.Fatal(err)
	}
	sec, _, _ := y.Lookup("stripe")
	salt := strings.Repeat("ab", 16)
	user, other := newVM(t, s.Dirs, vm1, "node"), newVM(t, s.Dirs, vm2, "node")
	update(t, user, func(st *state.VM) error { return BindYours(st, sec) })
	files := func(st *state.VM) (string, string) {
		t.Helper()
		if err := s.WriteFiles(ctx, st, salt); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(s.Dirs.VM(st.Name()).Secrets())
		pol, _ := os.ReadFile(s.Dirs.VM(st.Name()).SecretsPolicy())
		return string(data), string(pol)
	}
	binding := func(hosts string) string {
		return `{"version":1,"bindings":[{"name":"stripe","kind":"secret","hosts":[` + hosts + `],"path":"/v1/","header":"Authorization",` +
			`"placeholder":"sk_devvm-proxy-managed-secret-stripe-` + salt + `","scheme":"ApiKey","basic_user":true,"id":"` + sec.ID + `"}]}` + "\n"
	}
	if data, pol := files(user); data != binding(`"api.stripe.com"`) || pol != "api.stripe.com:443\n" {
		t.Errorf("the user's files = %s %q", data, pol)
	}
	if data, pol := files(other); data != `{"version":1,"bindings":[]}`+"\n" || pol != "" {
		t.Errorf("the other VM's files = %s %q", data, pol)
	}
	// A host gained: the VM keeps the one it had, until it's bound again.
	grown := sec
	grown.Hosts = []string{"api.stripe.com", "evil.example.com"}
	if err := y.Save(ctx, grown, redact.NewSecret("sk_test_e2efake")); err != nil {
		t.Fatal(err)
	}
	if data, pol := files(user); data != binding(`"api.stripe.com"`) || pol != "api.stripe.com:443\n" {
		t.Errorf("after a host was added: %s %q", data, pol)
	}
	grown, _, _ = y.Lookup("stripe")
	update(t, user, func(st *state.VM) error { return BindYours(st, grown) })
	if data, _ := files(user); data != binding(`"api.stripe.com","evil.example.com"`) {
		t.Errorf("bound again: %s", data)
	}
	// Deleted and saved again by its name: the VM is stale, and gets nothing.
	if err := y.Delete(ctx, "stripe", nil); err != nil {
		t.Fatal(err)
	}
	if err := y.Save(ctx, stripeDef(t, Def{Hosts: []string{"other.example.com"}}), redact.NewSecret("another")); err != nil {
		t.Fatal(err)
	}
	again, _, _ := y.Lookup("stripe")
	if !Stale(user, again) || again.ID == sec.ID {
		t.Errorf("Stale = %v, the IDs %s, %s", Stale(user, again), again.ID, sec.ID)
	}
	if data, pol := files(user); data != `{"version":1,"bindings":[]}`+"\n" || pol != "" {
		t.Errorf("a stale VM's files = %s %q", data, pol)
	}
	// unuse takes a stale name away.
	update(t, user, func(st *state.VM) error { return Bind(st, "stripe", false) })
	if b, _ := Of(user); len(b.Names) != 0 || len(b.Yours) != 0 {
		t.Errorf("unbound: %+v", b)
	}
}

// update changes st's state as devvm does, under its lock.
func update(t *testing.T, st *state.VM, do func(*state.VM) error) {
	t.Helper()
	if err := st.Update(t.Context(), do); err != nil {
		t.Fatal(err)
	}
}

// Every variable the VM's own scripts export, devvm's assets' layers and
// guest scripts, is one a secret of yours can't take (reservedEnv), so a
// new export there is refused too, or fails here.
func TestReservedEnvAssets(t *testing.T) {
	export := regexp.MustCompile(`export ([A-Za-z_][A-Za-z0-9_]*)=`)
	n := 0
	for _, dir := range []string{"layers", "guest", "shared-tools", "features"} {
		files, _ := filepath.Glob(filepath.Join("..", "..", "assets", dir, "*.sh"))
		for _, f := range files {
			b, err := os.ReadFile(f) //nolint:gosec // the repo's own assets
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range export.FindAllStringSubmatch(string(b), -1) {
				n++
				if CheckEnv(m[1]) == nil {
					t.Errorf("%s exports %s, which a secret of yours may take", f, m[1])
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no exports read from the assets")
	}
}
