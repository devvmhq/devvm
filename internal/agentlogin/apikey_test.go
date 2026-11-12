package agentlogin

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
)

// An API key saved: Codex's login becomes an API-key login, its ChatGPT
// login gone; opencode's gets the provider's entry beside the others. A
// provider devvm doesn't put keys in for, or a key that isn't one line,
// is refused, and nothing is written.
func TestSetKey(t *testing.T) {
	if got := KeyProviders(Opencode); !slices.Equal(got, []string{"anthropic", "deepseek", "google", "groq", "mistral", "openai", "openrouter", "xai"}) {
		t.Errorf("KeyProviders(opencode) = %v", got)
	}
	if got := KeyProviders(Codex); got != nil {
		t.Errorf("KeyProviders(codex) = %v", got)
	}

	c := storeFile(t, Codex, ".codex/auth.json")
	must(t, c.lockStoreWrite(map[string]any{"tokens": map[string]any{"access_token": "at"}}))
	must(t, c.SetKey("", "sk-test"))
	m := readStore(t, c)
	if str(m, "OPENAI_API_KEY") != "sk-test" || str(m, "auth_mode") != "apikey" || m["tokens"] != nil {
		t.Errorf("Codex's login: %v", m)
	}
	if bs, _ := c.Bindings(); len(bs) != 1 || bs[0].Name != "codex-key" {
		t.Errorf("Codex's bindings: %v", bs)
	}

	o := storeFile(t, Opencode, "auth.json")
	must(t, o.lockStoreWrite(map[string]any{"openai": map[string]any{"type": "oauth", "access": "a", "refresh": "r", "expires": 1}}))
	must(t, o.SetKey("anthropic", "sk-ant"))
	m = readStore(t, o)
	if e := obj(m, "anthropic"); str(e, "type") != "api" || str(e, "key") != "sk-ant" || str(obj(m, "openai"), "refresh") != "r" {
		t.Errorf("opencode's login: %v", m)
	}

	for _, bad := range []struct {
		f             File
		provider, key string
	}{
		{c, "anthropic", "k"}, {o, "", "k"}, {o, "lmstudio", "k"}, {c, "", " "}, {c, "", "a\nb"},
	} {
		if err := bad.f.SetKey(bad.provider, bad.key); err == nil {
			t.Errorf("SetKey(%s, %q, %q) took it", bad.f.Kind, bad.provider, bad.key)
		}
	}
	if str(readStore(t, c), "OPENAI_API_KEY") != "sk-test" {
		t.Error("a refused key changed Codex's login")
	}
}

// opencode's import is opencode 2's auth export entries for the VM's copy
// of the store's login: devvm's IDs, sorted, each API key devvm puts in and
// OpenAI's Codex login with the placeholders the copy holds and the proxy
// binds, never a key or token; a key devvm doesn't put in is left out, and
// an empty login is an empty list.
func TestOpencodeImport(t *testing.T) {
	o := storeFile(t, Opencode, "auth.json")
	access := jwtWith(time.Now().Add(time.Hour), map[string]any{"email": "a@example.com"})
	must(t, o.lockStoreWrite(map[string]any{
		"lmstudio": map[string]any{"type": "api", "key": "lms-kept"},
		"openai":   map[string]any{"type": "oauth", "access": access, "refresh": "rt-secret", "expires": 1, "accountId": "acct-1"},
	}))
	must(t, o.SetKey("anthropic", "sk-ant-secret"))
	o.Salt = "s4lt"
	c, err := o.VMCopy()
	must(t, err)
	bs, err := o.Bindings()
	must(t, err)
	ph := map[string]string{}
	for _, b := range bs {
		ph[b.Name] = b.Placeholder
	}
	data, err := OpencodeImport(c.Data)
	must(t, err)
	type value struct {
		Type, MethodID, Key, Access, Refresh string
		Metadata                             struct{ AccountID string }
	}
	var got []struct {
		ID, IntegrationID, Label string
		Active                   bool
		Value                    value
	}
	must(t, json.Unmarshal(data, &got))
	if len(got) != 2 {
		t.Fatalf("OpencodeImport = %s", data)
	}
	a, oa := got[0], got[1]
	if a.ID != "cred_devvm_anthropic" || a.IntegrationID != "anthropic" || a.Label != "devvm" || !a.Active ||
		a.Value != (value{Type: "key", Key: ph["opencode-anthropic"]}) {
		t.Errorf("anthropic: %+v", a)
	}
	want := value{Type: "oauth", MethodID: "chatgpt-browser", Access: ph["opencode-openai"], Refresh: ph["opencode-openai"]}
	want.Metadata.AccountID = "acct-1"
	if oa.ID != "cred_devvm_openai" || oa.IntegrationID != "openai" || oa.Value != want {
		t.Errorf("openai: %+v", oa)
	}
	for _, secret := range []string{"sk-ant-secret", "rt-secret", access, "lms-kept"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("OpencodeImport holds %q: %s", secret, data)
		}
	}
	// opencode 2 needs a whole number for expires.
	if data, err := OpencodeImport([]byte(`{"openai":{"type":"oauth","access":"a","refresh":"r"}}`)); err != nil || !strings.Contains(string(data), `"expires":0,`) {
		t.Errorf("OpencodeImport without expires = %s, %v", data, err)
	}
	if data, err := OpencodeImport([]byte(`{}`)); err != nil || string(data) != "[]" {
		t.Errorf("OpencodeImport({}) = %s, %v", data, err)
	}
	if _, err := OpencodeImport([]byte(`[`)); err == nil {
		t.Error("OpencodeImport took a broken copy")
	}
}

