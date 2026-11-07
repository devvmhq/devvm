package vm

import (
	"context"
	"os"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
)

// Settings shows and changes the folder's VM's settings: its .git
// protection (here), its size (size.go), its ports (ports.go) and its SSH
// agent forwarding (sshagent.go).
type Settings struct{ *Env }

// GitProtect returns the folder's VM's .git protection: on, strict, off,
// or clone for a --clone VM, which mounts no .git (gitprotect.Mode, as
// devvm ls shows it).
func (e Settings) GitProtect(ctx context.Context) (string, error) {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMGitProtect})
	if err != nil {
		return "", err
	}
	return gitprotect.Mode(t.st), nil
}

// Clone returns whether the folder's VM has its own clone of the repo,
// CloneOn, or mounts the folder, CloneOff (devvm clone).
func (e Settings) Clone(ctx context.Context) (string, error) {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return "", err
	}
	if isClone(t.st) {
		return CloneOn, nil
	}
	return CloneOff, nil
}

// SetGitProtect turns the folder's VM's .git protection on, strict or
// off. Locks change only while the VM is stopped, so nothing in it can
// race them: a running VM's choice is saved, and set when devvm next
// finds it stopped (starter.hostChecks), which a restart the user agrees to does
// now (restartOr). On and strict lock .git as it is then.
func (e Settings) SetGitProtect(ctx context.Context, mode string) error {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMGitProtect})
	if err != nil {
		return err
	}
	vm := t.f.Name
	if isClone(t.st) {
		return msg.GitProtectClone(vm)
	}
	p := e.Protector(ctx, t.f.Dir)
	if mode == gitprotect.Off && gitprotect.Mode(t.st) != gitprotect.Off {
		if ok, err := t.widen("devvm git-protect off"); !ok {
			return err
		}
	}
	// Elsewhere there are no locks to race. It's read again, as it may
	// have started while widen asked.
	if p.CanLock {
		if err := t.refresh(ctx); err != nil {
			return err
		}
		if t.in.Status == backend.StatusRunning {
			if err := gitprotect.SetNext(ctx, t.st, mode); err != nil {
				return err
			}
			return e.restartOr(ctx, vm, e.Info, msg.GitProtectNextStop(mode, vm))
		}
	}
	return setGitProtect(ctx, e.Env, p, t, mode)
}

// setGitProtect sets a stopped VM's .git protection, and says what it
// did: on a host without locks, off quiets the launches' warning.
func setGitProtect(ctx context.Context, e *Env, p *gitprotect.Protector, t *target, mode string) error {
	vm := t.f.Name
	if err := p.Set(ctx, t.st, mode); err != nil {
		return err
	}
	switch _, err := os.Lstat(filepath.Join(t.f.Dir, ".git")); {
	case mode == gitprotect.Off && !p.CanLock:
		e.Info(msg.GitUnprotectedQuiet(vm))
	case mode == gitprotect.Off:
		e.Info(msg.GitUnlocked(vm))
	case err != nil:
		e.Info(msg.GitLockLater(vm))
	case mode == gitprotect.Strict:
		e.Info(msg.GitLockedStrict(vm))
	default:
		e.Info(msg.GitLocked(vm))
	}
	return nil
}
