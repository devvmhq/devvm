package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// oneSecret takes one argument: a secret devvm keeps, or a name one of
// yours may have (secrets.CheckName), which completion offers from those
// saved. naming is whether the command may name a new one (devvm secret
// set), whose name's error says what a name may be; for any other a name
// no secret of yours can have is no_secret.
func oneSecret(cmd *cobra.Command, a *app, naming bool) {
	cmd.Args = cobra.MatchAll(exactArgs(1), func(_ *cobra.Command, args []string) error {
		if _, ok := secrets.Lookup(args[0]); ok {
			return nil
		}
		err := secrets.CheckName(args[0])
		if err != nil && !naming {
			return msg.NoSecret(args[0])
		}
		return err
	})
	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var names []string
		for _, s := range secrets.Known {
			names = append(names, s.Name)
		}
		if store, err := a.secrets(); err == nil {
			if cs, err := secrets.YoursOf(store).List(); err == nil {
				for _, s := range cs {
					names = append(names, s.Name)
				}
			}
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// newSecretCmd is devvm secret: the secrets kept on this machine, listed.
func newSecretCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "secret",
		Annotations: about{docs: "safety/network#saving-and-binding-secrets", group: groupChange}.annotations(),
		Short:       "List the secrets kept on this machine, for VMs to use",
		Long: "List the secrets devvm keeps on this machine, claude for the Claude Code token, github for a GitHub token, " +
			"proxy for the password of the company proxy that config.toml names, and your own, " +
			"with each one's hosts, header and variable, whether it's saved and the VMs using it. " +
			"A VM gets a placeholder, and its proxy puts the secret into requests for the secret's hosts. " +
			"No VM gets proxy, which devvm's proxies send only to the company proxy.",
		Example: examples(
			"devvm secret", "list the secrets and the VMs using each",
			"devvm secret set github", "save a GitHub token, asking for it",
			"devvm secret set stripe --host api.stripe.com --env STRIPE_API_KEY --prefix sk_", "save a secret of your own for api.stripe.com",
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

// secretSetOpts are devvm secret set's flags: --stdin, and what they give
// a secret of yours.
type secretSetOpts struct {
	stdin  bool
	change secrets.Change
}

func newSecretSetCmd(a *app) *cobra.Command {
	var o secretSetOpts
	cmd := &cobra.Command{
		Use: "set NAME",
		Annotations: about{
			docs:   "safety/network#saving-and-binding-secrets",
			noJSON: "it saves a secret",
		}.annotations(),
		Short: "Save a secret on this machine",
		Long: "Save a secret on this machine: claude for the Claude Code token that claude setup-token prints, " +
			"github for a GitHub token, proxy for the password of the company proxy that config.toml names, " +
			"or one of your own, which needs --host and --env the first time and keeps what it has of them after. " +
			"devvm asks for it without showing it, and --stdin reads a line instead. " +
			"Proxies that are running put a new value in at once, and a VM's proxy reads proxy as it starts.",
		Example: examples(
			"devvm secret set claude", "save the Claude Code token, asking for it",
			"gh auth token | devvm secret set github --stdin", "save GitHub CLI's token without asking",
			"devvm secret set stripe --host api.stripe.com --env STRIPE_API_KEY --prefix sk_", "save a Stripe key, sent to api.stripe.com alone",
			"devvm secret set openrouter --host openrouter.ai --env OPENROUTER_API_KEY", "save an API key, sent as Authorization: Bearer",
			"devvm secret set stripe", "save a new value for a secret of yours, keeping its hosts",
			"devvm secret set proxy", "save the company proxy's password, asking for it",
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			return saveSecret(cmd, a, args[0], o)
		},
	}
	oneSecret(cmd, a, true)
	cmd.Args = cobra.MatchAll(cmd.Args, func(c *cobra.Command, args []string) error {
		// Each flag's value is checked as it's given (checkFlagValue).
		o.change.BasicUserSet = c.Flags().Changed("basic-user")
		if _, ok := secrets.Lookup(args[0]); ok && !o.change.Empty() {
			return msg.SecretFixed(args[0])
		}
		return nil
	})
	f := cmd.Flags()
	f.BoolVar(&o.stdin, "stdin", false, "read the secret from a line of input, without asking")
	f.StringSliceVar(&o.change.Hosts, "host", nil, "a `HOST` that a secret of your own goes to, such as api.example.com: repeat it, or join hosts with commas")
	f.StringVar(&o.change.Header, "header", "", "the `HEADER` that a secret of your own goes in, such as X-Api-Key (default: Authorization)")
	f.StringVar(&o.change.Env, "env", "", "the `VARIABLE` that holds the placeholder of a secret of your own in VMs, such as STRIPE_API_KEY")
	f.StringVar(&o.change.Prefix, "prefix", "", "the `PREFIX` that the placeholder of a secret of your own starts with, such as sk_, for a client that checks how a key starts")
	f.StringVar(&o.change.Scheme, "scheme", "", "the `SCHEME` of a secret of your own in Authorization besides Bearer and Token, such as ApiKey, or none")
	f.StringVar(&o.change.Path, "path", "", "the `PATH` of the requests a secret of your own goes in, such as /api/ for everything below /api/, or none for every path")
	f.StringVar(&o.change.Methods, "method", "", "the `METHODS` of the requests a secret of your own goes in, such as GET,HEAD, or none for every method")
	f.BoolVar(&o.change.BasicUser, "basic-user", false, "let a secret of your own go in as the username of Basic credentials, as Stripe takes a key")
	return cmd
}

// saveSecret asks for a secret's token (with stdin, reads a line) and
// saves it: a known one, or one of yours, with its flags over it as it's
// saved.
func saveSecret(cmd *cobra.Command, a *app, name string, o secretSetOpts) error {
	u := a.u
	var sec secrets.Secret
	if err := a.env(func(env *vm.Env) error {
		var err error
		sec, err = vm.Net{Env: env}.SecretToSave(name, o.change)
		return err
	}, msg.SecretFailed); err != nil {
		return err
	}
	var given redact.Secret
	if o.stdin {
		given = redact.NewSecret(u.Line())
	} else {
		howto, prompt := msg.GithubTokenHowto(), msg.TokenPrompt()
		switch {
		case sec.Custom:
			howto = msg.CustomSecretHowto(strings.Join(sec.Hosts, ", "), name)
		case name == secrets.ClaudeToken:
			howto = msg.ClaudeTokenHowto()
		case name == proxy.UpstreamSecret:
			howto, prompt = msg.ProxyPasswordHowto(), msg.PasswordPrompt()
		}
		var err error
		if given, err = u.Secret(prompt.AnsweredBy("--stdin").About(howto)); err != nil {
			return err
		}
	}
	return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretSave(cmd.Context(), sec, given) }, msg.SecretFailed)
}

func newSecretRmCmd(a *app) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use: "rm NAME",
		Annotations: about{
			docs:   "safety/network#saving-and-binding-secrets",
			noJSON: "it deletes",
		}.annotations(),
		Short: "Delete a saved secret",
		Long: "Delete a saved secret, after asking in a terminal without --no-input. " +
			"A secret of your own goes with its hosts, header and variable, and the VMs using it stop using it. " +
			"Proxies that are running stop putting it in at once, but what a VM did with it stays done.",
		Example: examples(
			"devvm secret rm github", "delete the GitHub token, after asking",
			"devvm secret rm github --force", "delete it without asking, as a script must",
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretRm(cmd.Context(), args[0], force) }, msg.SecretFailed)
		},
	}
	oneSecret(cmd, a, false)
	addForceFlag(cmd.Flags(), &force)
	return cmd
}

