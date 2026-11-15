package netpolicy

import (
	"context"
	"errors"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Results of the self-check, in the VM's state.
const (
	CheckOK         = "ok"
	CheckUnenforced = "unenforced"
)

// directArgv connects from the VM straight to an address on the internet,
// past its proxy settings: it must fail.
var directArgv = []string{"curl", "--noproxy", "*", "-sS", "-m", "3", "-o", "/dev/null", "https://1.1.1.1"}

// proxyArgv asks the VM's proxy for its blocks, through the proxy its
// guest environment names (HTTPS_PROXY, from /etc/environment, which a
// command run in the VM may not have read): it fails when the VM has no
// proxy set, or its proxy doesn't answer it. The password stays in the
// VM: the command names the variable, not its value.
var proxyArgv = []string{"sh", "-c", `set -a; . /etc/environment 2>/dev/null; set +a; ` +
	`[ -n "${HTTPS_PROXY:-}" ] && curl -sSf -m 3 -o /dev/null --proxy "$HTTPS_PROXY" http://devvm.proxy/blocked`}

// SelfCheck checks, after a VM starts, that its tier holds. An open VM's
// proxy is set in the VM and answers it (checkOpen). On a confined tier,
// it checks that its policy is enforced: on the host, that the VM runs on the process its backend
// started confined (Confined), then that a direct connection from the VM
// fails (the VM answers that one, so it only catches the honest case). The
// result goes in the VM's state.
//
// A VM devvm started confined that still gets out has another way out (a
// network devvm didn't set up), which a restart wouldn't change. One
// started some other way, or before it left the open tier, is
// offered a restart, confined (restart starts
// it again); where devvm can't ask, or if it's still unenforced after the
// restart, devvm stops. Either way the VM is stopped first, so it isn't
// left reaching the internet. A restart declined is warned about, and the
// VM is used as it is.
func (p *Proxy) SelfCheck(ctx context.Context, b backend.Backend, st *state.VM, restart func(context.Context) error) error {
	vm := st.Name()
	if !TierOf(st).Confined() {
		p.checkOpen(ctx, b, vm)
		return nil
	}
	ok, err := enforced(ctx, b, vm)
	if err != nil {
		return err
	}
	if ok {
		return setCheck(ctx, st, CheckOK)
	}
	if err := setCheck(ctx, st, CheckUnenforced); err != nil {
		return err
	}
	run, err := b.Running(ctx, vm)
	if err != nil {
		return err
	}
	if run.Confined {
		return p.stop(ctx, b, vm, msg.NetDirectRoute(vm))
	}
	yes, err := p.Confirm(msg.NetUnenforcedRestart(vm).AnsweredBy("devvm restart"))
	if err != nil {
		return p.stop(ctx, b, vm, msg.NetUnenforcedStop(vm))
	}
	if !yes {
		p.Warn(msg.NetUnenforced(vm))
		return nil
	}
	if err := b.Stop(ctx, vm); err != nil {
		return err
	}
	if err := restart(ctx); err != nil {
		return err
	}
	if ok, err := enforced(ctx, b, vm); err != nil {
		return err
	} else if !ok {
		return p.stop(ctx, b, vm, msg.NetUnenforcedStop(vm))
	}
	return setCheck(ctx, st, CheckOK)
}

// checkOpen is an open VM's self-check: its proxy is set in the VM and
// answers it (proxyArgv). An open VM isn't confined, so one whose proxy
// doesn't answer it reaches what it reaches all the same, but its tools
// that use the proxy fail: a warning, and the VM is used as it is.
func (p *Proxy) checkOpen(ctx context.Context, b backend.Backend, vm vmname.VM) {
	if err := b.Exec(ctx, vm, proxyArgv, backend.ExecOpts{}); err != nil && ctx.Err() == nil {
		p.Warn(msg.NetProxyUnreachable(vm))
	}
}

// setCheck records the self-check's result in the VM's state.
func setCheck(ctx context.Context, st *state.VM, result string) error {
	return st.Update(ctx, func(st *state.VM) error {
		n, err := Of(st)
		if err != nil {
			return err
		}
		n.Made.Check = result
		return Set(st, n)
	})
}

// Listed is a VM's tier as devvm ls shows it: its name, or unenforced: a
// confined tier, but the last self-check failed, or the VM runs but not
// confined (started some other way since). A stopped VM starts confined
// next time, unless that self-check failed. "" when its creation didn't
// finish.
func Listed(ctx context.Context, b backend.Backend, st *state.VM, running bool) (string, error) {
	n, err := Of(st)
	switch {
	case err != nil:
		return "", err
	case !st.Finished():
		return "", nil
	case !n.Tier.Confined():
		return string(n.Tier), nil
	case n.Made.Check == CheckUnenforced:
		return CheckUnenforced, nil
	case !running:
		return string(n.Tier), nil
	}
	run, err := b.Running(ctx, st.Name())
	if err != nil {
		return "", err
	}
	if !run.Confined {
		return CheckUnenforced, nil
	}
	return string(n.Tier), nil
}

// enforced reports whether the VM runs confined and can't connect out
// directly.
func enforced(ctx context.Context, b backend.Backend, vm vmname.VM) (bool, error) {
	run, err := b.Running(ctx, vm)
	if err != nil || !run.Confined {
		return false, err
	}
	return !Direct(ctx, b, vm), nil
}

// Direct reports whether the VM reaches the internet without its proxy (it
// shouldn't). A VM that doesn't answer doesn't.
func Direct(ctx context.Context, b backend.Backend, vm vmname.VM) bool {
	return b.Exec(ctx, vm, directArgv, backend.ExecOpts{}) == nil
}

// Unproxied stops a running VM whose proxy couldn't start (err): a VM never
// runs on without its proxy. It warns that it stopped
// it and returns err; if the stop fails, the stop's failure, after warning
// err. The stop isn't cancelled with ctx: an interrupt never leaves the VM
// running.
func (p *Proxy) Unproxied(ctx context.Context, b backend.Backend, vm vmname.VM, err error) error {
	if serr := b.Stop(context.WithoutCancel(ctx), vm); serr != nil {
		var m msg.M
		if errors.As(err, &m) {
			p.Warn(m)
		}
		return serr
	}
	p.Warn(msg.NetProxyDownStopped(vm))
	return err
}

// stop stops the VM, whose network tier doesn't hold, and returns why:
// m, or the stop's failure after warning m.
func (p *Proxy) stop(ctx context.Context, b backend.Backend, vm vmname.VM, m msg.M) error {
	if err := b.Stop(ctx, vm); err != nil {
		p.Warn(m)
		return err
	}
	return m
}
