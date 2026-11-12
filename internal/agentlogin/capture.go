package agentlogin

// Capturing logins: an agent logs in inside a VM, and its OAuth token endpoint's reply goes
// through the VM's proxy, which hands it here (proxy.Capturer, through devvm's secrets store). The
// tokens in it are saved in devvm's store of the agent's logins, in the agent's own format, under
// the file's lock, and the VM gets the reply with placeholders in their place: its access token's
// is the one the store's login binds, and a refresh token is only ever managed, a word, as the
// host refreshes the login. The VM's reply says its access token lasts until farFuture, so its
// agent never refreshes it.
//
// Endpoints and replies (checked against upstream, 2026-11): Codex and opencode (2's Codex
// logins, chatgpt-browser and chatgpt-headless; its chatgpt-token-sharing posts to
// /api/accounts/oauth/token, whose replies the proxy's guard refuses) both post to
// auth.openai.com/oauth/token, as a form, with OpenAI's client ID: authorization_code (the
// browser's code, or the device flow's) and refresh_token (Codex sends JSON) grants, answered
// with id_token, access_token, refresh_token and expires_in. Codex then exchanges the id_token for
// an API key (a token-exchange grant, requested_token=openai-api-key), answered with an
// access_token that's the key; the VM's request carries its placeholder of the id_token, and the
// store's id_token goes in its place. Claude Code posts to platform.claude.com/v1/oauth/token
// (console.anthropic.com's for older versions), as JSON, answered with access_token,
// refresh_token, expires_in and scope, and its account and organization (claude.go): its
// subscription's login goes in the store's .credentials.json (a File of kind ClaudeOAuth), and
// claude setup-token's token, which secrets.Store tells apart (SetupToken), with devvm's secrets.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

// The OAuth grants whose replies are captured, and what Codex's exchange asks for.
const (
	grantCode     = "authorization_code"
	grantRefresh  = "refresh_token"
	grantExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	apiKeyToken   = "openai-api-key"                            //nolint:gosec // a grant's field value, not a secret
	idTokenType   = "urn:ietf:params:oauth:token-type:id_token" //nolint:gosec // a grant's field value, not a secret
	// managed is what a VM's login holds in place of a token it never sends: its refresh token.
	managed = "devvm-proxy-managed"
)

// endpoint is one of an agent's OAuth token endpoints, whose replies every VM's proxy captures:
// its host and path, and as proxy.Capture has them, its method (POST when empty), the guard of a
// host that serves an API beside it, and the fields the agent keeps its tokens in beside OAuth's.
type endpoint struct {
	host, path, method, guard string
	tokenKeys                 []string
}

// captureFormat is a format whose logins the VM's proxy captures as its agent logs in (File.Capture),
// at its endpoints.
type captureFormat interface {
	format
	// endpoints are the agent's token endpoints, in order.
	endpoints() []endpoint
	// userAgents are how the User-Agents of the agent's token requests start, in lowercase, which
	// tell its login from another agent's at an endpoint they share (Pick): none for the agent a
	// shared endpoint's login is for when no other's matches.
	userAgents() []string
	// capture saves r, the reply to t, in m, the store's login, and returns m and the VM's reply.
	capture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error)
	// managedReply is the reply to a VM's refresh of its own login, m the store's: its
	// placeholders, as the code's reply gave them.
	managedReply(m map[string]any, salt string) ([]byte, error)
}

// exchangeFormat is a capture format whose agent also exchanges its login for an API key, a
// grant whose reply its capture reads when exchange reports t is it (Codex's).
type exchangeFormat interface {
	exchange(t tokenRequest) bool
}

// pollFormat is a capture format whose agent polls for its login, a GET whose query is the token
// request (Cursor's): poll reports whether t is that poll, the only request its capture reads.
type pollFormat interface {
	poll(t tokenRequest) bool
}

// refreshPlaceholderFormat is a capture format whose VM copy holds a placeholder as its refresh
// token too, besides managed: refreshPlaceholder returns it, m the store's login (opencode's
// holds its access token's).
type refreshPlaceholderFormat interface {
	refreshPlaceholder(m map[string]any, salt string) string
}

// captureOf returns the format of the agent's login file whose logins the proxy captures, and the
// file's kind: false when the proxy captures none of its logins.
func captureOf(agent string) (captureFormat, string, bool) {
	d, _ := agents.Get(agent)
	for _, l := range d.Logins {
		if c, ok := formats()[l.Kind].(captureFormat); ok {
			return c, l.Kind, true
		}
	}
	return nil, "", false
}

