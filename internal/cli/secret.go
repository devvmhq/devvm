package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/vm"
)

// secretNames are the secrets devvm keeps, for completion and checks.
func secretNames() []string {
	names := make([]string, len(secrets.Known))
	for i, s := range secrets.Known {
		names[i] = s.Name
	}
	return names
}

// oneSecret takes one argument: a secret devvm keeps.
func oneSecret(cmd *cobra.Command) {
	cmd.Args = exactArgs(1)
	cmd.ValidArgs = secretNames()
}

// newSecretCmd is devvm secret: the secrets kept on this machine, listed.
func newSecretCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "List the secrets kept on this machine, for VMs to use",
		Long: "List the secrets devvm keeps on this machine, claude for the Claude Code token, github for a GitHub token " +
			"and proxy for the password of the company proxy that config.toml names, " +
			"with whether each is saved and the VMs using it. " +
			"A VM gets a placeholder, and its proxy puts the secret into requests for the secret's hosts. " +
			"No VM gets proxy, which devvm's proxies send only to the company proxy.",
		Example: examples(
			"devvm secret", "list the secrets and the VMs using each",
			"devvm secret set github", "save a GitHub token, asking for it",
			"devvm secret use github", "let this folder's VM use it",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listSecrets(cmd, a, asJSON) },
	}
	jsonFlag(cmd, &asJSON)
	cmd.AddCommand(newSecretSetCmd(a), newSecretRmCmd(a),
		newSecretUseCmd(a, "use", true), newSecretUseCmd(a, "unuse", false))
	return cmd
}

func newSecretSetCmd(a *app) *cobra.Command {
	var stdin bool
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Save a secret on this machine",
		Long: "Save a secret on this machine, claude for the Claude Code token that claude setup-token prints, " +
			"github for a GitHub token, or proxy for the password of the company proxy that config.toml names. " +
			"devvm asks for it without showing it, and --stdin reads a line instead. " +
			"Proxies that are running put a new token in at once, and a VM's proxy reads proxy as it starts.",
		Example: examples(
			"devvm secret set claude", "save the Claude Code token, asking for it",
			"gh auth token | devvm secret set github --stdin", "save GitHub CLI's token without asking",
			"devvm secret set proxy", "save the company proxy's password, asking for it",
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			return saveSecret(cmd, a, args[0], stdin)
		},
	}
	oneSecret(cmd)
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the secret from a line of input, without asking")
	return cmd
}

// saveSecret asks for a secret's token (with stdin, reads a line) and
// saves it.
func saveSecret(cmd *cobra.Command, a *app, name string, stdin bool) error {
	u := a.u
	var given string
	if stdin {
		given = u.Line()
	} else {
		howto, prompt := msg.GithubTokenHowto(), msg.TokenPrompt()
		switch name {
		case secrets.ClaudeToken:
			howto = msg.ClaudeTokenHowto()
		case proxy.UpstreamSecret:
			howto, prompt = msg.ProxyPasswordHowto(), msg.PasswordPrompt()
		}
		var err error
		if given, err = u.Secret(prompt.AnsweredBy("--stdin").About(howto)); err != nil {
			return err
		}
	}
	return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretSave(cmd.Context(), name, given) }, msg.SecretFailed)
}

func newSecretRmCmd(a *app) *cobra.Command {
	u := a.u
	var force bool
	cmd := &cobra.Command{
		Use:   "rm NAME",
		Short: "Delete a saved secret",
		Long: "Delete a saved secret, after asking in a terminal without --no-input. " +
			"Proxies that are running stop putting it in at once, but what a VM did with it stays done.",
		Example: examples(
			"devvm secret rm github", "delete the GitHub token, after asking",
			"devvm secret rm github --force", "delete it without asking, as a script must",
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok := force
			if !ok {
				var err error
				if ok, err = u.Ask(msg.SecretRmConfirm(args[0]).AnsweredBy("--force")); err != nil {
					return err
				}
			}
			if !ok {
				u.Info(msg.NothingDeleted())
				return nil
			}
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretRm(cmd.Context(), args[0]) }, msg.SecretFailed)
		},
	}
	oneSecret(cmd)
	addForceFlag(cmd.Flags(), &force)
	return cmd
}

// secretOutput is devvm secret --json: each secret's name, whether it's
// saved and the VMs using it, never its value.
type secretOutput struct {
	Secrets []secretJSON `json:"secrets"`
}

type secretJSON struct {
	Name   string   `json:"name"`
	Saved  bool     `json:"saved"`
	UsedBy []string `json:"used_by"`
}

// listSecrets writes the secrets as a table, or with asJSON as
// secretOutput.
func listSecrets(cmd *cobra.Command, a *app, asJSON bool) error {
	u := a.u
	var ss []vm.SecretListed
	if err := a.env(func(env *vm.Env) error {
		var err error
		ss, err = vm.Net{Env: env}.Secrets(cmd.Context())
		return err
	}, msg.SecretFailed); err != nil {
		return err
	}
	if asJSON {
		out := secretOutput{Secrets: make([]secretJSON, 0, len(ss))}
		for _, s := range ss {
			out.Secrets = append(out.Secrets, secretJSON{Name: s.Name, Saved: s.Saved, UsedBy: append([]string{}, s.UsedBy...)})
		}
		return printJSON(u, out)
	}
	tab := u.Table()
	tab.Header("NAME", "SAVED", "USED BY")
	for _, s := range ss {
		saved := "no"
		if s.Saved {
			saved = "yes"
		}
		tab.Row(cell(s.Name), cell(saved), cell(strings.Join(s.UsedBy, ", ")))
	}
	return tab.Flush()
}

// newSecretUseCmd is devvm secret use (use) or unuse.
func newSecretUseCmd(a *app, name string, use bool) *cobra.Command {
	short, long := "Let this folder's VM use a secret", "Let this folder's VM use a secret. "+
		"Its proxy puts the secret in at once, and the VM gets its placeholder from its next launch."
	example := examples("devvm secret use github", "let the VM use your GitHub token")
	if !use {
		short, long = "Stop this folder's VM using a secret", "Stop this folder's VM using a secret, "+
			"which its proxy stops putting in at once. "+
			"A VM whose image has claude always uses claude."
		example = examples("devvm secret unuse github", "stop the VM using your GitHub token")
	}
	cmd := &cobra.Command{
		Use:     name + " NAME",
		Short:   short,
		Long:    long,
		Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretUse(cmd.Context(), args[0], use) }, msg.SecretFailed)
		},
	}
	oneSecret(cmd)
	return addFolderFlags(cmd, a)
}
