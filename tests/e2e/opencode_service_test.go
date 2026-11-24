//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This machine's opencode service password and remote route, planted in
// the agent store's service.json as an earlier devvm agent import left it,
// which no VM may get.
const (
	opencodeHostPassword = "e2e-host-opencode-service-password" //nolint:gosec // a test's fake password, planted to be found missing
	opencodeHostRoute    = "e2ehostroute0001"
)

// plantOpencodeService writes opencode's service.json into the sandbox's
// agent store, as opencode 2.4.1 writes it on this machine with remote
// access on, and returns its path, and the error of writing it.
func plantOpencodeService(s *S) (string, error) {
	p := filepath.Join(filepath.Dir(s.Paths.Layers), "agents", "opencode", ".config", "opencode", "service.json")
	data := `{"password": "` + opencodeHostPassword + `", "remote": {"route": "` + opencodeHostRoute + `"}, "remoteAccepted": true}`
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return p, err
	}
	return p, os.WriteFile(p, []byte(data), 0o600)
}

// netOpencodeService: the start of vm, an opencode VM in the folder oc,
// takes the store's opencode service settings (stored) out, saying so in
// its launch's output (lr); no VM gets this machine's password or route;
// each VM's opencode makes a service password of its own; and a launch's
// setup takes out the copy with devvm-redacted that an earlier devvm gave
// the VM. planted is the error of planting stored.
func netOpencodeService(n *netRun, oc, vm, stored string, planted error, lr Res) {
	s := n.s
	s.Check("an opencode VM's start takes this machine's opencode service settings out of the agent store, and says so", func() error {
		if planted != nil {
			return failed("planting "+stored, planted)
		}
		if _, err := os.Lstat(stored); !errors.Is(err, os.ErrNotExist) {
			return failed("the store still has "+stored, err)
		}
		if !strings.Contains(lr.Out, "agent_store_own_cleaned") {
			return errors.New("no warning names it:\n" + lr.Out)
		}
		return nil
	})
	s.Check("no VM gets this machine's opencode service password or remote route", func() error {
		for _, v := range []string{vm, n.vm} {
			has, err := vmHas(s, v, false, "cat ~/.config/opencode/service*.json 2>/dev/null; true", opencodeHostPassword, opencodeHostRoute)
			if v == "" || err != nil || has {
				return failed("VM "+v+"'s opencode service settings hold this machine's", err)
			}
		}
		return nil
	})
	own := "opencode service get password"
	s.Check("each VM's opencode service has a password of its own", func() error {
		a, b := vmOut(s, vm, own), vmOut(s, n.vm, own)
		for _, p := range []string{a, b} {
			if p == "" || strings.ContainsAny(p, " \n") || p == "devvm-redacted" || p == opencodeHostPassword {
				return fmt.Errorf("a VM's password is %q", p)
			}
		}
		if a == b {
			return errors.New("two VMs have the same opencode service password")
		}
		return nil
	})
	s.Check("a launch takes out the copy with devvm-redacted that an earlier devvm gave the VM, so its opencode makes its own", func() error {
		old := `{"password": "devvm-redacted", "remote": {"route": "` + opencodeHostRoute + `"}}`
		if vm == "" || inVM(s, vm, "printf '%s' '"+old+"' > ~/.config/opencode/service.json") != nil {
			return errors.New("planting devvm's old copy in the VM failed")
		}
		if _, _, err := launchWith(s, oc, n.home()); err != nil {
			return err
		}
		if has, err := vmHas(s, vm, false, "cat ~/.config/opencode/service.json 2>/dev/null; true", "devvm-redacted", opencodeHostRoute); err != nil || has {
			return failed("the old copy is still in the VM", err)
		}
		if p := vmOut(s, vm, own); p == "" || p == "devvm-redacted" {
			return fmt.Errorf("the VM's password is %q", p)
		}
		return nil
	})
}
