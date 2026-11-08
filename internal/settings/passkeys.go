package settings

import (
	"maps"
	"regexp"
	"slices"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// passKeys is config.toml's [pass-keys] table: the providers, per agent,
// whose API keys a VM gets as they are, the ones the VM's proxy can't put
// in, such as a local model server's, which devvm otherwise withholds. Its
// keys are the names of the agents it takes (agents.Def's PassKeys); any
// other is an unknown key (passKeys.unknown).
type passKeys map[string]*list[providerKind]

// unknown is the first of p's keys, sorted, that isn't an agent [pass-keys]
// takes, as its key in config.toml, or "".
func (p passKeys) unknown() string {
	for _, k := range slices.Sorted(maps.Keys(p)) {
		if d, ok := agents.Get(k); !ok || !d.PassKeys {
			return "pass-keys." + k
		}
	}
	return ""
}

// refusal is the message the check refused one of p's lists with.
func (p passKeys) refusal() (msg.M, bool) {
	for _, k := range slices.Sorted(maps.Keys(p)) {
		if m, ok := p[k].refusal(); ok {
			return m, true
		}
	}
	return msg.M{}, false
}

// PassKeys returns the providers whose API keys config.toml's
// [pass-keys] says the agent's VMs get as they are, nil for an agent
// it has no key for. No command saves it.
func PassKeys(d paths.Dirs, agent string) ([]string, error) {
	cf, err := decodeConfig(d.ConfigFile())
	if err != nil {
		return nil, err
	}
	if l := cf.PassKeys[agent]; l != nil {
		return l.v, nil
	}
	return nil, nil
}

// providerName is a provider's ID, as an agent's login file names it:
// lower case letters, digits, dots, dashes and underscores.
var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type providerKind struct{}

func (providerKind) parse(v any) (string, error) {
	return checked(text(v))(func(s string) error {
		if !providerName.MatchString(s) {
			return errText("not a provider's ID, such as \"ollama\": " + s)
		}
		return nil
	})
}
