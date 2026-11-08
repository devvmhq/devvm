package agentlogin

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// piHostLogins is a pi auth.json on the host, its keys and tokens fakes (e2efake): the review's
// shapes, a key with env, extra and nested fields beside it, a templated key, keys in env alone, a
// command key, other providers' keys, and OAuth logins with nested and list fields.
const piHostLogins = `{
	"anthropic": {"type": "api_key", "key": "sk-ant-e2efake", "env": {"K": "sk-ant-e2efake-env"},
		"token": "tk-e2efake", "nested": {"refresh": "rt-e2efake-nested"}},
	"openrouter": {"type": "api_key", "key": "sk-or-e2efake"},
	"openai": {"type": "api_key", "key": "$K", "env": {"K": "sk-e2efake-template", "OPENAI_API_KEY": "sk-e2efake-env"}},
	"azure": {"type": "api_key", "key": "x", "env": {"AZURE_OPENAI_API_KEY": "az-e2efake"}},
	"amazon-bedrock": {"type": "api_key", "env": {"AWS_SECRET_ACCESS_KEY": "aws-e2efake"}},
	"google": {"type": "api_key", "key": "!pass show e2efake"},
	"groq": {"type": "api_key", "key": "gsk-e2efake"},
	"ollama": {"type": "api_key", "key": "ollama-local", "env": {"OLLAMA_HOST": "kept"}},
	"groq-oauth": {"type": "oauth", "access": "at-e2efake-passed", "refresh": "rt-e2efake-passed"},
	"openai-codex": {"type": "oauth", "access": "at-e2efake", "refresh": "rt-e2efake", "expires": 1, "accountId": "acct-1",
		"idToken": "id-e2efake", "scopes": ["openid"], "extra": {"refresh": "rt-e2efake-x"},
		"email": [{"t": "e2efake-in-a-list"}]}
}`