// Each provider's API key, saved as devvm agent login --api-key --provider
// saves it for opencode and for pi, is bound for that provider's hosts and
// headers alone, a binding each, below its path when it has one, as checked
// against both agents' source, under the agent's placeholder, which starts
// as the agent checks the provider's keys start and which the VM's copy
// holds in the key's place; Get gives the key back.
func TestProviderKeys(t *testing.T) {
	want := map[string][]string{
		"anthropic": {"api.anthropic.com", "", "x-api-key"}, "deepseek": {"api.deepseek.com", "", "Authorization"},
		"google": {"generativelanguage.googleapis.com", "", "x-goog-api-key"}, "groq": {"api.groq.com", "", "Authorization"},
		"mistral": {"api.mistral.ai", "", "Authorization"}, "openai": {"api.openai.com", "", "Authorization"},
		"openrouter": {"openrouter.ai", "/api/", "Authorization", "x-api-key"}, "xai": {"api.x.ai", "", "Authorization"},
	}
	// pi takes an OpenAI key that doesn't start with sk- for a ChatGPT sign-in's token.
	prefix := map[string]string{"openai": "sk-"}
	keyed := agents.Select(func(d agents.Def) bool { return d.Keys == agents.ProviderKeys })
	if !slices.Contains(keyed, Pi) {
		t.Fatalf("the agents with a key for each provider are %v, without pi", keyed)
	}
	for _, kind := range keyed {
		if got := KeyProviders(kind); len(got) != len(want) {
			t.Errorf("KeyProviders(%s) = %v", kind, got)
		}
		for _, id := range KeyProviders(kind) {
			f := storeFile(t, kind, "auth.json")
			must(t, f.lockStoreWrite(map[string]any{}))
			key := id + "-e2efake-key"
			must(t, f.SetKey(id, key))
			f.Salt = "s1"
			ph := prefix[id] + "devvm-proxy-managed-" + kind + "-" + id + "-s1"
			bs := bindings(t, f)
			var headers []string
			for _, b := range bs {
				headers = append(headers, b.Header)
				if b.Name != kind+"-"+id || !slices.Equal(b.Hosts, []string{want[id][0]}) || b.Path != want[id][1] || b.Placeholder != ph {
					t.Errorf("%s's %s binding: %+v", kind, id, b)
				}
			}
			if !slices.Equal(headers, want[id][2:]) {
				t.Errorf("%s's %s bindings' headers: %q, want %q", kind, id, headers, want[id][2:])
			}
			c, err := f.VMCopy()
			must(t, err)
			var vm map[string]any
			must(t, json.Unmarshal(c.Data, &vm))
			if str(obj(vm, id), "key") != ph || strings.Contains(string(c.Data), "e2efake") || len(c.Withheld) > 0 {
				t.Errorf("%s's %s VM copy: %s", kind, id, c.Data)
			}
			if v, err := f.Get(t.Context(), kind+"-"+id); err != nil || v != key {
				t.Errorf("Get(%s-%s) = %q, %v", kind, id, v, err)
			}
		}
	}
}

// lockStoreWrite writes m as f's login, its folder made.
func (f File) lockStoreWrite(m map[string]any) error {
	unlock, err := f.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	return writeJSON(f.Path, m)
}

// The ChatGPT login a store's file holds, with its account's email: from
// Codex's id_token, or opencode's access token's profile; none for an
// API key or no file.
func TestChatGPT(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	c := storeFile(t, Codex, ".codex/auth.json")
	if _, ok, err := c.ChatGPT(); ok || err != nil {
		t.Errorf("no file: %v, %v", ok, err)
	}
	must(t, c.lockStoreWrite(map[string]any{"tokens": map[string]any{
		"id_token": jwtWith(exp, map[string]any{"email": "a@b.test"}), "access_token": jwtWith(exp, nil),
	}}))
	if e, ok, err := c.ChatGPT(); e != "a@b.test" || !ok || err != nil {
		t.Errorf("Codex: %q, %v, %v", e, ok, err)
	}
	must(t, c.SetKey("", "sk-test"))
	if _, ok, _ := c.ChatGPT(); ok {
		t.Error("an API key read as a ChatGPT login")
	}

	o := storeFile(t, Opencode, "auth.json")
	must(t, o.lockStoreWrite(map[string]any{"openai": map[string]any{
		"type":   "oauth",
		"access": jwtWith(exp, map[string]any{"https://api.openai.com/profile": map[string]any{"email": "o@b.test"}}),
	}}))
	if e, ok, err := o.ChatGPT(); e != "o@b.test" || !ok || err != nil {
		t.Errorf("opencode: %q, %v, %v", e, ok, err)
	}
	must(t, o.lockStoreWrite(map[string]any{"openai": map[string]any{"type": "oauth", "access": "opaque"}}))
	if e, ok, _ := o.ChatGPT(); e != "" || !ok {
		t.Errorf("opencode without claims: %q, %v", e, ok)
	}
}
