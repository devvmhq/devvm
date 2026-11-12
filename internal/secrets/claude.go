package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

// ClaudeToken is the secret that is the Claude Code token, named after
// its agent (agents.Def's Secret): claude.
const ClaudeToken = agents.Claude

// Claude is the Claude Code login devvm keeps, one of two: a Claude Code
// token, the claude secret (devvm secret set claude, or claude
// setup-token's, captured), or the subscription's OAuth login, captured
// into devvm's store (agents.ClaudeOAuth). The newest replaces the
// other (Store.Capture, Store.Save).
type Claude struct{ s *Store }

// ClaudeOf is the Claude Code login s keeps.
func ClaudeOf(s *Store) Claude { return Claude{s} }

// Env is the Claude Code token in devvm's environment
// (CLAUDE_CODE_OAUTH_TOKEN), which wins over the saved one, or "".
func (c Claude) Env() string { return c.s.Getenv("CLAUDE_CODE_OAUTH_TOKEN") }

// Login is the Claude Code login VMs get: "token", the claude secret's
// (Env, or the saved one), which wins as it does in Claude Code; "oauth",
// the OAuth login in devvm's store, when there's no token; or "" for
// none. A VM never gets both.
func (c Claude) Login(ctx context.Context) (string, error) {
	if _, err := c.s.Value(ctx, ClaudeToken); err == nil {
		return "token", nil
	} else if !errors.Is(err, ErrNone) {
		return "", err
	}
	if ok, err := c.s.login(agents.ClaudeOAuth, "").ClaudeLogin(); err != nil || !ok {
		return "", err
	}
	return "oauth", nil
}

// lock holds the lock of the Claude Code logins folder in devvm's store
// while one Claude Code login replaces the other, and returns the unlock.
func (c Claude) lock() (func(), error) {
	// The folder of its .claude.json: devvm's store of Claude Code's logins.
	dir := filepath.Dir(LoginFile(c.s.Dirs, agents.Claude))
	if err := state.PrivateDir(dir); err != nil {
		return nil, err
	}
	return agentlogin.LockFolder(dir)
}

// saveToken saves a Claude Code token and takes the OAuth login away,
// under lock.
func (c Claude) saveToken(ctx context.Context, value string) error {
	if err := c.s.save(ctx, ClaudeToken, value); err != nil {
		return err
	}
	return c.s.login(agents.ClaudeOAuth, "").Remove()
}

// capture is Store.Capture for Claude Code, ok false for another agent.
// devvm keeps one Claude Code login, so the newest replaces the other:
// claude setup-token's token is saved as the claude secret (Save, which
// takes the OAuth login away), and an OAuth login takes the saved token
// away, both under the Claude Code logins folder's lock (lock), so two at
// once never lose both. A saved token that can't be deleted is a warning,
// as the login is saved and the VM gets its reply.
func (c Claude) capture(ctx context.Context, agent, salt, ctype string, req, reply []byte) (vm []byte, ok bool, err error) {
	if agent != agents.Claude {
		return nil, false, nil
	}
	unlock, err := c.lock()
	if err != nil {
		return nil, true, err
	}
	defer unlock()
	if agentlogin.SetupToken(ctype, req) {
		sec, _ := Lookup(ClaudeToken)
		tok, vm, err := agentlogin.ClaudeToken(ctype, req, reply, sec.Placeholder(salt))
		if err != nil {
			return nil, true, err
		}
		if tok, err = Token(ClaudeToken, tok); err != nil {
			return nil, true, err
		}
		return vm, true, c.saveToken(ctx, tok)
	}
	vm, err = c.s.captureFile(agent, salt).Capture(ctype, req, reply)
	if err != nil {
		return nil, true, err
	}
	if err := c.s.Delete(ctx, ClaudeToken); err != nil && !errors.Is(err, ErrNone) {
		c.s.Warn(msg.SecretFailed(err.Error()).Because(err))
	}
	return vm, true, nil
}

// ownSecret reports whether the secret name is the Claude Code token.
func (c Claude) ownSecret(name string) bool { return name == ClaudeToken }

// oauthToken is what the claude secret's binding gets while no token is
// saved: the OAuth login's access token (Get).
func (c Claude) oauthToken(ctx context.Context) (string, error) {
	return c.s.Get(ctx, proxy.Binding{Kind: proxy.KindLogin, Agent: agents.ClaudeOAuth, Name: agentlogin.ClaudeOAuthName})
}

// OAuthInstead reports whether a VM's secret name, with no value saved,
// gets Claude Code's OAuth login instead: the claude secret, while devvm
// keeps that login, so the VM gets the one or the other, never both.
func (c Claude) OAuthInstead(name string) bool {
	return c.ownSecret(name) && c.s.HasLogin(agents.ClaudeOAuth)
}

// used reports whether a VM uses its login of kind, as far as Claude Code
// goes: its OAuth login only when it's the Claude Code login VMs get
// (Login), never with the Claude Code token; any other kind, yes.
func (c Claude) used(ctx context.Context, kind string) bool {
	if kind != agents.ClaudeOAuth {
		return true
	}
	l, err := c.Login(ctx)
	return err == nil && l == "oauth"
}

// bound reports whether the login of kind is Claude Code's OAuth login,
// whose binding bindings gives, not its file.
func (c Claude) bound(kind string) bool { return kind == agents.ClaudeOAuth }

// bindings are the bindings of the Claude Code login of a VM with these
// shared tools: a VM with Claude Code has its OAuth login's binding while
// devvm keeps either login, as it gets the one devvm keeps (Get), so the
// VM's files name the one it had at its launch.
func (c Claude) bindings(ctx context.Context, tools []string, salt string) ([]proxy.Binding, error) {
	if !slices.Contains(image.AgentsOf(tools), agents.Claude) {
		return nil, nil
	}
	if l, err := c.Login(ctx); err != nil || l == "" {
		return nil, err
	}
	return []proxy.Binding{agentlogin.ClaudeOAuthBinding(salt)}, nil
}

// Sums are hashes of the Claude Code logins devvm keeps, "" for one it
// doesn't: the OAuth login's file, and the saved Claude Code token, not
// Env's. They tell a login that changed one from one that didn't.
func (c Claude) Sums(ctx context.Context) (oauth, token string, err error) {
	if ok, err := c.s.login(agents.ClaudeOAuth, "").ClaudeLogin(); err != nil {
		return "", "", err
	} else if ok {
		oauth = c.s.LoginSum(agents.ClaudeOAuth)
	}
	v, err := c.s.saved(ctx, ClaudeToken)
	if errors.Is(err, ErrNone) {
		return oauth, "", nil
	} else if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(v))
	return oauth, hex.EncodeToString(sum[:]), nil
}
