package cli

import (
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
)

// newNetworkCmd is devvm network [open|internet|learn|allowlist|none],
// with -g for what new VMs get, and allow, deny, rm RULE... [-g], log and
// lock.
func newNetworkCmd(a *app) *cobra.Command {
	u := a.u
	var global, asJSON bool
	choices := netpolicy.Choices
	cmd := &cobra.Command{
		Use:         "network [open|internet|learn|allowlist|none]",
		Annotations: about{docs: "safety/network#choosing-a-tier", group: groupChange}.annotations(),
		Short:       "Show or change the network tier of this folder's VM",
		Long: "Show the network tier of this folder's VM, which is open, internet, allowlist, none or unenforced, and then its allowlist, policy by policy: devvm's built-in ones by name, then your files by path. " +
			"With a tier, give the VM that tier: between the confined tiers at once, and into or out of open from the VM's next start, so in a terminal devvm offers to restart a running VM now, and learn gives it the internet tier while it learns the hosts it reaches, until devvm network lock. " +
			"With -g, show or change the tier new VMs get, allowlist unless you change it, which is never learn, because each VM learns on its own.",
		Example: examples(
			"devvm network", "show the VM's network tier and allowlist",
			"devvm network allow example.com", "let the VM reach example.com",
			"devvm network log --blocked", "list what the VM's proxy blocked",
			"devvm network learn", "let the VM reach any public host while it learns the ones it needs",
			"devvm network lock", "choose which of them it keeps, and put it on the allowlist tier",
			"devvm network -g none", "give every new VM the none tier",
		),
		Args: showsOnly(func(cmd *cobra.Command, args []string) error {
			if err := maxArgs(1)(cmd, args); err != nil || len(args) == 0 {
				return err
			}
			if !slices.Contains(choices(), args[0]) {
				return msg.ArgNotChoice(strings.Join(choices(), ", "), cmd.CommandPath(), args[0])
			}
			// Each VM learns on its own: no default makes new VMs learn.
			if g, _ := cmd.Flags().GetBool("global"); g && args[0] == netpolicy.Learn {
				return msg.NetLearnNotGlobal()
			}
			return nil
		}),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return choices(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			what := msg.NetworkFailed
			if global {
				what = msg.NetDefaultFailed
			}
			var (
				mode  settings.Value
				label string
				shown vm.NetworkShown
			)
			err := a.env(func(env *vm.Env) error {
				n := vm.Net{Env: env}
				var err error
				switch {
				case global && len(args) == 0:
					mode, err = n.NetworkDefault()
					label = mode.Source.Label(env.Dirs)
				case global:
					err = n.SetNetworkDefault(args[0])
				case len(args) == 0:
					shown, err = n.Network(cmd.Context())
				case args[0] == netpolicy.Learn:
					err = n.Learn(cmd.Context(), "devvm network "+args[0])
				default:
					err = n.SetTier(cmd.Context(), netpolicy.Tier(args[0]), "devvm network "+args[0])
				}
				return err
			}, what)
			switch {
			case err != nil || len(args) == 1:
				return err
			case global:
				return printDefault(u, asJSON, "policy", mode, label)
			}
			return printNetwork(u, asJSON, shown)
		},
	}
	cmd.Flags().BoolVarP(&global, "global", "g", false, "show or change new VMs' network tier instead")
	jsonFlag(cmd, &asJSON)
	cmd.AddCommand(newNetworkRuleCmds(a)...)
	cmd.AddCommand(newNetworkLogCmd(a), newNetworkLockCmd(a), newNetworkTransparentCmd(a))
	return addFolderFlags(cmd, a)
}

// networkOutput is devvm network --json: the VM's tier (or unenforced),
// since when it learns the hosts it reaches when it does, its allowlist,
// each policy with rules in the order its proxy reads them, and its
// transparent mode.
type networkOutput struct {
	Policy        string           `json:"policy"`
	LearningSince string           `json:"learning_since,omitempty"`
	Allowlist     []policyFileJSON `json:"allowlist"`
	Transparent   transparentJSON  `json:"transparent"`
}

// policyFileJSON is a policy: a built-in one, by its name in devvm's
// files, or one of the user's files, by its path; whether only its
// denies apply (on the internet and none tiers), when its rules are its
// denies alone; its rules, and its lines that aren't rules, which its
// proxy skips.
type policyFileJSON struct {
	Builtin    string        `json:"builtin,omitempty"`
	File       string        `json:"file,omitempty"`
	DeniesOnly bool          `json:"denies_only,omitempty"`
	Rules      []string      `json:"rules"`
	NotRules   []notRuleJSON `json:"not_rules"`
	// Quiet are a built-in policy's quiet hosts (netpolicy.RuleFile.Quiet).
	Quiet []string `json:"quiet,omitempty"`
}