// CaptureKind is the kind of the agent's login file that a login the proxy captures goes in: its
// first whose format captures logins (Claude Code's OAuth login, beside its .claude.json), else
// its own name's.
func CaptureKind(agent string) string {
	if _, kind, ok := captureOf(agent); ok {
		return kind
	}
	return agent
}

// Endpoints returns the token endpoints whose replies every VM's proxy captures, whatever its
// image holds, as a VM can run an agent it installed itself: each capture format's, in
// agents.Defs' order.
func Endpoints() []proxy.Capture {
	var out []proxy.Capture
	for _, d := range agents.Defs {
		if c, _, ok := captureOf(d.Name); ok {
			for _, e := range c.endpoints() {
				out = append(out, proxy.Capture{Agent: d.Name, Host: e.host, Path: e.path, Method: e.method, Guard: e.guard, TokenKeys: e.tokenKeys})
			}
		}
	}
	return out
}

// Pick returns which of agents, the VM's whose token endpoint a request went to, it's for: the one
// whose capture format's userAgents the request's User-Agent starts with, else the first whose
// format has none, else the first. OpenAI's endpoint is Codex's and opencode's, and opencode runs
// on Bun.
func Pick(names []string, userAgent string) string {
	if len(names) == 0 {
		return ""
	}
	ua := strings.ToLower(userAgent)
	uas := func(a string) []string {
		c, _, _ := captureOf(a)
		if c == nil {
			return nil
		}
		return c.userAgents()
	}
	for _, a := range names {
		if slices.ContainsFunc(uas(a), func(p string) bool { return strings.HasPrefix(ua, p) }) {
			return a
		}
	}
	if len(names) > 1 {
		if i := slices.IndexFunc(names, func(a string) bool { return len(uas(a)) == 0 }); i >= 0 {
			return names[i]
		}
	}
	return names[0]
}

var (
	errNoLogin = errors.New("the reply holds no login devvm reads")
	errGrant   = errors.New("a token request devvm doesn't capture")
)

// tokenRequest is a token request's fields, as a form or JSON (its strings) sent them.
type tokenRequest struct {
	fields url.Values
	form   bool
}

func (t tokenRequest) grant() string { return t.fields.Get("grant_type") }

// errRepeated: a token request names a field twice, which devvm and the endpoint could read
// apart (the first or the last).
var errRepeated = errors.New("the token request repeats a field")

// parseTokenRequest reads a token request's body of Content-Type ctype: a form (or no type) or a
// JSON object, each field once.
func parseTokenRequest(ctype string, body []byte) (tokenRequest, error) {
	mt, _, _ := mime.ParseMediaType(ctype)
	switch mt {
	case "", "application/x-www-form-urlencoded":
		v, err := url.ParseQuery(string(body))
		if err != nil {
			return tokenRequest{}, errors.New("the token request isn't a form")
		}
		for _, vs := range v {
			if len(vs) > 1 {
				return tokenRequest{}, errRepeated
			}
		}
		return tokenRequest{fields: v, form: true}, nil
	case "application/json":
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if d.Decode(&m) != nil || m == nil {
			return tokenRequest{}, errors.New("the token request isn't a JSON object")
		}
		if n, err := topKeys(body); err != nil || n != len(m) {
			return tokenRequest{}, errRepeated
		}
		// Its strings, and its numbers as they're written (claude setup-token's expires_in).
		v := url.Values{}
		for k, x := range m {
			switch x := x.(type) {
			case string:
				v.Set(k, x)
			case json.Number:
				v.Set(k, x.String())
			}
		}
		return tokenRequest{fields: v}, nil
	}
	return tokenRequest{}, fmt.Errorf("a token request of type %q", mt)
}

