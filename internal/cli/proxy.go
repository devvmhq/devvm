package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/vmlog"
)

// newProxyCmd is devvm _proxy: one VM's proxy, run with
// its config (-config FILE). Hidden: it's not for users. Its flag is the
// proxy's own (one dash or two), parsed by proxy.Main; its event log and
// log lines are data, written to devvm's stdout and stderr. Its built-in
// policies (its config's) come from devvm's own files in the binary, its
// secrets' values from devvm's store, looked up in this process, and the
// logins it captures go there too. It holds the logs the VM's backend
// writes as the VM runs to their cap (vmlog.KeepVM).
func newProxyCmd(a *app) *cobra.Command {
	u := a.u
	return &cobra.Command{
		Use:                "_proxy [flags]",
		Short:              "Run a VM's proxy",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := a.secrets()
			var d paths.Dirs
			if err == nil {
				d, err = a.dirs()
			}
			if err == nil {
				err = proxy.Main(args, proxy.IO{
					Stdout:  u.Stdout(),
					Stderr:  u.Stderr(),
					FS:      assets.FS, // the built-in policies, read from the binary
					Secret:  store.Get,
					Capture: netpolicy.Capturer{Dirs: d, Secrets: store},
					// The VM's backend's logs, held to their cap while the VM runs.
					KeepLogs: vmlog.KeepVM,
				})
			}
			if err != nil {
				return msg.ProxyFailed(err.Error()).Because(err)
			}
			return nil
		},
	}
}