// secretOutput is devvm secret --json: each secret's name, whether it's
// one of yours, its hosts, header and variable, whether it's saved and
// the VMs using it, never its value.
type secretOutput struct {
	Secrets []secretJSON `json:"secrets"`
}

type secretJSON struct {
	Name   string      `json:"name"`
	Saved  bool        `json:"saved"`
	UsedBy []vmname.VM `json:"used_by"`
	Custom bool        `json:"custom"`
	Hosts  []string    `json:"hosts"`
	Header string      `json:"header,omitempty"`
	Env    string      `json:"env,omitempty"`
	Prefix string      `json:"prefix,omitempty"`
	// A secret of yours' own scheme, path, methods and way into Basic
	// credentials.
	Scheme    string `json:"scheme,omitempty"`
	Path      string `json:"path,omitempty"`
	Methods   string `json:"methods,omitempty"`
	BasicUser bool   `json:"basic_user,omitempty"`
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
			out.Secrets = append(out.Secrets, secretJSON{
				Name: s.Name, Saved: s.Saved, UsedBy: append([]vmname.VM{}, s.UsedBy...),
				Custom: s.Custom, Hosts: append([]string{}, s.Hosts...), Header: s.Header, Env: s.Env, Prefix: s.Prefix,
				Scheme: s.Scheme, Path: s.Path, Methods: s.Methods, BasicUser: s.BasicUser,
			})
		}
		return printJSON(u, out)
	}
	tab := u.Table()
	tab.Header("NAME", "SAVED", "VARIABLE", "HEADER", "HOSTS", "PATH", "METHODS", "USED BY")
	for _, s := range ss {
		saved := "no"
		if s.Saved {
			saved = "yes"
		}
		tab.Row(cell(s.Name), cell(saved), cell(s.Env), cell(s.Header), cell(strings.Join(s.Hosts, ", ")), cell(s.Path), cell(s.Methods),
			cell(vmname.Join(s.UsedBy, ", ")))
	}
	return tab.Flush()
}

// newSecretUseCmd is devvm secret use (use) or unuse.
func newSecretUseCmd(a *app, name string, use bool) *cobra.Command {
	short, long := "Let this folder's VM use a secret", "Let this folder's VM use a secret, and reach its hosts on port 443. "+
		"Its proxy puts the secret in at once, and the VM gets its placeholder from its next launch."
	example := examples(
		"devvm secret use github", "let the VM use your GitHub token",
		"devvm secret use stripe", "let the VM use a secret of yours",
	)
	if !use {
		short, long = "Stop this folder's VM using a secret", "Stop this folder's VM using a secret, "+
			"which its proxy stops putting in at once. "+
			"A VM whose image has claude always uses claude."
		example = examples("devvm secret unuse github", "stop the VM using your GitHub token")
	}
	cmd := &cobra.Command{
		Use: name + " NAME",
		Annotations: about{
			docs:   "safety/network#saving-and-binding-secrets",
			noJSON: "it changes a setting",
		}.annotations(),
		Short:   short,
		Long:    long,
		Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.SecretUse(cmd.Context(), args[0], use) }, msg.SecretFailed)
		},
	}
	oneSecret(cmd, a, false)
	return addFolderFlags(cmd, a)
}
