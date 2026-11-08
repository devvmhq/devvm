package settings

import (
	"errors"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/state"
)

// config.toml's [pass-keys]: opencode's and pi's lists of providers, each
// agent's own, none without it, and none for an agent it has no key for; a
// name that isn't a provider's ID, a repeat or an agent devvm has no key
// for (Codex) is refused, naming its line.
func TestPassKeys(t *testing.T) {
	d := testDirs(t)
	for _, c := range []struct {
		config       string
		pi, opencode []string
	}{
		{"", nil, nil},
		{"[pass-keys]\n", nil, nil},
		{"[pass-keys]\npi = []\n", []string{}, nil},
		{"[pass-keys]\npi = [\"ollama\", \"lm-studio\"]\n", []string{"ollama", "lm-studio"}, nil},
		{"[pass-keys]\nopencode = [\"ollama\"]\n", nil, []string{"ollama"}},
		{"[pass-keys]\nopencode = [\"llama.cpp\"]\npi = [\"ollama\"]\n", []string{"ollama"}, []string{"llama.cpp"}},
	} {
		write(t, d.ConfigFile(), c.config)
		for agent, want := range map[string][]string{"pi": c.pi, "opencode": c.opencode} {
			if got, err := PassKeys(d, agent); err != nil || !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Errorf("%q for %s: %q, %v; want %q", c.config, agent, got, err, want)
			}
		}
		if got, err := PassKeys(d, "codex"); err != nil || got != nil {
			t.Errorf("%q for codex: %q, %v", c.config, got, err)
		}
	}
	for config, line := range map[string]int{
		"[pass-keys]\npi = [\"Ollama\"]\n":               2,
		"[pass-keys]\npi = [\"a\", \"a\"]\n":             2,
		"[pass-keys]\npi = \"ollama\"\n":                 2,
		"[pass-keys]\nopencode = [\"Not One\"]\n":        2,
		"# x\n[pass-keys]\ncodex = [\"ollama\"]\n":       3,
		"[pass-keys]\npi = []\nnone = [\"ollama\"]\n":    3,
		"[pass-keys]\nopencode = []\nclaude = [\"x\"]\n": 3,
	} {
		write(t, d.ConfigFile(), config)
		_, err := PassKeys(d, "pi")
		if c, ok := errors.AsType[*state.CorruptError](err); !ok || c.Line != line {
			t.Errorf("%q: %v, want line %d", config, err, line)
		}
	}
}
