package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
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