// notRuleJSON is a policy file's line that isn't a rule: its number, from
// 1, and its text, comment left out.
type notRuleJSON struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// printNetwork writes the VM's tier: its line then its rules,
// or with asJSON as networkOutput.
func printNetwork(u *ui.UI, asJSON bool, s vm.NetworkShown) error {
	if !asJSON {
		if _, err := io.WriteString(u.Stdout(), netpolicy.Shown(s.Policy, !s.Learning.IsZero())+"\n"); err != nil {
			return err
		}
		return netpolicy.WriteRules(u.Stdout(), s.Rules)
	}
	out := networkOutput{Policy: s.Policy, LearningSince: jsonTime(s.Learning), Allowlist: []policyFileJSON{}, Transparent: transparentJSON(s.Transparent)}
	for _, f := range s.Rules {
		j := policyFileJSON{DeniesOnly: f.DeniesOnly, Rules: append([]string{}, f.Rules()...), NotRules: []notRuleJSON{}, Quiet: f.Quiet()}
		if f.Builtin {
			j.Builtin = f.Path
		} else {
			j.File = f.Path
		}
		for _, l := range f.NotRules() {
			j.NotRules = append(j.NotRules, notRuleJSON{Line: l.N, Text: l.Text})
		}
		out.Allowlist = append(out.Allowlist, j)
	}
	return printJSON(u, out)
}

// newNetworkRuleCmds are devvm network allow, deny and rm RULE... [-g].
func newNetworkRuleCmds(a *app) []*cobra.Command {
	sub := func(use, short, long, example string, deny, remove bool) *cobra.Command {
		c := vm.RuleChange{Deny: deny, Remove: remove}
		cmd := &cobra.Command{
			Use:         use + " RULE...",
			Annotations: about{docs: "safety/network#changing-rules", noJSON: "it changes a setting"}.annotations(),
			Short:       short,
			Long:        long,
			Example:     example,
			Args: cobra.MatchAll(minArgs(1), func(_ *cobra.Command, args []string) error {
				if remove && c.Methods == "" && c.Path == "" {
					return nil // as written, a deny with its !
				}
				for _, r := range args {
					if _, err := netpolicy.Rule(r, deny || remove && strings.HasPrefix(r, "!"), c.Methods, c.Path); err != nil {
						return err
					}
				}
				return nil
			}),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.env(func(env *vm.Env) error {
					return vm.Net{Env: env}.PolicyChange(cmd.Context(), args, c)
				}, msg.PolicyFailed)
			},
		}
		f := cmd.Flags()
		f.BoolVarP(&c.Global, "global", "g", false, "change your rules for every VM instead")
		f.StringVar(&c.Methods, "method", "", "narrow each rule to these `METHODS` of a host's requests, such as GET,HEAD")
		f.StringVar(&c.Path, "path", "", "narrow each rule to the requests whose path matches `PATH`, such as /repos/ for everything below /repos/")
		return addFolderFlags(cmd, a)
	}
	rules := " RULE is a host, *.host for its subdomains, ** for any host, or an address or CIDR, " +
		"and with a port, such as example.com:8443, it matches only that port instead of 443 and 80."
	narrow := " --method and --path narrow a host's rule to some of its requests, which its proxy decrypts to check."
	return []*cobra.Command{
		sub("allow", "Allow hosts for this folder's VM", "Add RULEs to the allowlist of this folder's VM, "+
			"or with -g of every VM, from the next connection on."+rules+narrow,
			examples(
				"devvm network allow example.com", "let the VM reach example.com",
				"devvm network allow '*.example.com'", "and any of its subdomains",
				"devvm network allow 10.0.0.5:5432", "one port of one address",
				"devvm network allow api.example.com --method GET,HEAD", "let it only read from api.example.com",
				"devvm network allow -g pypi.org", "let every VM reach pypi.org",
			), false, false),
		sub("deny", "Deny hosts for this folder's VM", "Add RULEs as denies to this folder's VM's rules, "+
			"or with -g every VM's, from the next connection on, and a deny beats any allow."+rules+narrow,
			examples(
				"devvm network deny example.com", "keep the VM from reaching example.com",
				"devvm network deny api.github.com --method POST,PUT,PATCH,DELETE", "keep it from changing anything through GitHub's API",
				"devvm network deny github.com --path '/*/*/git-receive-pack'", "keep it from pushing to GitHub over HTTPS",
				"devvm network deny -g '*.example.com'", "keep every VM from its subdomains",
			), true, false),
		sub("rm", "Remove rules from this folder's VM", "Remove RULEs, as written with a deny's !, from "+
			"this folder's VM's own rules, or with -g yours for every VM, with --method and --path for a rule that narrows. "+
			"The change applies at once.",
			examples(
				"devvm network rm example.com", "remove an allow",
				"devvm network rm '!example.com'", "remove a deny",
				"devvm network rm api.example.com --method GET,HEAD", "remove an allow that narrows",
			), false, true),
	}
}