// pi's auth.json in the VM, whole. A bound key (Anthropic's, OpenRouter's) becomes the VM's
// placeholder, bound for its host and header, and its entry holds nothing else: no env, which pi
// lets win over the environment, and no other field, nested or not. A templated key ($K, filled
// from env) and a command key aren't bound; neither is another provider's key, nor an env holding
// one (Azure's, Bedrock's): each is withheld, as an OAuth login is, keeping only fields that name
// an account. The provider File.Pass lists (a local model server) goes as it is. Get reads only
// the bound keys from the host's file.
func TestPiLogins(t *testing.T) {
	var host map[string]any
	must(t, json.Unmarshal([]byte(piHostLogins), &host))
	f := File{Kind: Pi, Salt: "s1", Pass: []string{"ollama", "groq-oauth"}, Path: writeFile(t, host)}
	var got []string
	for _, b := range bindings(t, f) {
		got = append(got, b.Name+" "+strings.Join(b.Hosts, ",")+" "+b.Header+" "+b.Placeholder)
	}
	want := []string{
		"pi-anthropic api.anthropic.com x-api-key devvm-proxy-managed-pi-anthropic-s1",
		"pi-openrouter openrouter.ai Authorization devvm-proxy-managed-pi-openrouter-s1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("bindings:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	c, err := f.VMCopy()
	must(t, err)
	var vm, wantVM any
	must(t, json.Unmarshal(c.Data, &vm))
	must(t, json.Unmarshal([]byte(`{
		"anthropic": {"type": "api_key", "key": "devvm-proxy-managed-pi-anthropic-s1"},
		"openrouter": {"type": "api_key", "key": "devvm-proxy-managed-pi-openrouter-s1"},
		"openai": {"type": "api_key"}, "azure": {"type": "api_key"}, "amazon-bedrock": {"type": "api_key"},
		"google": {"type": "api_key"}, "groq": {"type": "api_key"},
		"ollama": {"type": "api_key", "key": "ollama-local", "env": {"OLLAMA_HOST": "kept"}},
		"groq-oauth": {"type": "oauth"},
		"openai-codex": {"type": "oauth", "expires": 1, "accountId": "acct-1", "scopes": ["openid"]}
	}`), &wantVM))
	if !reflect.DeepEqual(vm, wantVM) {
		t.Errorf("the VM's copy:\n%s", c.Data)
	}
	if strings.Contains(string(c.Data), "e2efake") {
		t.Errorf("the VM's copy holds a key or token:\n%s", c.Data)
	}
	withheld := []string{"amazon-bedrock", "azure", "google", "groq", "groq-oauth", "openai", "openai-codex"}
	if !slices.Equal(c.Kept, []string{"ollama"}) || !slices.Equal(c.Withheld, withheld) {
		t.Errorf("kept %v, withheld %v", c.Kept, c.Withheld)
	}
	if v, err := f.Get(t.Context(), "pi-anthropic"); err != nil || v != "sk-ant-e2efake" {
		t.Errorf("Get(pi-anthropic) = %q, %v", v, err)
	}
	for _, name := range []string{"pi-openai", "pi-groq", "pi-ollama", "pi-openai-codex", "pi-nothing"} {
		if v, err := f.Get(t.Context(), name); err == nil {
			t.Errorf("Get(%s) = %q, no error", name, v)
		}
	}
}

// A pi key is bound only when it's a value: not a command (!), and without a $NAME or ${NAME}
// that pi fills in, $$ being a literal $.
func TestPiKeyTemplates(t *testing.T) {
	for key, bound := range map[string]bool{
		"sk-plain": true, "sk-$": true, "sk-$1": true, "$$K": true, "a$$b": true,
		"$K": false, "${K}": false, "a$K": false, "a${K}b": false, "$$$K": false, "!cmd": false, "": false,
	} {
		m := map[string]any{"anthropic": map[string]any{"type": "api_key", "key": key}}
		if _, ok := piKey(m, "anthropic"); ok != bound {
			t.Errorf("piKey(%q) bound = %v, want %v", key, ok, bound)
		}
	}
}

// pi's settings.json in the VM: without the keys that would undo devvm's (its proxy, a session
// folder outside the history, a self-update command), named; a file without them as it is; the
// trust that loads every project's .pi said; one that isn't a JSON object refused.
func TestPiSettings(t *testing.T) {
	in := []byte(`{"theme":"dark","httpProxy":"http://proxy.example:8080","sessionDir":"/tmp/s",` +
		`"selfUpdateCommand":["sh","-c","x"],"defaultProjectTrust":"always"}`)
	out, dropped, trustsAll, err := PiSettings(in)
	must(t, err)
	var m map[string]any
	must(t, json.Unmarshal(out, &m))
	if !reflect.DeepEqual(m, map[string]any{"theme": "dark", "defaultProjectTrust": "always"}) ||
		!slices.Equal(dropped, []string{"httpProxy", "selfUpdateCommand", "sessionDir"}) || !trustsAll {
		t.Errorf("PiSettings = %s, %v, %v", out, dropped, trustsAll)
	}
	plain := []byte(`{"theme":"light"}`)
	if out, dropped, trustsAll, err := PiSettings(plain); err != nil || string(out) != string(plain) || dropped != nil || trustsAll {
		t.Errorf("PiSettings of a plain file = %s, %v, %v, %v", out, dropped, trustsAll, err)
	}
	for _, bad := range []string{`[]`, `{`, `"x"`} {
		if _, _, _, err := PiSettings([]byte(bad)); err == nil {
			t.Errorf("PiSettings(%s) took it", bad)
		}
	}
}

// devvm agent login pi --api-key: pi's entry for the provider in its own format, its env kept, and
// its other providers kept; a provider devvm doesn't put a key in for is refused.
func TestPiSetKey(t *testing.T) {
	if got := KeyProviders(Pi); !slices.Equal(got, []string{"anthropic", "openai", "openrouter"}) {
		t.Errorf("KeyProviders(pi) = %v", got)
	}
	p := storeFile(t, Pi, ".pi/agent/auth.json")
	must(t, p.lockStoreWrite(map[string]any{
		"openai": map[string]any{"type": "api_key", "key": "old", "env": map[string]any{"OPENAI_ORG": "org-1"}},
		"groq":   map[string]any{"type": "api_key", "key": "gsk"},
	}))
	must(t, p.SetKey("openai", "sk-new"))
	must(t, p.SetKey("anthropic", "sk-ant"))
	m := readStore(t, p)
	switch e := obj(m, "openai"); {
	case str(e, "type") != "api_key" || str(e, "key") != "sk-new" || str(obj(e, "env"), "OPENAI_ORG") != "org-1":
		t.Errorf("pi's openai entry: %v", e)
	case str(obj(m, "anthropic"), "key") != "sk-ant" || str(obj(m, "groq"), "key") != "gsk":
		t.Errorf("pi's login: %v", m)
	}
	if err := p.SetKey("groq", "k"); err == nil {
		t.Error("SetKey(pi, groq) took it")
	}
	if bs, _ := p.Bindings(); len(bs) != 2 {
		t.Errorf("pi's bindings: %v", bs)
	}
}
