package agentlogin

// pi (checked against upstream at 1.5.0, 2026-11) keeps its logins in ~/.pi/agent/auth.json, one
// entry per provider: {"type":"api_key","key","env"}, where a key that starts with ! is a command
// pi runs for the key, a key can be a $NAME or ${NAME} template ($$ a literal $) filled from env or
// the environment, and env's values win over the process environment; or
// {"type":"oauth","access","refresh","expires" (ms)} with the provider's own fields.
//
// The VM's copy (piVM) holds each entry in one of three ways:
//   - bound: an api_key entry of a provider whose hosts devvm knows (opencodeAPI, the same providers
//     as opencode's) with a key that's a value, neither a command nor a template. The VM gets
//     {type, key} alone, its key a placeholder the proxy fills in; its env stays on the host.
//   - passed: an api_key entry of a provider the user listed (File.Pass, config.toml's
//     [pass-keys]), such as a local model server's, which goes as it is.
//   - withheld: every other entry, OAuth logins included. The VM gets only the fields in
//     withheldKeep (withheldEntry), so no key, token or env value of it reaches the VM.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// piPlaceholder is the VM's in place of a provider's API key in pi's auth.json.
func piPlaceholder(provider, salt string) string {
	return salted("devvm-proxy-managed-pi-"+provider, salt)
}

// piTemplate matches a pi key's $NAME or ${NAME}, once its literal $$ are taken out.
var piTemplate = regexp.MustCompile(`\$\{?[A-Za-z_]`)

// piKey returns the API key of pi's entry for provider id in m, and whether devvm binds it: an
// api_key entry of a provider in opencodeAPI whose key is a value, not a command or a template.
func piKey(m map[string]any, id string) (string, bool) {
	e := obj(m, id)
	k := str(e, "key")
	_, known := opencodeAPI[id]
	value := k != "" && !strings.HasPrefix(k, "!") && !piTemplate.MatchString(strings.ReplaceAll(k, "$$", ""))
	return k, known && str(e, "type") == "api_key" && value
}

func piBindings(m map[string]any, salt string) []proxy.Binding {
	var bs []proxy.Binding
	for _, id := range sortedKeys(m) {
		if _, ok := piKey(m, id); ok {
			p := opencodeAPI[id]
			bs = append(bs, login(agents.Pi, "pi-"+id, p.hosts, p.header, piPlaceholder(id, salt)))
		}
	}
	return bs
}

// piPassed reports whether pi's entry for provider id goes into the VM's copy as it is: an api_key
// entry of a provider in pass that devvm doesn't bind.
func piPassed(m map[string]any, id string, pass []string) bool {
	_, bound := piKey(m, id)
	return !bound && str(obj(m, id), "type") == "api_key" && slices.Contains(pass, id)
}

// piVM is the VM's copy of pi's logins m: bound entries as {type, key} with their placeholders,
// passed ones as they are, and withheld ones as withheldEntry makes them.
func piVM(m map[string]any, salt string, pass []string) map[string]any {
	out := map[string]any{}
	for _, b := range piBindings(m, salt) {
		out[strings.TrimPrefix(b.Name, "pi-")] = map[string]any{"type": "api_key", "key": b.Placeholder}
	}
	for _, id := range sortedKeys(m) {
		if _, done := out[id]; done {
			continue
		}
		if piPassed(m, id, pass) {
			out[id] = m[id]
			continue
		}
		out[id] = withheldEntry(obj(m, id))
	}
	return out
}

// piSplit returns the providers of pi's whose entries the VM's copy holds as they are (passed)
// and those it holds without their keys and tokens (neither bound nor passed), each sorted.
func piSplit(m map[string]any, pass []string) (passed, withheld []string) {
	return splitUnbound(m,
		func(id string) bool { _, bound := piKey(m, id); return bound },
		func(id string) bool { return piPassed(m, id, pass) })
}

// piVMDropped are the keys of pi's settings.json that the VM's copy leaves out, as each would undo
// what devvm sets in a VM: httpProxy replaces the VM's proxy for pi, sessionDir moves pi's sessions
// out of what a history export holds, and selfUpdateCommand runs a command of the file's on pi
// update.
var piVMDropped = []string{"httpProxy", "sessionDir", "selfUpdateCommand"}

// PiSettings returns pi's settings.json, data, as the VM's copy holds it: without piVMDropped's
// keys, which it names, sorted, and whether it trusts every project folder (defaultProjectTrust
// "always"), so pi loads a project's own .pi resources without asking. A file that doesn't parse
// as a JSON object is an error, as Redact's is.
func PiSettings(data []byte) (out []byte, dropped []string, trustsAll bool, err error) {
	v, err := decode("json", data)
	if err != nil {
		return nil, nil, false, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, false, errors.New("not a JSON object")
	}
	for _, k := range piVMDropped {
		if _, ok := m[k]; ok {
			delete(m, k)
			dropped = append(dropped, k)
		}
	}
	slices.Sort(dropped)
	if dropped == nil {
		out = data
	} else if out, err = encode("json", m); err != nil {
		return nil, nil, false, err
	}
	return out, dropped, str(m, "defaultProjectTrust") == "always", nil
}

func (f File) piGet(name string) (string, error) {
	m, err := readJSON(f.Path)
	if err != nil {
		return "", err
	}
	if k, ok := piKey(m, strings.TrimPrefix(name, "pi-")); ok {
		return k, nil
	}
	return "", fmt.Errorf("no pi API key %q", name)
}
