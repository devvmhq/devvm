package cli

import (
	"runtime"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/seatbelt"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vmlog"
)

// newProxyCmd is devvm _proxy: one VM's proxy, run with its config
// (-config FILE): the broker, which starts its child, devvm _proxy-child,
// and answers it. Hidden: it's not for users. Its flag is the proxy's own
// (one dash or two), parsed by proxy.Main; its event log and log lines
// are data, written to devvm's stdout and stderr, which its child gets as
// its own. Its secrets' values come from devvm's store, looked up in this
// process, and the logins it captures go there too. It holds the logs the
// VM's backend writes as the VM runs to their cap (vmlog.KeepVM). A signal
// waits for it to stop its child.
func newProxyCmd(a *app) *cobra.Command {
	u := a.u
	return &cobra.Command{
		Use:                "_proxy [flags]",
		Short:              "Run a VM's proxy",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			u.StopsItsOwn() // a signal waits for the broker to stop its child
			store, err := a.secrets()
			var d paths.Dirs
			if err == nil {
				d, err = a.dirs()
			}
			if err == nil {
				out, errOut := ui.Files(u)
				std := proxy.IO{
					Stdout: u.Stdout(), Stderr: u.Stderr(), OutFile: out, ErrFile: errOut,
					FS:      assets.FS, // the built-in policies, read from the binary
					Secret:  store.Get,
					Capture: netpolicy.Capturer{Dirs: d, Secrets: store},
					// The VM's backend's logs, held to their cap while the VM runs.
					KeepLogs: vmlog.KeepVM,
					// Names the broker in run/proxy.confined, which devvm doctor reads.
					Started: platform.ProcessStart,
				}
				// On macOS the child starts under Seatbelt, confined from its first instruction.
				if runtime.GOOS == "darwin" {
					std.Seatbelt = seatbelt.Sandbox{Exec: seatbelt.Exec}.ProxyChild
				}
				err = proxy.Main(cmd.Context(), args, std)
			}
			if err != nil {
				return msg.ProxyFailed(err.Error()).Because(err)
			}
			return nil
		},
	}
}

// newProxyChildCmd is devvm _proxy-child: the child of a VM's proxy's
// broker, which reads the VM's network (-for FILE, the broker's config,
// which it never opens, only so it can be told apart). Hidden: only the
// broker starts it, with its socket and listener. Its built-in policies
// come from devvm's own files in the binary; it looks up nothing itself.
func newProxyChildCmd(a *app) *cobra.Command {
	u := a.u
	return &cobra.Command{
		Use:                proxy.ChildCommand + " [flags]",
		Short:              "Run a VM's proxy's child",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			out, errOut := ui.Files(u)
			err := proxy.ChildMain(args, proxy.IO{
				Stdout: u.Stdout(), Stderr: u.Stderr(), OutFile: out, ErrFile: errOut,
				FS: assets.FS, // the built-in policies, read from the binary
			})
			if err != nil {
				return msg.ProxyFailed(err.Error()).Because(err)
			}
			return nil
		},
	}
}
