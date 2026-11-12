package vm

// Claude Code's own code in internal/vm: the hooks the code for every agent
// calls, which do something for Claude Code alone. Its ~/.claude.json is
// both its settings and its login file, which devvm merges into the VM's
// own and saves a part of, and devvm keeps one Claude Code login, its OAuth
// login or a setup token's secret.

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/tools"
)

// claude logs Claude Code in in the VM, its login's code pasted on this
// terminal, ok false for another agent: the VM's proxy saves its
// subscription's login in the store, or claude setup-token's token as the
// claude secret, and takes the other away. Then the VM is set up again, so
// it gets the login devvm keeps now, its token in the environment or its
// OAuth login's file, never both, and Run says which it saved.
func (e AgentLogin) claude(ctx context.Context, t *target, conf backend.Confinement, o AgentLoginOptions) (bool, error) {
	if o.Agent != agents.Claude {
		return false, nil
	}
	vm, store := t.f.Name, e.Proxy.Secrets
	oauth, token, err := secrets.ClaudeOf(store).Sums(ctx)
	if err != nil {
		return true, err
	}
	if err := e.login(ctx, t, o); err != nil {
		return true, err
	}
	oauth2, token2, err := secrets.ClaudeOf(store).Sums(ctx)
	if err != nil {
		return true, err
	}
	if o.SetupToken && (token2 == "" || token2 == token) || !o.SetupToken && (oauth2 == "" || oauth2 == oauth) {
		return true, msg.AgentLoginNotSaved(o.Agent, vm)
	}
	if err := e.launcher.syncer.sync(ctx, t.b, t.st, t.f.Slug(), tools.Files(t.st), conf); err != nil {
		return true, err
	}
	if o.SetupToken {
		e.Info(msg.ClaudeSetupTokenSaved(vm))
	} else {
		e.Info(msg.AgentOwnLoginSaved(agents.Of(o.Agent).Display, vm))
	}
	if secrets.ClaudeOf(store).Env() != "" {
		e.Warn(msg.ClaudeEnvTokenWins())
	}
	return true, nil
}

// claudeSyncName is the name a launch's setup gives the VM's copy of the
// login of kind, merged true: Claude Code's .claude.json, claude.json,
// which sync.sh merges into the VM's own rather than copying it. Any other
// kind's is copied to its place below the home folder: "", false.
func claudeSyncName(kind string) (string, bool) {
	if kind != agents.Claude {
		return "", false
	}
	return "claude.json", true
}

// claudeTokenUnsaved warns, as a launch's setup finds the VM's secret name
// with no value saved, that the claude secret has none, and reports
// whether it was the claude secret: no warning while devvm keeps Claude
// Code's OAuth login, which the VM gets instead (stageSecrets), never both.
func claudeTokenUnsaved(store *secrets.Store, name, vm string, warn func(msg.M)) bool {
	if name != secrets.ClaudeToken {
		return false
	}
	if !secrets.ClaudeOf(store).OAuthInstead(name) {
		warn(msg.NoClaudeToken(vm))
	}
	return true
}

// claudeTokenInvalid reports whether v, the saved value of the VM's
// secret name, is a Claude Code token that isn't one from claude
// setup-token, so the VM doesn't get it.
func claudeTokenInvalid(name, v string) bool {
	return name == secrets.ClaudeToken && !strings.HasPrefix(v, "sk-ant-oat01-")
}

// claudeSaves reports whether devvm agent save saves a part of the agent's
// login file too: Claude Code's ~/.claude.json (saver.claudeJSON).
func claudeSaves(agent string) bool { return agent == agents.Claude }

// claudeSaveLines are agentSaveScript's lines that send what devvm's store
// keeps of ~/.claude.json (agentlogin.ClaudeTopKeys), when names has Claude
// Code, so its per-project entries never leave the VM.
func claudeSaveLines(names []string) []string {
	if !slices.Contains(names, agents.Claude) {
		return nil
	}
	keys, _ := json.Marshal(agentlogin.ClaudeTopKeys())
	own := `"$t/` + saveOwn
	return []string{
		`if [ -f ` + agents.ClaudeJSON + ` ] && [ ! -L ` + agents.ClaudeJSON + ` ]; then`,
		`  jq --argjson k ` + shell.Quote(string(keys)) + ` 'with_entries(select(.key as $x | any($k[]; . == $x)))' ` + agents.ClaudeJSON + ` > ` + own + `/claude.json" 2>/dev/null ||`,
		`    { rm -f ` + own + `/claude.json"; : > ` + own + `/claude-unreadable"; }`,
		"fi",
	}
}

// claudeHostLogin reads this machine's login file of the agent at path for
// devvm agent import: Claude Code's ~/.claude.json without its projects'
// history or API key (agentlogin.ClaudeCopy), another agent's as it is.
func claudeHostLogin(agent, path string) ([]byte, error) {
	if agent == agents.Claude {
		return agentlogin.ClaudeCopy(path)
	}
	return os.ReadFile(path) //nolint:gosec // the user's own login file, not a link
}

// claudePlugins is the agent's settings file p, data, as devvm agent import
// copies it from folder on this machine into store: Claude Code's plugin
// records name their folders by path, so the copy names them in the store,
// where VMs mount them. Any other file is as it is.
func claudePlugins(agent, p string, data []byte, folder, store string) []byte {
	if agent != agents.Claude || path.Dir(p) != "plugins" || path.Ext(p) != ".json" {
		return data
	}
	return renamePrefix(data, filepath.Join(folder, "plugins"), filepath.Join(store, "plugins"))
}