// newNetworkLogCmd is devvm network log [--follow] [-n N] [--blocked].
func newNetworkLogCmd(a *app) *cobra.Command {
	var lines uint
	var o vm.LogOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "log",
		Annotations: about{docs: "safety/network#logs"}.annotations(),
		Short:       "Show what this folder's VM connected to and what its proxy blocked",
		Long: "Show the network log of this folder's VM, oldest first: one line per connection " +
			"or request its proxy allowed or blocked, and one line per connection to one of its links " +
			"and each that devvm refused. " +
			"--blocked lists the blocks instead, one row per host and port, with the devvm network allow " +
			"command that allows each.",
		Example: examples(
			"devvm network log", "show the last 50 connections and requests",
			"devvm network log --follow", "keep showing new ones until Ctrl+C",
			"devvm network log --blocked", "list the blocks, with the command that allows each",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.Lines = int(min(lines, 1<<31-1))
			if asJSON {
				return printLogJSON(cmd, a, o)
			}
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.Log(cmd.Context(), a.u.Stdout(), o) }, msg.LogFailed)
		},
	}
	f := cmd.Flags()
	f.UintVarP(&lines, "lines", "n", 50, "show the last `N` connections and requests")
	f.BoolVar(&o.Follow, "follow", false, "keep showing new ones as they happen, until interrupted")
	f.BoolVar(&o.Blocked, "blocked", false, "list the blocks by host and port, with the command that allows each")
	jsonFlag(cmd, &asJSON)
	exclusive(cmd, "blocked", "follow")
	exclusive(cmd, "blocked", "lines")
	exclusive(cmd, "json", "follow")
	return addFolderFlags(cmd, a)
}

// logEventJSON is an event in devvm network log --json: what the VM's
// proxy logged of a connection or request, leaving out what it lacks.
type logEventJSON struct {
	Time     string   `json:"time"`
	Event    string   `json:"event"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Method   string   `json:"method,omitempty"`
	Path     string   `json:"path,omitempty"`
	Rule     string   `json:"rule,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Secrets  []string `json:"secrets,omitempty"`
	Refused  bool     `json:"refused,omitempty"`
	Upstream string   `json:"upstream,omitempty"`
	Target   string   `json:"target,omitempty"`
	Via      string   `json:"via,omitempty"`
	Quiet    bool     `json:"quiet,omitempty"`
}

// logOutput is devvm network log --json; blockedOutput, with --blocked.
type logOutput struct {
	Events []logEventJSON `json:"events"`
}

