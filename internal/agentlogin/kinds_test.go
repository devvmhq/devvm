package agentlogin

import (
	"maps"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
)

// The login kinds, as the tests name them.
const (
	Codex       = agents.Codex
	Opencode    = agents.Opencode
	Claude      = agents.Claude
	Pi          = agents.Pi
	ClaudeOAuth = agents.ClaudeOAuth
)

// Every agent's login file has a format, and every format is an agent's login file's: one
// with API keys (agents.Keys) can save them, and none can that has none to save.
func TestFormatsCoverLogins(t *testing.T) {
	kinds := agents.LoginKinds()
	if got := slices.Sorted(maps.Keys(formats())); !slices.Equal(got, slices.Sorted(slices.Values(kinds))) {
		t.Errorf("formats are for %v, the agents' login files are %v", got, kinds)
	}
	for _, d := range agents.Defs {
		_, keys := formats()[d.Name].(keyFormat)
		if keys != (d.Keys != agents.NoKeys) {
			t.Errorf("%s: its login's format saves API keys: %v; its Keys: %v", d.Name, keys, d.Keys)
		}
		if len(d.Logins) > 0 && d.Logins[0].Kind != d.Name {
			t.Errorf("%s: its first login file is of kind %s, not its name's", d.Name, d.Logins[0].Kind)
		}
		_, chatGPT := formats()[d.Name].(chatGPTFormat)
		if chatGPT != d.ChatGPT {
			t.Errorf("%s: its login's format holds a ChatGPT login: %v; its ChatGPT: %v", d.Name, chatGPT, d.ChatGPT)
		}
	}
}
