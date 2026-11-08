package agentlogin

// Claude Code's logins, as devvm keeps one of them: its subscription's OAuth login, in the
// store's .claude/.credentials.json (a File of kind ClaudeOAuth), or a Claude Code token from
// claude setup-token, which devvm keeps with its secrets (secrets.Store), never both.
//
// Formats (checked against Claude Code 2.1.317's bundle, 2026-11): Claude Code logs in at
// ClaudeTokenURL with its public client ID (claudeClientID, PKCE, no secret), as JSON: a code's
// grant {grant_type, code, redirect_uri, client_id, code_verifier, state}, and a refresh's
// {grant_type, refresh_token, client_id, scope}. claude setup-token's code grant adds
// expires_in (a year, 31536000), with only the user:inference scope asked for. The reply holds
// access_token, refresh_token, expires_in, scope, and its account and organization. On Linux,
// Claude Code keeps its login in ~/.claude/.credentials.json, mode 600, as claudeAiOauth
// {accessToken, refreshToken, expiresAt (Unix ms), scopes, subscriptionType, rateLimitTier, ...};
// an empty refreshToken means a login it can't refresh. With an OAuth login (or
// CLAUDE_CODE_OAUTH_TOKEN, which wins over it) it sends the access token as a Bearer
// Authorization to api.anthropic.com, with its anthropic-beta header naming oauth-2025-04-20
// itself, so the proxy only swaps the placeholder.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

const (
	// ClaudeTokenURL is Claude Code's OAuth token endpoint, and claudeClientID its public client ID.
	ClaudeTokenURL = "https://platform.claude.com/v1/oauth/token" //nolint:gosec // a URL, not a secret
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"       //nolint:gosec // a public client ID, not a secret
	// claudeInference is the scope a login must have for Claude Code to use the model: a Console
	// login's (API billing) hasn't, so devvm keeps none.
	claudeInference = "user:inference"
	// claudeAPIKeyScope lets a login make an API key, at
	// api.anthropic.com/api/oauth/claude_cli/create_api_key: devvm keeps no login that has it.
	claudeAPIKeyScope = "org:create_api_key" //nolint:gosec // a scope's name, not a secret
	// ClaudeOAuthName is the OAuth login's binding's name: its file's kind's.
	ClaudeOAuthName = agents.ClaudeOAuth
	// claudeTokenStart is how Claude Code's access tokens and tokens from claude setup-token start.
	claudeTokenStart = "sk-ant-oat01-" //nolint:gosec // a token's prefix, not a secret
)

// ErrClaudeNoLogin is Get's error when the store has no Claude Code OAuth login.
var ErrClaudeNoLogin = errors.New("there's no Claude Code login")

// errConsole: a Claude login that can't use the model, such as a Console login's.
var errConsole = errors.New("a Claude login without the " + claudeInference + " scope or with " + claudeAPIKeyScope + ", such as a Console login's")

// claudeOAuthPlaceholder is the VM's in place of the OAuth login's access token.
func claudeOAuthPlaceholder(salt string) string {
	return salted(claudeTokenStart+"devvm-proxy-managed-oauth", salt)
}

func claudeOAuth(m map[string]any) map[string]any { return obj(m, "claudeAiOauth") }

func claudeOAuthBindings(m map[string]any, salt string) []proxy.Binding {
	if str(claudeOAuth(m), "accessToken") == "" {
		return nil
	}
	return []proxy.Binding{ClaudeOAuthBinding(salt)}
}

// ClaudeOAuthBinding is the binding of the OAuth login's placeholder ending in salt: its access
// token goes into Authorization for api.anthropic.com.
func ClaudeOAuthBinding(salt string) proxy.Binding {
	return login(agents.ClaudeOAuth, ClaudeOAuthName, []string{"api.anthropic.com"}, "Authorization", claudeOAuthPlaceholder(salt))
}

