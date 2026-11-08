package agentlogin

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
)

// KeyProviders are the providers whose API keys devvm agent login saves
// for the agent, sorted, when it saves one for each (agents.ProviderKeys):
// those whose keys the proxy puts in (opencodeAPI). None for an agent with
// one key (agents.OneKey: Codex's, OpenAI's, which needs no name) or none.
// Any other provider's key would reach the VM as it is, so it's never
// saved this way.
func KeyProviders(agent string) []string {
	if d, ok := agents.Get(agent); ok && d.Keys == agents.ProviderKeys {
		return slices.Sorted(maps.Keys(opencodeAPI))
	}
	return nil
}

// errKeyProvider: an API key for a provider devvm doesn't put in.
var errKeyProvider = errors.New("no API key devvm puts in for that provider")

// OpencodeImport is what opencode's auth import reads to give a VM whose opencode has made its
// database the logins of vmCopy, the VM's copy of the store's opencode login (File.VMCopy, which
// holds placeholders and passed keys alone): opencode 2 keeps its logins there and reads auth.json
// only as it makes it. It's each API key the copy holds, the placeholders of KeyProviders' and the
// keys config.toml's [pass-keys] passes with their metadata (a withheld entry holds no key), and
// OpenAI's Codex login, as opencode's auth export prints them, sorted by provider, under IDs of
// devvm's (cred_devvm_<provider>), which the VM's setup logs out of before it imports a new set,
// so the user's own credentials stay.
func OpencodeImport(vmCopy []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(vmCopy))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, id := range slices.Sorted(maps.Keys(m)) {
		e := obj(m, id)
		var v map[string]any
		switch {
		case str(e, "type") == "api" && str(e, "key") != "":
			v = map[string]any{"type": "key", "key": str(e, "key")}
			if md := obj(e, "metadata"); len(md) > 0 {
				v["metadata"] = md
			}
		case str(e, "type") == "oauth" && id == "openai" && str(e, "access") != "":
			// opencode 2 (checked at v2.2.5) needs a method, Codex's in a browser, whose tokens
			// come from the endpoint the store refreshes at, and a whole number for expires.
			expires, _ := e["expires"].(json.Number)
			if _, err := expires.Int64(); err != nil {
				expires = "0"
			}
			v = map[string]any{
				"type": "oauth", "methodID": "chatgpt-browser", "access": str(e, "access"),
				"refresh": str(e, "refresh"), "expires": expires,
			}
			if a := str(e, "accountId"); a != "" {
				v["metadata"] = map[string]any{"accountID": a}
			}
		default:
			continue
		}
		out = append(out, map[string]any{"id": "cred_devvm_" + id, "integrationID": id, "label": "devvm", "active": true, "value": v})
	}
	return json.Marshal(out)
}

// SetKey saves key as the agent's API key in f, the store's login file,
// under its lock, in the agent's own format (its keyFormat): for an agent
// with one key (agents.OneKey) with no provider, as Codex's auth.json
// becomes an API-key login, as codex login --with-api-key makes it, its
// ChatGPT login gone; for one with a key for each provider
// (agents.ProviderKeys) as provider's entry, of KeyProviders, the other
// providers kept (a pi entry's env too).
func (f File) SetKey(provider, key string) error {
	if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
		return errors.New("the API key is empty or more than one line")
	}
	d, _ := agents.Get(f.Kind)
	fm, ok := formats()[f.Kind].(keyFormat)
	switch {
	case !ok:
		return errKeyProvider
	case d.Keys == agents.OneKey && provider == "":
	case d.Keys == agents.ProviderKeys && slices.Contains(KeyProviders(f.Kind), provider):
	default:
		return errKeyProvider
	}
	unlock, err := f.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := readLogin(f.Path)
	if err != nil {
		return err
	}
	return writeRefreshed(f.Path, fm.setKey(m, provider, key))
}

// ChatGPT is the ChatGPT login the store's login file f holds, of a
// format that can hold one (chatGPTFormat): Codex's, or opencode's OpenAI
// login, with the account's email when its tokens name one; ok is false
// when it holds none.
func (f File) ChatGPT() (email string, ok bool, err error) {
	m, err := readLogin(f.Path)
	if err != nil {
		return "", false, err
	}
	var tokens []string
	if fm, ok := formats()[f.Kind].(chatGPTFormat); ok {
		tokens = fm.chatGPT(m)
	}
	if len(tokens) == 0 || tokens[len(tokens)-1] == "" {
		return "", false, nil
	}
	for _, tok := range tokens {
		c := jwtClaims(tok)
		if e := cmp.Or(str(c, "email"), str(obj(c, "https://api.openai.com/profile"), "email")); e != "" {
			return e, true, nil
		}
	}
	return "", true, nil
}
