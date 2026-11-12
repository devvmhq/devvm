package cli

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ui"
)

// NewRoot returns the devvm command with every subcommand registered, each
// printing and asking through u, on the run's services (app). devvm
// itself is up: a shell in this folder's VM.
func NewRoot(u *ui.UI) *cobra.Command {
	a := newApp(u)
	uf := new(upFlags)
	fl := new(ui.Flags)
	root := &cobra.Command{
		Use:         "devvm",
		Annotations: about{docs: "getting-started/commands", noJSON: "it opens a shell"}.annotations(),
		Short:       "Open a shell in this folder's VM, creating it first",
		Long: "Open a shell in this folder's VM, at the same path, and create the VM first when the folder has none. " +
			"devvm shows what a new VM gets, and in a terminal you can customize its toolchains, coding agents, layers, tier, ports and size, which --tools, --agents, --layers, --network, --ports, --cpus, --memory and --disk choose without asking. " +
			"The VM mounts only this folder read-write, and with --clone it gets its own clone of the repo instead.",
		Example: examples(
			"devvm", "open this folder's VM, choosing what a new one gets",
			"devvm --agents claude", "create the VM with Claude Code and the toolchains the folder needs",
			"devvm --tools node --agents none --layers docker", "create the VM with node and the docker layer alone",
			"devvm --clone", "create the VM with its own clone of this repo",
			"devvm ls", "list every VM",
		),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			f := *fl
			f.PipedAnswers, f.PickOnPipe = a.testOverrides().PipedAnswers, a.testOverrides().PickOnPipe
			u.SetFlags(f)
			if wantsJSON(cmd) {
				u.DataOnly() // stdout is the JSON's alone
			}
			makePrivate(a)
			if !skipsLayout(cmd) {
				if err := checkLayout(cmd.Context(), a); err != nil {
					return ranError{err}
				}
			}
			if !skipsRetention(cmd) {
				autoRetain(cmd.Context(), a)
			}
			return nil
		},
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return up(cmd.Context(), a, uf)
		},
		SuggestionsMinimumDistance: msg.SuggestDistance,
	}
	root.SetOut(u.Stdout())
	root.PersistentFlags().BoolVarP(&fl.Verbose, "verbose", "v", false, "show the output of the commands devvm runs instead of a status line")
	root.PersistentFlags().BoolVar(&fl.NoInput, "no-input", false, "never ask, and fail where devvm would ask, naming the flag that answers it")
	addUpFlags(root, a, uf)
	addVersion(root)
	root.AddCommand(newBuildCmd(a), newRebuildCmd(a), newImagesCmd(a), newGCCmd(a), newSpaceCmd(a), newLayerCmd(a), newSetupCmd(a), newAgentCmd(a), newMsgCmd(u),
		newRestartCmd(a), newStopCmd(a), newUpdateCmd(a), newToolsCmd(a), newAdoptCmd(a), newHistoryCmd(a), newRmCmd(a), newLsCmd(a), newNetworkCmd(a), newHostCmd(a), newCodeCmd(a), newDesktopCmd(a), newSSHAgentCmd(a), newGitProtectCmd(a), newCloneCmd(a), newPortsCmd(a), newSizeCmd(a), newMountCacheCmd(a), newDoctorCmd(a), newLimactlCmd(a), newQEMUCmd(u), newProxyCmd(a), newForwarderCmd(a), newRefreshToolsCmd(a), newSecretCmd(a),
		newLinkCmd(a), newGroupCmd(a), newLinksCmd(a), newRetentionCmd(a), newVersionCmd(u), newCompletionCmd(u), newUpgradeCmd(a), newPasteCmd(a))
	addCompletions(root, a)
	checkCommands(root)
	addHelp(root, u.Logo)
	addGroups(root)
	addDocs(root)
	return root
}

// makePrivate makes devvm's data and settings folders the user's alone on
// every run, whatever mode they had. Without a HOME there are no folders,
// and the command says so itself.
func makePrivate(a *app) {
	d, err := a.dirs()
	if err != nil {
		return
	}
	if err := state.MakePrivate(d); err != nil {
		a.u.Warn(msg.FoldersNotPrivate(d.Config, d.Data).Because(err))
	}
}