// claudeOAuthVM is the VM's .credentials.json: the login's placeholder, a refresh token that's
// only managed, and an expiry at farFuture, so the VM's Claude Code never refreshes it, and of
// the rest only what Claude Code reads of it, none of it a secret. Nothing else of the store's
// file goes.
func claudeOAuthVM(m map[string]any, salt string) map[string]any {
	o := claudeOAuth(m)
	if str(o, "accessToken") == "" {
		return map[string]any{}
	}
	c := map[string]any{"accessToken": claudeOAuthPlaceholder(salt), "refreshToken": managed, "expiresAt": int64(farFuture) * 1000}
	for _, k := range []string{"scopes", "subscriptionType", "rateLimitTier"} {
		if v, ok := o[k]; ok {
			c[k] = v
		}
	}
	return map[string]any{"claudeAiOauth": c}
}

func scopesOf(o map[string]any) []string {
	var out []string
	if l, ok := o["scopes"].([]any); ok {
		for _, s := range l {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// claudeOAuthGet is the OAuth login's access token, refreshed here when it's due, as Claude Code
// refreshes it, written back under the file's lock.
func (f File) claudeOAuthGet(ctx context.Context, name string) (string, error) {
	if name != ClaudeOAuthName {
		return "", fmt.Errorf("no Claude Code login %q", name)
	}
	unlock, err := lockLogin(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrClaudeNoLogin
	} else if err != nil {
		return "", err
	}
	defer unlock()
	m, err := readJSON(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrClaudeNoLogin
	} else if err != nil {
		return "", err
	}
	o := claudeOAuth(m)
	if str(o, "accessToken") == "" {
		return "", ErrClaudeNoLogin
	}
	exp, _ := o["expiresAt"].(json.Number)
	ms, _ := exp.Int64()
	if time.Until(time.UnixMilli(ms)) > refreshMargin {
		return str(o, "accessToken"), nil
	}
	fields := map[string]string{"grant_type": grantRefresh, "refresh_token": str(o, "refreshToken"), "client_id": claudeClientID}
	if sc := scopesOf(o); len(sc) > 0 {
		fields["scope"] = strings.Join(sc, " ")
	}
	nt, err := f.refresh(ctx, false, fields)
	if err == nil && !claudeTokenOK(str(nt, "access_token")) {
		err = errors.New("the reply's access token isn't Claude Code's")
	} else if err == nil {
		err = claudeScopesOK(strings.Fields(str(nt, "scope")), false)
	}
	if err != nil {
		return "", &RefreshError{Agent: agents.Claude, Err: err}
	}
	o["accessToken"], o["expiresAt"] = str(nt, "access_token"), expiresAt(nt)
	if v := str(nt, "refresh_token"); v != "" {
		o["refreshToken"] = v
	}
	if sc := str(nt, "scope"); sc != "" {
		o["scopes"] = strings.Fields(sc)
	}
	return str(o, "accessToken"), writeRefreshed(f.Path, m)
}

// claudeCapture saves a Claude Code reply in m, its .credentials.json: its OAuth login
// (claudeAiOauth), as Claude Code keeps it. A code's login must be able to use the model
// (claudeInference), so a Console login is refused. It returns m and the VM's reply, with its
// account and organization.
func claudeCapture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	at, rt := str(r, "access_token"), str(r, "refresh_token")
	if !claudeTokenOK(at) || t.grant() == grantCode && rt == "" {
		return nil, nil, errNoLogin
	}
	o := claudeOAuth(m)
	if err := claudeScopesOK(strings.Fields(str(r, "scope")), t.grant() == grantCode || o == nil); err != nil {
		return nil, nil, err
	}
	if t.grant() == grantCode || o == nil {
		o = map[string]any{}
		m = map[string]any{"claudeAiOauth": o}
	}
	o["accessToken"], o["expiresAt"] = at, expiresAt(r)
	if rt != "" {
		o["refreshToken"] = rt
	}
	if sc := str(r, "scope"); sc != "" {
		o["scopes"] = strings.Fields(sc)
	}
	vm := vmReply(r, "account", "organization")
	vm["access_token"] = claudeOAuthPlaceholder(salt)
	if rt != "" {
		vm["refresh_token"] = managed
	}
	return m, vm, nil
}

// claudeTokenOK reports whether tok is shaped as Claude Code's access tokens are: its start, then
// only characters a header and the Keychain's command line take.
func claudeTokenOK(tok string) bool {
	return strings.HasPrefix(tok, claudeTokenStart) &&
		!strings.ContainsFunc(tok, func(r rune) bool { return r <= ' ' || r == '"' || r == '\\' || r == 0x7f })
}

// claudeScopesOK refuses a login devvm doesn't keep, by the scopes its reply grants: one that can
// make an API key (claudeAPIKeyScope), which the proxy would let a VM make with the login, and,
// for a new login, one that can't use the model (claudeInference), such as a Console login's. A
// refresh that names no scope keeps the login's.
func claudeScopesOK(scopes []string, newLogin bool) error {
	if slices.Contains(scopes, claudeAPIKeyScope) || newLogin && !slices.Contains(scopes, claudeInference) {
		return errConsole
	}
	return nil
}

// claudeManagedReply is the reply to a VM's refresh of its own OAuth login, m the store's: its
// placeholder and scopes, as the code's reply gave them.
func claudeManagedReply(m map[string]any, salt string) ([]byte, error) {
	o := claudeOAuth(m)
	if str(o, "accessToken") == "" {
		return nil, errors.New("the store has no login to refresh")
	}
	vm := vmReply(map[string]any{"token_type": "Bearer"})
	vm["access_token"], vm["refresh_token"] = claudeOAuthPlaceholder(salt), managed
	if sc := scopesOf(o); len(sc) > 0 {
		vm["scope"] = strings.Join(sc, " ")
	}
	return json.Marshal(vm)
}

// SetupToken reports whether a token request to Claude Code's endpoint, of Content-Type ctype, is
// claude setup-token's: a code's, asking for a long-lived token (expires_in). Its reply is a
// Claude Code token (ClaudeToken), not a login devvm refreshes.
func SetupToken(ctype string, body []byte) bool {
	t, err := parseTokenRequest(ctype, body)
	return err == nil && t.grant() == grantCode && t.fields.Has("expires_in")
}

// ClaudeToken returns the Claude Code token in reply, the endpoint's success for claude
// setup-token's request req (SetupToken), and the reply the VM gets: placeholder in its place, a
// refresh token only managed, and its account and organization. A reply without a token that
// starts as Claude Code's do is refused.
func ClaudeToken(ctype string, req, reply []byte, placeholder string) (string, []byte, error) {
	if !SetupToken(ctype, req) {
		return "", nil, errGrant
	}
	var r map[string]any
	if json.Unmarshal(reply, &r) != nil || r == nil {
		return "", nil, errors.New("the reply isn't a JSON object")
	}
	at := str(r, "access_token")
	if !strings.HasPrefix(at, claudeTokenStart) {
		return "", nil, errNoLogin
	}
	vm := vmReply(r, "account", "organization")
	vm["access_token"] = placeholder
	if str(r, "refresh_token") != "" {
		vm["refresh_token"] = managed
	}
	out, err := json.Marshal(vm)
	return at, out, err
}

// LockFolder holds an exclusive lock on dir, a folder of logins, while one login replaces another
// across files, and returns the unlock. It's another lock than a login file's own (lockLogin's,
// on its folder), so its holder can take that one too.
func LockFolder(dir string) (func(), error) { return lockLogin(filepath.Join(dir, "lock")) }

// Remove deletes f, the store's login file, under its lock: none there is no error.
func (f File) Remove() error {
	unlock, err := lockLogin(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ClaudeLogin reports whether f, the store's .credentials.json, holds a Claude Code login.
func (f File) ClaudeLogin() (bool, error) {
	m, err := readLogin(f.Path)
	if err != nil {
		return false, err
	}
	return str(claudeOAuth(m), "accessToken") != "", nil
}
