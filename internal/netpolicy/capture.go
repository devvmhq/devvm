package netpolicy

import (
	"context"
	"errors"
	"slices"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// Capturer is a VM's proxy's capture of its agents' logins
// (proxy.IO.Capture), in devvm _proxy's own process: a token request and
// its reply go to devvm's store of the agent's logins (secrets.Store's
// Capture), with the VM's placeholders, and once a login is saved, the
// VM's bindings are written again, so its proxy puts the login in at once.
type Capturer struct {
	Dirs    paths.Dirs
	Secrets *secrets.Store
}

// vm is the VM a capture is for, by its proxy's name for it, its
// placeholders' salt, and the agent the exchange is for: of the agents
// whose endpoint it went to, one the VM's image has (agentlogin.Pick among
// them). A capture replaces the login every VM with the agent uses, so a VM
// whose image has none of them captures nothing (login_capture_refused).
func (c Capturer) vm(x proxy.Exchange) (*state.VM, string, string, error) {
	st, err := state.ReadVM(c.Dirs, x.VM)
	if err != nil {
		return nil, "", "", err
	}
	n, err := Of(st)
	if err != nil {
		return nil, "", "", err
	}
	if !st.Exists() || n.Made.ProxySalt == "" {
		return nil, "", "", errors.New("the VM has no placeholders")
	}
	own := image.AgentsOf(tools.Names(st))
	mine := slices.DeleteFunc(slices.Clone(x.Agents), func(a string) bool { return !slices.Contains(own, a) })
	if len(mine) == 0 {
		return nil, "", "", msg.LoginCaptureRefused(agentlogin.Pick(x.Agents, x.UserAgent), st.Name())
	}
	return st, n.Made.ProxySalt, agentlogin.Pick(mine, x.UserAgent), nil
}

// Request is the token request's body as it goes on, or the reply the VM
// gets instead.
func (c Capturer) Request(_ context.Context, x proxy.Exchange) (out, answer []byte, err error) {
	_, salt, agent, err := c.vm(x)
	if err != nil {
		return nil, nil, err
	}
	return c.Secrets.CaptureRequest(agent, salt, x.ContentType, x.Body)
}

// Response saves the login in reply and returns what the VM gets, then
// writes the VM's bindings again with it: when that fails, the login is
// saved still, and the error comes with the VM's reply.
func (c Capturer) Response(ctx context.Context, x proxy.Exchange, reply []byte) ([]byte, string, error) {
	st, salt, agent, err := c.vm(x)
	if err != nil {
		return nil, "", err
	}
	out, err := c.Secrets.Capture(ctx, agent, salt, x.ContentType, x.Body, reply)
	if err != nil {
		return nil, "", err
	}
	return out, agent, c.Secrets.WriteFiles(ctx, st, salt)
}