// checkLayout checks the layout version of devvm's folders before the
// command runs (state.CheckLayout), and fails when a newer devvm changed
// them or a version file can't be read. Without a HOME there are no
// folders, and the command says so itself.
func checkLayout(ctx context.Context, a *app) error {
	if d, err := a.dirs(); err == nil {
		return failed(state.CheckLayout(ctx, d), msg.LayoutUnchecked)
	}
	return nil
}

// failure is a command's failure and what failed, the message for an
// error that isn't one: what failed returns, which execute shows.
type failure struct {
	err  error
	what func(reason string) msg.M
}

func (f failure) Error() string { return f.err.Error() }
func (f failure) Unwrap() error { return f.err }

// failed is err as a command's failure, nil for nil, which its RunE
// returns: execute shows it as showFailure says, with what failed for an
// error that isn't a message.
func failed(err error, what func(reason string) msg.M) error {
	if err == nil {
		return nil
	}
	return failure{err, what}
}

// showFailure prints what a command's RunE returned: a question's
// refusal (msg.Refusal: the question, then what answers it), the run
// log's end and its message when the details are there (a
// backend.LoggedError), the message err is or carries, or else what
// failed (output_failed without failed's), err's text its reason. Once
// the run is interrupted it prints nothing, as the failure is the
// interrupt's.
func showFailure(u *ui.UI, err error) {
	if r, ok := errors.AsType[*msg.Refusal](err); ok {
		u.Refused(r)
		return
	}
	if e, ok := errors.AsType[*backend.LoggedError](err); ok {
		u.ErrorLogged(e.Msg)
		return
	}
	if m, ok := errors.AsType[msg.M](err); ok {
		u.Error(m)
		return
	}
	what := msg.OutputFailed
	if f, ok := errors.AsType[failure](err); ok {
		what, err = f.what, f.err
	}
	reason := err.Error()
	if c, ok := errors.AsType[*state.CorruptError](err); ok && c.Error() == reason {
		// A file devvm can't read, unwrapped: its message as the UI
		// shows messages, names in names mode.
		reason = c.Shown(u.Text)
	}
	u.Error(what(reason).Because(err))
}

// skipsLayout reports whether cmd runs without checking the layout
// version: what reads none of devvm's folders (help, completion, the
// version, the messages, devvm upgrade, which a layout a newer devvm
// wrote is a reason to run), and devvm's own hidden commands, which a
// devvm that checked it runs.
func skipsLayout(cmd *cobra.Command) bool {
	path := strings.Fields(cmd.CommandPath())
	if len(path) < 2 {
		return false
	}
	return strings.HasPrefix(path[1], "_") || slices.Contains([]string{"completion", "version", "upgrade", "help", "__complete", "__completeNoDesc"}, path[1])
}

// Execute runs devvm with args on u and returns its exit code: 0, 1 for a
// failure, usageCode for a usage error (a bad argument or flag, an
// unknown command), which is followed by usage_help, naming the
// command's own --help.
func Execute(ctx context.Context, u *ui.UI, args []string) int {
	return execute(ctx, u, NewRoot(u), args)
}

// execute runs root with args, as Execute does.
func execute(ctx context.Context, u *ui.UI, root *cobra.Command, args []string) (code int) {
	defer func() {
		if p := recover(); p != nil {
			w, ok := p.(wordPanic)
			if !ok {
				panic(p)
			}
			printUsage(u, root, w.err)
			code = usageCode
		}
	}()
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	if cmd == nil {
		cmd = root
	}
	if e, ok := errors.AsType[ranError](err); ok {
		showFailure(u, e.err)
		return 1
	}
	// Anything else is the parser's, before the command ran. A word that
	// isn't a command is devvm's, whichever command (help) refused it.
	e := usage(err)
	if strings.HasPrefix(e.m.ID(), "no_command") {
		cmd = root
	}
	printUsage(u, cmd, e)
	return usageCode
}
