package cli

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/packaging"
)

// upgradeOpts are devvm upgrade's flags.
type upgradeOpts struct {
	upgrade.Options
	check, asJSON bool
}

// newUpgradeCmd is devvm upgrade.
func newUpgradeCmd(a *app) *cobra.Command {
	var o upgradeOpts
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install devvm's newest release",
		Long: "Install devvm's newest release the way this devvm was installed, after asking. " +
			"The install script's devvm gets the release once its signature and checksum check out, and Homebrew's runs brew upgrade devvm. " +
			"--check only says whether a newer release is out.",
		Example: examples(
			"devvm upgrade", "install the newest release, asking first",
			"devvm upgrade --check", "say whether a newer release is out",
			"devvm upgrade --force", "install it without asking, as a script must",
			"devvm upgrade --to 0.1.0", "install that release, even an older one",
			"devvm upgrade --to 0.2.0-rc.1", "install a prerelease, which only --to installs",
		),
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			up, err := a.upgrader()
			if err != nil {
				return failed(err, msg.UpgradeFailedReason)
			}
			if o.check || o.asJSON {
				return failed(showUpgrade(cmd.Context(), a, up, o.asJSON), msg.UpgradeFailedReason)
			}
			o.Version = strings.TrimPrefix(o.Version, "v")
			return failed(up.Upgrade(cmd.Context(), o.Options), msg.UpgradeFailedReason)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.check, "check", false, "say whether a newer release is out, and install nothing")
	f.StringVar(&o.Version, "to", "", "install the release `VERSION` instead of the newest, even an older one or a prerelease")
	addForceFlag(f, &o.Force)
	jsonFlag(cmd, &o.asJSON)
	exclusive(cmd, "check", "force")
	exclusive(cmd, "json", "force")
	exclusive(cmd, "check", "to")
	exclusive(cmd, "json", "to")
	return cmd
}

// upgrader is devvm upgrade's: this devvm, as it was installed, and the
// release source, GitHub's releases of devvm checked with the release
// key built into devvm, or a test build's stand-ins.
func (a *app) upgrader() (upgrade.Upgrader, error) {
	o := a.testOverrides().Release
	src := upgrade.NewSource(o.URL, cmp.Or(o.Key, packaging.MinisignKey()))
	if o.CA != "" {
		pem, err := os.ReadFile(o.CA)
		if err != nil {
			return upgrade.Upgrader{}, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return upgrade.Upgrader{}, errors.New(o.CA + " has no certificate")
		}
		src.Client.Trust(roots)
	}
	exe, err := os.Executable()
	if err != nil {
		return upgrade.Upgrader{}, err
	}
	return upgrade.Upgrader{
		Reporter:   a.u,
		Source:     src,
		Install:    upgrade.Detect(exe),
		Current:    cmp.Or(o.Version, devvmVersion()),
		FromSource: version == "" && o.Version == "",
	}, nil
}

// upgradeJSON is devvm upgrade --json: this devvm's version, how and
// where it was installed, the newest release's version, and whether
// that's newer.
type upgradeJSON struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Newer   bool   `json:"newer"`
	Install string `json:"install"`
	Path    string `json:"path"`
}

// showUpgrade says what devvm upgrade would install, as JSON with
// asJSON, and installs nothing.
func showUpgrade(ctx context.Context, a *app, up upgrade.Upgrader, asJSON bool) error {
	u := a.u
	done := u.Status(msg.UpgradeChecking())
	av, err := up.Source.Check(ctx, up.Current)
	done()
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(u, upgradeJSON{
			Current: av.Current, Latest: av.Latest, Newer: av.Newer,
			Install: string(up.Install.Kind), Path: up.Install.Exe,
		})
	}
	switch {
	case up.Install.Kind == upgrade.Other || !upgrade.Valid(up.Current):
		u.Info(msg.UpgradeLatestIs(av.Latest))
		u.Info(up.NotInstalled())
	case av.Newer:
		u.Info(msg.UpgradeAvailable(av.Current, av.Latest))
	case av.Latest == av.Current:
		u.Info(msg.UpgradeAlreadyLatest(av.Current))
	case upgrade.Prerelease(av.Current):
		u.Info(msg.UpgradePrereleaseAhead(av.Current, av.Latest))
	default:
		u.Info(msg.UpgradeLatestOlder(av.Current, av.Latest))
	}
	return nil
}
