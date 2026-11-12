package vm

// opencode's own code in internal/vm: opencode 2 keeps its logins in its
// database, which reads its auth.json only as it's made, so devvm imports
// the store's logins into it with opencodeScript.

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"strings"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// opencodeScript is devvm's guest script that gives opencode 2 in the VM
// the store's opencode logins that changed, run by every launch's setup
// too (guest/sync.sh).
const opencodeScript = "opencode.sh"

// importOutMax is the most of opencodeScript's output devvm agent login
// reads: a few lines a provider.
const importOutMax = 1 << 20

// opencodeKeepsDB reports whether the agent keeps its logins in a database
// of its own, which devvm agent import reads with its auth export:
// opencode, from 2 on.
func opencodeKeepsDB(agent string) bool { return agent == agents.Opencode }

// opencodeSyncImport adds to a launch's setup the store's opencode logins
// as opencode auth import reads them, opencode-import.json, when kind is
// opencode's login and vmCopy the VM's copy of it.
func opencodeSyncImport(s *stage, kind string, vmCopy []byte) error {
	if kind != agents.Opencode {
		return nil
	}
	imp, err := agentlogin.OpencodeImport(vmCopy)
	if err != nil {
		return err
	}
	s.put("opencode-import.json", 0o600, imp)
	return nil
}

// importLogins gives the VM's opencode, which keeps its logins in its
// database, the logins of vmCopy, the VM's copy of the store's
// (agentlogin.OpencodeImport), when the agent is opencode: with
// opencodeScript in a login shell (opencode on PATH), the logins on its
// input. devvm reads at most importOutMax bytes of what it prints, and
// stops it past that.
func (e AgentLogin) importLogins(ctx context.Context, t *target, agent string, vmCopy []byte) error {
	if agent != agents.Opencode {
		return nil
	}
	data, err := agentlogin.OpencodeImport(vmCopy)
	if err != nil {
		return msg.LoginCopyFailed(agents.Opencode, err.Error(), t.f.Name).Because(err)
	}
	script, err := fs.ReadFile(e.Assets, "guest/"+opencodeScript)
	if err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	log := e.vmLog()
	out := &bounded{max: importOutMax, cancel: cancel}
	argv := []string{"bash", "-lc", string(script), opencodeScript}
	err = t.exec(run, argv, backend.ExecOpts{Stdin: bytes.NewReader(data), Stdout: io.MultiWriter(out, log), Stderr: log})
	switch {
	case out.over:
		return msg.OpencodeImportTooMuch(t.f.Name)
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	if failed := opencodeRefused(out.String()); len(failed) > 0 {
		return backend.Logged(msg.OpencodeImportFailed(strings.Join(failed, ", "), t.f.Name))
	}
	return backend.Logged(msg.AgentKeyCopyFailed(agents.Opencode, t.f.Name))
}

// opencodeRefused is the providers opencodeScript's output names as
// refused by opencode's import, its "devvm-opencode-failed PROVIDER" lines.
func opencodeRefused(out string) []string {
	var failed []string
	for line := range strings.Lines(out) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "devvm-opencode-failed "); ok && p != "" {
			failed = append(failed, msg.Printable(p))
		}
	}
	return failed
}