// topKeys counts the keys of the JSON object b, each time one appears: more than its decoded
// map's means a key is repeated.
func topKeys(b []byte) (int, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	if _, err := d.Token(); err != nil { // {
		return 0, err
	}
	n := 0
	for d.More() {
		if _, err := d.Token(); err != nil { // the key
			return 0, err
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// grantOK refuses a request whose reply the agent's capture can't read: a grant other than a
// code's or a refresh's, or its format's exchange (exchangeFormat); for a format whose login is
// polled for (pollFormat), anything but its poll.
func (f File) grantOK(t tokenRequest) error {
	if p, ok := formats()[f.Kind].(pollFormat); ok {
		if p.poll(t) {
			return nil
		}
		return errGrant
	}
	switch t.grant() {
	case grantCode, grantRefresh:
		return nil
	case grantExchange:
		if x, ok := formats()[f.Kind].(exchangeFormat); ok && x.exchange(t) {
			return nil
		}
	}
	return errGrant
}

// apiKeyExchange reports whether t is Codex's exchange of its id_token for an API key, as a form.
func apiKeyExchange(t tokenRequest) bool {
	return t.grant() == grantExchange && t.form &&
		t.fields.Get("requested_token") == apiKeyToken && t.fields.Get("subject_token_type") == idTokenType
}

// readLogin reads the store's login file at path, or none (an empty one) when it isn't there.
func readLogin(path string) (map[string]any, error) {
	m, err := readJSON(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	return m, err
}

// lockStore readies f's folder in the store and holds its lock (lockLogin).
func (f File) lockStore() (func(), error) {
	if err := state.PrivateDir(filepath.Dir(f.Path)); err != nil {
		return nil, err
	}
	return lockLogin(f.Path)
}

// CaptureRequest returns a token request's body as it goes on to the agent's endpoint, f the
// store's login file: as it is, except Codex's API-key exchange, whose id_token, the VM's
// placeholder of the store's, becomes the store's. A Codex or opencode refresh of the VM's own
// login, whose refresh token is managed, goes nowhere: answer is the reply the VM gets, the
// store's login as placeholders, since the host refreshes it. A request whose reply can't be
// captured is refused, and so is an exchange for another id_token.
func (f File) CaptureRequest(ctype string, body []byte) (out, answer []byte, err error) {
	t, err := parseTokenRequest(ctype, body)
	if err != nil {
		return nil, nil, err
	}
	if err := f.grantOK(t); err != nil {
		return nil, nil, err
	}
	refresh := t.grant() == grantRefresh
	if t.grant() != grantExchange && !refresh {
		return body, nil, nil
	}
	unlock, err := f.lockStore()
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	m, err := readLogin(f.Path)
	if err != nil {
		return nil, nil, err
	}
	if refresh {
		rt := t.fields.Get("refresh_token")
		p, ok := formats()[f.Kind].(refreshPlaceholderFormat)
		if rt == managed || ok && rt == p.refreshPlaceholder(m, f.Salt) {
			answer, err = f.managedReply(m)
			return nil, answer, err
		}
		return body, nil, nil
	}
	id := str(obj(m, "tokens"), "id_token")
	if id == "" || t.fields.Get("subject_token") != openaiPlaceholder(id, f.Salt) {
		return nil, nil, errors.New("the API-key exchange isn't for the login devvm keeps")
	}
	t.fields.Set("subject_token", id)
	return []byte(t.fields.Encode()), nil, nil
}

// managedReply is the reply to a VM's refresh of its own login, m the store's: its placeholders,
// as the code's reply gave them.
func (f File) managedReply(m map[string]any) ([]byte, error) {
	c, ok := formats()[f.Kind].(captureFormat)
	if !ok {
		return nil, f.errKind()
	}
	return c.managedReply(m, f.Salt)
}

// openaiManagedReply is the managed reply of an OpenAI login (Codex's or opencode's): at, its
// access token, and id, its id_token when it has one, as placeholders.
func openaiManagedReply(at, id, salt string) ([]byte, error) {
	vm := vmReply(map[string]any{"token_type": "Bearer"})
	if at == "" {
		return nil, errors.New("the store has no login to refresh")
	}
	vm["access_token"], vm["refresh_token"] = openaiPlaceholder(at, salt), managed
	if id != "" {
		vm["id_token"] = openaiPlaceholder(id, salt)
	}
	return json.Marshal(vm)
}

// Capture saves the login in reply, the agent's endpoint's success for the token request req (of
// Content-Type ctype), in f, the store's login file, in the agent's own format, under its lock: a
// code's replaces the login there, a refresh's or an exchange's adds to it, and the store keeps
// only the newest refresh token. It returns the reply the VM gets: placeholders ending in f.Salt in
// place of the tokens, no refresh token, and an expiry at farFuture. A reply without the tokens a
// login needs is refused, and nothing is saved.
func (f File) Capture(ctype string, req, reply []byte) ([]byte, error) {
	t, err := parseTokenRequest(ctype, req)
	if err != nil {
		return nil, err
	}
	if err := f.grantOK(t); err != nil {
		return nil, err
	}
	var r map[string]any
	if json.Unmarshal(reply, &r) != nil || r == nil {
		return nil, errors.New("the reply isn't a JSON object")
	}
	c, ok := formats()[f.Kind].(captureFormat)
	if !ok {
		return nil, f.errKind()
	}
	unlock, err := f.lockStore()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := readLogin(f.Path)
	if err != nil {
		return nil, err
	}
	m, vm, err := c.capture(m, t, r, f.Salt)
	if err != nil {
		return nil, err
	}
	if err := writeRefreshed(f.Path, m); err != nil {
		return nil, err
	}
	return json.Marshal(vm)
}

// vmReply is the VM's reply with what of r holds no secret: its token type and scope, keep's
// fields, and an expiry at farFuture.
func vmReply(r map[string]any, keep ...string) map[string]any {
	out := map[string]any{"expires_in": farFuture - time.Now().Unix()}
	for _, k := range append([]string{"token_type", "scope"}, keep...) {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	return out
}

// expiresAt is when a reply's access token expires, in Unix milliseconds: its expires_in from now,
// an hour without one.
func expiresAt(r map[string]any) int64 {
	secs := 3600.0
	if v, ok := r["expires_in"].(float64); ok && v > 0 {
		secs = v
	}
	return time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
}

// chatgptAccount is the ChatGPT account a token's claims name, or "".
func chatgptAccount(tok string) string {
	c := jwtClaims(tok)
	if a := str(c, "chatgpt_account_id"); a != "" {
		return a
	}
	return str(obj(c, "https://api.openai.com/auth"), "chatgpt_account_id")
}

// codexCapture saves a Codex reply in m, its auth.json: a login's tokens, its account and when it
// was refreshed, or the exchange's API key. It returns m and the VM's reply.
func codexCapture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	vm := vmReply(r)
	if t.grant() == grantExchange {
		key := str(r, "access_token")
		if key == "" {
			return nil, nil, errNoLogin
		}
		m["OPENAI_API_KEY"] = key
		vm["access_token"] = codexKeyPlaceholder(salt)
		return m, vm, nil
	}
	id, at, rt := str(r, "id_token"), str(r, "access_token"), str(r, "refresh_token")
	if at == "" || t.grant() == grantCode && (id == "" || rt == "") {
		return nil, nil, errNoLogin
	}
	tk := obj(m, "tokens")
	if t.grant() == grantCode || tk == nil {
		// A new login: the key, if any, was another login's, and the exchange brings its own.
		tk = map[string]any{}
		m = map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": tk}
	}
	for k, v := range map[string]string{"id_token": id, "access_token": at, "refresh_token": rt} {
		if v != "" {
			tk[k] = v
		}
	}
	if a := chatgptAccount(str(tk, "id_token")); a != "" {
		tk["account_id"] = a
	}
	m["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if id != "" {
		vm["id_token"] = openaiPlaceholder(id, salt)
	}
	vm["access_token"] = openaiPlaceholder(at, salt)
	if rt != "" {
		vm["refresh_token"] = managed
	}
	return m, vm, nil
}

// opencodeCapture saves an opencode reply in m, its auth.json: its OpenAI login (openai), with its
// account. It returns m and the VM's reply.
func opencodeCapture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	id, at, rt := str(r, "id_token"), str(r, "access_token"), str(r, "refresh_token")
	if at == "" || t.grant() == grantCode && rt == "" {
		return nil, nil, errNoLogin
	}
	e := obj(m, "openai")
	if t.grant() == grantCode || str(e, "type") != "oauth" {
		e = map[string]any{"type": "oauth"}
		m["openai"] = e
	}
	e["access"], e["expires"] = at, expiresAt(r)
	if rt != "" {
		e["refresh"] = rt
	}
	if a := cmp.Or(chatgptAccount(id), chatgptAccount(at)); a != "" {
		e["accountId"] = a
	}
	vm := vmReply(r)
	if id != "" {
		vm["id_token"] = openaiPlaceholder(id, salt)
	}
	vm["access_token"] = openaiPlaceholder(at, salt)
	if rt != "" {
		vm["refresh_token"] = managed
	}
	return m, vm, nil
}
