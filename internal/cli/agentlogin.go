package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// agentLoginOpts are devvm agent login's flags.
type agentLoginOpts struct {
	device, apiKey, stdin, setupToken bool
	provider                          string
}

// newAgentLoginCmd is devvm agent login AGENT.
func newAgentLoginCmd(a *app) *cobra.Command {
	var o agentLoginOpts
	cmd := &cobra.Command{
		Use:   "login AGENT",
		Short: "Log an agent in, in this folder's VM, and save the login in devvm's store",
		Long: "Log AGENT in inside this folder's VM, and keep the login in devvm's store for every VM with the agent, which gets only placeholders. " +
			"claude logs in to your Claude subscription in a browser on any machine, whose code you paste back, " +
			"or gets a long-lived Claude Code token with --setup-token. " +
			andList(agents.Select(func(d agents.Def) bool { return d.ChatGPT })) +
			" log in to ChatGPT in a browser on this machine, or with a code with --device-code, " +
			"or save an API key with --api-key, which is " + andList(possessive(agents.Select(agents.Def.KeysOnly))) + " only login.",
		Example: examples(
			"devvm agent login claude", "log Claude Code in to your Claude subscription",
			"devvm agent login claude --setup-token", "get a Claude Code token with claude setup-token instead",
			"devvm agent login codex", "log Codex in to ChatGPT in a browser on this machine",
			"devvm agent login codex --device-code", "log in with a code, in a browser on any machine",
			"devvm agent login opencode --api-key --provider anthropic", "save opencode's Anthropic API key, asking for it",
			"devvm agent login pi --api-key --provider openai", "save pi's OpenAI API key, asking for it",
		),
		Args:      cobra.MatchAll(exactArgs(1), func(_ *cobra.Command, args []string) error { return o.check(args[0]) }),
		ValidArgs: agents.LoginAgents(),
		RunE: func(cmd *cobra.Command, args []string) error {
			lo := vm.AgentLoginOptions{Agent: args[0], Device: o.device, SetupToken: o.setupToken, Provider: o.provider}
			if o.apiKey {
				key, err := o.askKey(a, lo)
				if err != nil {
					return err
				}
				if strings.TrimSpace(key) == "" {
					return msg.AgentKeyEmpty()
				}
				lo.Key = key
			}
			return a.env(func(env *vm.Env) error { return vm.AgentLogin{Env: env}.Run(cmd.Context(), lo) }, msg.AgentLoginFailed)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.device, "device-code", false, "log in with a code to type into a browser on any machine, so nothing is forwarded")
	f.BoolVar(&o.apiKey, "api-key", false, "save an API key instead, asking for it")
	keyed := agents.Select(func(d agents.Def) bool { return d.Keys == agents.ProviderKeys })
	f.StringVar(&o.provider, "provider", "", "with "+orList(possessive(keyed))+" --api-key, the `PROVIDER` it's for: "+orList(agentlogin.KeyProviders(keyed[0])))
	f.BoolVar(&o.stdin, "stdin", false, "read the secret from a line of input, without asking")
	f.BoolVar(&o.setupToken, "setup-token", false, "with claude, get a long-lived Claude Code token with claude setup-token instead")
	exclusive(cmd, "device-code", "api-key")
	addFolderFlags(cmd, a)
	return cmd
}

// check refuses flags that don't go with agent's login: --setup-token
// only with claude, which takes neither --device-code nor --api-key, an
// agent with API keys alone (pi) only with --api-key, --provider only
// with the --api-key of an agent with a key for each provider, which
// needs it, and --stdin only with --api-key.
func (o agentLoginOpts) check(agent string) error {
	providers := agentlogin.KeyProviders(agent)
	d, _ := agents.Get(agent)
	switch {
	case o.setupToken && agent != agents.Claude:
		return msg.AgentLoginSetupTokenAlone()
	case agent == agents.Claude && o.device:
		return msg.AgentLoginClaudeFlag("--device-code")
	case agent == agents.Claude && o.apiKey:
		return msg.AgentLoginClaudeFlag("--api-key")
	case agent == agents.Claude && o.stdin:
		return msg.AgentLoginClaudeFlag("--stdin")
	case d.KeysOnly() && !o.apiKey:
		return msg.AgentLoginKeyOnly(agent)
	case o.stdin && !o.apiKey:
		return msg.AgentLoginStdinAlone(agent)
	case o.provider != "" && (!o.apiKey || len(providers) == 0):
		keyed := agents.Select(func(d agents.Def) bool { return d.Keys == agents.ProviderKeys })
		return msg.AgentLoginProviderAlone(strings.Join(keyed, ", "))
	case o.apiKey && len(providers) > 0 && o.provider == "":
		return msg.AgentLoginProviderNeeded(agent, strings.Join(providers, ", "))
	case o.provider != "" && !slices.Contains(providers, o.provider):
		return msg.ArgNotChoice(strings.Join(providers, ", "), "--provider", o.provider)
	}
	return nil
}

// askKey asks for the API key without showing it, or with --stdin reads
// a line.
func (o agentLoginOpts) askKey(a *app, lo vm.AgentLoginOptions) (string, error) {
	if o.stdin {
		return a.u.Line(), nil
	}
	provider := lo.Provider
	if provider == "" {
		provider = "openai"
	}
	return a.u.Secret(msg.AgentKeyPrompt().AnsweredBy("--stdin").About(msg.AgentKeyHowto(lo.Agent, provider)))
}