type blockJSON struct {
	Last  string `json:"last"`
	Count int    `json:"count"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Allow string `json:"allow,omitempty"`
	Quiet bool   `json:"quiet,omitempty"`
}

type blockedOutput struct {
	Blocked []blockJSON `json:"blocked"`
}

// printLogJSON writes the folder's VM's network log as logOutput, or with
// o.Blocked its blocks as blockedOutput.
func printLogJSON(cmd *cobra.Command, a *app, o vm.LogOptions) error {
	u := a.u
	var (
		events []netpolicy.Event
		blocks []netpolicy.Block
	)
	if err := a.env(func(env *vm.Env) error {
		var err error
		if o.Blocked {
			blocks, err = vm.Net{Env: env}.LogBlocks(cmd.Context())
		} else {
			events, err = vm.Net{Env: env}.LogEvents(cmd.Context(), o.Lines)
		}
		return err
	}, msg.LogFailed); err != nil {
		return err
	}
	if o.Blocked {
		out := blockedOutput{Blocked: make([]blockJSON, 0, len(blocks))}
		for _, b := range blocks {
			out.Blocked = append(out.Blocked, blockJSON(b))
		}
		return printJSON(u, out)
	}
	out := logOutput{Events: make([]logEventJSON, 0, len(events))}
	for _, e := range events {
		out.Events = append(out.Events, logEventJSON{
			Time: e.Time, Event: e.Event, Host: e.Host, Port: e.Port, Method: e.Method, Path: e.Path, Rule: e.Rule,
			Reason: e.Reason, Agent: e.Agent, Secrets: e.Secrets, Refused: e.Refused, Upstream: e.Upstream, Target: e.Target, Via: e.Via, Quiet: e.Quiet,
		})
	}
	return printJSON(u, out)
}

// newNetworkLockCmd is devvm network lock [--all] [--none] [--json].
func newNetworkLockCmd(a *app) *cobra.Command {
	var o vm.LockOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "lock",
		Annotations: about{docs: "safety/network#locking-it"}.annotations(),
		Short:       "Lock this folder's VM to the hosts it learned, on the allowlist tier",
		Long: "For this folder's VM, which learns the hosts it reaches, choose in a list which of the hosts it reached beyond its allowlist to add to its own rules, then give it the allowlist tier at once. " +
			"Only a host on port 443 of a package registry or vendor that devvm knows, such as registry.npmjs.org, starts ticked, and risky ones are listed apart, so read the list before you confirm it. " +
			"Without a terminal, --all adds every host and --none adds none, and --json lists the hosts and changes nothing.",
		Example: examples(
			"devvm network lock", "choose the hosts the VM keeps, then lock it to them",
			"devvm network lock --json", "list the hosts it learned, changing nothing",
			"devvm network lock --all", "keep every host it learned, without asking",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return printLockJSON(cmd, a)
			}
			return a.env(func(env *vm.Env) error { return vm.Net{Env: env}.Lock(cmd.Context(), o) }, msg.NetworkFailed)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.All, "all", false, "add every host the VM learned, without asking")
	f.BoolVar(&o.None, "none", false, "add none of the hosts the VM learned, without asking")
	jsonFlag(cmd, &asJSON)
	exclusive(cmd, "all", "none")
	exclusive(cmd, "json", "all")
	exclusive(cmd, "json", "none")
	return addFolderFlags(cmd, a)
}

// lockOutput is devvm network lock --json: since when the folder's VM
// learns, whether its log lost hosts since, and the hosts it reached that
// its allowlist doesn't allow, the unrisky first, each by its rule.
type lockOutput struct {
	LearningSince string        `json:"learning_since"`
	Partial       bool          `json:"partial,omitempty"`
	Hosts         []learnedJSON `json:"hosts"`
}

// learnedJSON is a host a VM learned: the rule devvm network lock adds
// for it, its host and port, how many connections it had, when it was
// last reached, the User-Agent's tool that reached it most, whether it's
// one of the reviewed hosts that start ticked (netpolicy.Tickable), why
// it's risky (netpolicy's Risk), if it is, and via, dns when only tools
// without proxy settings reached it (Learned.NoProxy).
type learnedJSON struct {
	Rule     string `json:"rule"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Count    int    `json:"count"`
	Last     string `json:"last"`
	Agent    string `json:"agent,omitempty"`
	Tickable bool   `json:"tickable,omitempty"`
	Risk     string `json:"risk,omitempty"`
	Via      string `json:"via,omitempty"`
}

// printLockJSON writes what the folder's VM learned as lockOutput.
func printLockJSON(cmd *cobra.Command, a *app) error {
	var l netpolicy.LearnedHosts
	if err := a.env(func(env *vm.Env) error {
		var err error
		l, err = vm.Net{Env: env}.Learned(cmd.Context())
		return err
	}, msg.NetworkFailed); err != nil {
		return err
	}
	return printJSON(a.u, lockJSON(l))
}

func lockJSON(l netpolicy.LearnedHosts) lockOutput {
	out := lockOutput{LearningSince: jsonTime(l.Since), Partial: l.Partial, Hosts: make([]learnedJSON, 0, len(l.Hosts))}
	for _, h := range l.Hosts {
		j := learnedJSON{
			Rule: h.Rule, Host: h.Host, Port: h.Port, Count: h.Count, Last: h.Last, Agent: h.Agent, Tickable: h.Tickable, Risk: string(h.Risk),
		}
		if h.NoProxy {
			j.Via = proxy.ViaDNS
		}
		out.Hosts = append(out.Hosts, j)
	}
	return out
}
