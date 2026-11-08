// Package agentlogin is agents' files on the host as VMs see them. Codex
// (~/.codex/auth.json), opencode (~/.local/share/opencode/auth.json) and pi (~/.pi/agent/auth.json,
// pi.go) keep their logins there, and
// Claude Code its MCP servers' secrets (~/.claude.json) and its OAuth login
// (~/.claude/.credentials.json, claude.go). Every VM gets its own copy of each file
// with placeholders instead of keys and tokens (File.VMCopy), and the proxy puts the real ones
// into requests for each provider's hosts (File.Bindings), looking each value up in its own process
// (File.Get).
// OAuth access tokens are refreshed here, on the host, and written back to the host's file, so the
// host's login keeps working: a VM never holds a refresh token, so it can't rotate one away. A
// refresh goes through the company proxy the VM's proxy goes through (proxy.RouteOf).
// A login made inside a VM is captured from its token endpoint's reply (File.Capture, capture.go)
// and saved in the store, the VM given placeholders.
// Agents' settings files get their secret-looking values taken out (Redact).
//
// Formats (checked against upstream, 2026-10): Codex keeps OPENAI_API_KEY or tokens{id_token,
// access_token, refresh_token, account_id} plus last_refresh; it decodes id_token's claims and
// access_token's exp (never the signatures) and refreshes 5 minutes before exp. opencode keeps one
// entry per provider: {"type":"api","key"} or {"type":"oauth","access","refresh","expires" (ms)}.
// opencode 2 (checked at v2.2.5) keeps its logins in its SQLite database instead, and imports
// auth.json, in that format, once, as it makes its credential table; devvm's store keeps the
// file, which a VM's opencode reads as it first runs, and each setup after that imports what changed
// (OpencodeImport).
package agentlogin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

const (
	// OpenAITokenURL is OpenAI's OAuth token endpoint, which refreshes Codex's and opencode's
	// ChatGPT logins, and openaiClientID Codex's public client ID (opencode uses the same one).
	OpenAITokenURL = "https://auth.openai.com/oauth/token" //nolint:gosec // a URL, not a secret
	openaiClientID = "app_EMoamEEZ73f0CkXaXp7hrann"        //nolint:gosec // a public client ID, not a secret
	// Refresh this long before an access token expires: the proxy caches values for a while.
	refreshMargin = 10 * time.Minute
	// How long a token refresh may take.
	refreshTimeout = 30 * time.Second
	// The most of a token endpoint's reply read.
	replyMax = 1 << 20
	// farFuture is 2099-01-01 UTC, in Unix seconds: when the VM's placeholders expire.
	farFuture = 4070908800
)

// The errors of Get that logging in again with the agent on the host fixes.
var (
	// ErrCodexNoKey: Codex's login on the host has no API key.
	ErrCodexNoKey = errors.New("codex isn't logged in with an API key")
	// ErrCodexNoChatGPT: Codex's login on the host has no ChatGPT login.
	ErrCodexNoChatGPT = errors.New("codex isn't logged in with ChatGPT")
)

// RefreshError is Get's error for an OAuth login whose refresh on the host failed: Agent's (an
// agents.Def's Name), and for opencode its Provider's. Logging in again with the agent fixes it.
type RefreshError struct {
	Agent, Provider string
	Err             error
}

func (e *RefreshError) Error() string { return "refreshing the login: " + e.Err.Error() }
func (e *RefreshError) Unwrap() error { return e.Err }

// File is one agent's file on the host.
type File struct {
	Kind string // an agents.Login's Kind, whose format it has (formats)
	Path string
	// Salt is the VM's key, which ends every placeholder, so each VM's are its own: a proxy puts a
	// value in only for the placeholders bound for its VM. Empty: none.
	Salt string
	// TokenURL is where OAuth tokens are refreshed: when empty, ClaudeTokenURL for ClaudeOAuth,
	// else OpenAITokenURL.
	TokenURL string
	// Pass are the providers whose API-key entries the VM's copy holds as they are (opencode's
	// or pi's, from config.toml's [pass-keys]), unless devvm binds them; any other entry devvm
	// doesn't bind is withheld.
	Pass []string
}

// salted: a placeholder made the VM's own.
func salted(ph, salt string) string {
	if salt == "" {
		return ph
	}
	return ph + "-" + salt
}

// withheldKeep are the fields a withheld login entry (opencode's or pi's, neither bound nor
// passed) keeps in the VM's copy: those that name an account or how it logs in, never a key or a
// token.
var withheldKeep = []string{"type", "expires", "accountId", "enterpriseUrl", "projectId", "email", "scope", "scopes", "clientId"}

// withheldEntry is a login entry e as the VM's copy holds it withheld: withheldKeep's fields
// alone, each a plain value or a list of strings.
func withheldEntry(e map[string]any) map[string]any {
	kept := map[string]any{}
	for _, k := range withheldKeep {
		if v, ok := e[k]; ok && plainField(v) {
			kept[k] = v
		}
	}
	return kept
}

// plainField: a string, number, bool or null, or a list of strings.
func plainField(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return false
	case []any:
		for _, x := range t {
			if _, ok := x.(string); !ok {
				return false
			}
		}
	}
	return true
}

// splitUnbound returns the entries of m, a login file of one entry per provider, that devvm
// doesn't bind: those passed, which the VM's copy holds as they are, and the others, which it
// holds withheld (withheldEntry), each sorted.
func splitUnbound(m map[string]any, bound, passed func(id string) bool) (pass, withheld []string) {
	for _, id := range sortedKeys(m) {
		switch {
		case bound(id):
		case passed(id):
			pass = append(pass, id)
		default:
			withheld = append(withheld, id)
		}
	}
	return pass, withheld
}

// opencode providers devvm knows: where an API key or access token goes. Logins for other providers
// are withheld from the VM's copy unless config.toml's [pass-keys] passes them (opencodeVM). pi's
// API keys are bound for opencodeAPI's providers too (pi.go).
type provider struct {
	hosts  []string
	header string
}

var opencodeAPI = map[string]provider{
	"anthropic":  {[]string{"api.anthropic.com"}, "x-api-key"},
	"openai":     {[]string{"api.openai.com"}, "Authorization"},
	"openrouter": {[]string{"openrouter.ai"}, "Authorization"},
}

var opencodeOAuth = map[string]provider{
	"openai":         {[]string{"chatgpt.com"}, "Authorization"},
	"github-copilot": {[]string{"api.githubcopilot.com"}, "Authorization"},
}

// fileMax is the most of an agent's file read: ~/.claude.json keeps every project's history, so it
// grows.
const fileMax = 64 << 20

// readJSON reads an agent's file: a regular file (proxy.ReadRegular, so a FIFO or a device there
// can't hold devvm), at most fileMax, holding a JSON object.
func readJSON(path string) (map[string]any, error) {
	data, err := proxy.ReadRegular(path, fileMax)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// lockLogin holds an exclusive lock on the login file's folder while a lookup reads, refreshes and
// writes it back: VMs each have a proxy, and two refreshing with the same (rotating) refresh token
// would lose one result or get the login revoked. It returns the unlock.
func lockLogin(path string) (func(), error) {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		_ = d.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(d.Fd()), syscall.LOCK_UN); _ = d.Close() }, nil
}

// writeJSON replaces path atomically, keeping it mode 600.
func writeJSON(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFile(path, append(data, '\n'))
}

// writeRefreshed writes a login just refreshed: the old refresh token may no longer work, so a
// failed write (a full disk for a moment, say) is tried again before giving up.
func writeRefreshed(path string, m map[string]any) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = writeJSON(path, m); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

// --- JWTs (never verified: only read for claims, or made up as placeholders) ---

func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var c map[string]any
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return c
}

func jwtExp(tok string) (time.Time, bool) {
	exp, ok := jwtClaims(tok)["exp"].(float64)
	return time.Unix(int64(exp), 0), ok
}

// fakeJWT: a JWT-shaped placeholder with these claims and no real signature. Deterministic, so the
// same claims give the same placeholder (bindings name it).
func fakeJWT(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "." + jwtMark
}

// jwtMark is the signature of every JWT-shaped placeholder (fakeJWT).
const jwtMark = "devvm-placeholder"

// The account claims Codex and opencode read from tokens, without anything that grants access.
func accountClaims(tok, salt string) map[string]any {
	c := jwtClaims(tok)
	out := map[string]any{"exp": farFuture, "devvm": salted("placeholder", salt)}
	if a, ok := c["https://api.openai.com/auth"]; ok {
		out["https://api.openai.com/auth"] = a
	}
	for _, k := range []string{"email", "https://api.openai.com/profile", "chatgpt_account_id"} {
		if v, ok := c[k]; ok {
			out[k] = v
		}
	}
	return out
}

// openaiPlaceholder is the VM's in place of an OpenAI token (Codex's and opencode's): a JWT with
// tok's account claims, ending in salt.
func openaiPlaceholder(tok, salt string) string { return fakeJWT(accountClaims(tok, salt)) }

// --- Codex ---

// codexLogin is the name of Codex's ChatGPT login's binding: its agent's.
const codexLogin = agents.Codex

// codexKeyPlaceholder is the VM's in place of Codex's API key.
func codexKeyPlaceholder(salt string) string { return salted("sk-devvm-proxy-managed-codex", salt) }

// login is one of agent's logins as a binding, of proxy.KindLogin: its value is File.Get's for name.
func login(agent, name string, hosts []string, header, placeholder string) proxy.Binding {
	return proxy.Binding{Name: name, Kind: proxy.KindLogin, Agent: agent, Hosts: hosts, Header: header, Placeholder: placeholder}
}

func codexBindings(m map[string]any, salt string) []proxy.Binding {
	var bs []proxy.Binding
	if str(m, "OPENAI_API_KEY") != "" {
		bs = append(bs, login(agents.Codex, "codex-key", []string{"api.openai.com"}, "Authorization", codexKeyPlaceholder(salt)))
	}
	if t := obj(m, "tokens"); str(t, "access_token") != "" {
		bs = append(bs, login(agents.Codex, codexLogin, []string{"chatgpt.com", "api.openai.com"}, "Authorization",
			openaiPlaceholder(str(t, "access_token"), salt)))
	}
	return bs
}

// codexKnown: auth.json's keys codexVM knows. Others are copied as they are, and named.
var codexKnown = map[string]bool{"OPENAI_API_KEY": true, "tokens": true, "last_refresh": true, "auth_mode": true}

func codexOthers(m map[string]any) []string {
	var others []string
	for k := range m {
		if !codexKnown[k] {
			others = append(others, k)
		}
	}
	sort.Strings(others)
	return others
}

func codexVM(m map[string]any, salt string) map[string]any {
	bs := codexBindings(m, salt)
	t := obj(m, "tokens")
	// The placeholder Codex's API-key exchange sends back (CaptureRequest).
	id := openaiPlaceholder(str(t, "id_token"), salt)
	// Every token goes, bound or not (a login without an access token still has the others).
	for _, k := range []string{"id_token", "refresh_token", "access_token"} {
		if str(t, k) != "" {
			t[k] = managed
		}
	}
	for _, b := range bs {
		switch b.Name {
		case "codex-key":
			m["OPENAI_API_KEY"] = b.Placeholder
		case codexLogin:
			t["id_token"] = id
			t["access_token"] = b.Placeholder
			// Just refreshed, as far as the VM's Codex can tell: it never tries.
			m["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	return m
}

func (f File) codexGet(ctx context.Context, name string) (string, error) {
	unlock, err := lockLogin(f.Path)
	if err != nil {
		return "", err
	}
	defer unlock()
	m, err := readJSON(f.Path)
	if err != nil {
		return "", err
	}
	switch name {
	case "codex-key":
		if k := str(m, "OPENAI_API_KEY"); k != "" {
			return k, nil
		}
		return "", ErrCodexNoKey
	case codexLogin:
		t := obj(m, "tokens")
		if str(t, "access_token") == "" {
			return "", ErrCodexNoChatGPT
		}
		if exp, ok := jwtExp(str(t, "access_token")); ok && time.Until(exp) > refreshMargin {
			return str(t, "access_token"), nil
		}
		nt, err := f.refresh(ctx, false, map[string]string{
			"grant_type": "refresh_token", "client_id": openaiClientID, "refresh_token": str(t, "refresh_token"),
		})
		if err != nil {
			return "", &RefreshError{Agent: agents.Codex, Err: err}
		}
		for _, k := range []string{"id_token", "access_token", "refresh_token"} {
			if v := str(nt, k); v != "" {
				t[k] = v
			}
		}
		m["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		return str(t, "access_token"), writeRefreshed(f.Path, m)
	}
	return "", fmt.Errorf("no Codex login %q", name)
}

// --- opencode ---

// opencodePlaceholder is the VM's in place of a provider's API key or token, but OpenAI's OAuth
// login's (openaiPlaceholder).
func opencodePlaceholder(provider, salt string) string {
	return salted("devvm-proxy-managed-opencode-"+provider, salt)
}

func opencodeBindings(m map[string]any, salt string) []proxy.Binding {
	var bs []proxy.Binding
	for id := range m {
		e := obj(m, id)
		var p provider
		var ok bool
		switch str(e, "type") {
		case "api":
			p, ok = opencodeAPI[id]
		case "oauth":
			p, ok = opencodeOAuth[id]
			ok = ok && str(e, "enterpriseUrl") == ""
		}
		if !ok {
			continue
		}
		ph := opencodePlaceholder(id, salt)
		if id == "openai" && str(e, "type") == "oauth" {
			ph = openaiPlaceholder(str(e, "access"), salt) // opencode reads the account from it
		}
		bs = append(bs, login(agents.Opencode, "opencode-"+id, p.hosts, p.header, ph))
	}
	return bs
}

// opencodeVM is the VM's copy of opencode's logins m (checked against opencode's auth.json schema
// at v1.19.5 and its import at v2.2.5: {"type":"api","key","metadata"},
// {"type":"oauth","refresh","access","expires","accountId","enterpriseUrl"} or
// {"type":"wellknown","key","token"}), each entry held in one of three ways:
//   - bound: an API key's entry becomes {type, key} alone, its key the placeholder, so its
//     metadata stays on the host; an OAuth login's keeps withheldKeep's fields, its access and
//     refresh tokens the placeholder.
//   - passed: an api entry of a provider pass lists (File.Pass, config.toml's [pass-keys]) that
//     devvm doesn't bind goes as it is.
//   - withheld: every other entry, OAuth and well-known logins included, as withheldEntry makes
//     it, which opencode skips as it reads the file.
func opencodeVM(m map[string]any, salt string, pass []string) map[string]any {
	out := map[string]any{}
	for _, b := range opencodeBindings(m, salt) {
		id := strings.TrimPrefix(b.Name, "opencode-")
		e := obj(m, id)
		if str(e, "type") == "api" {
			out[id] = map[string]any{"type": "api", "key": b.Placeholder}
			continue
		}
		v := withheldEntry(e)
		v["access"] = b.Placeholder
		v["refresh"] = b.Placeholder // Copilot sends this one; nothing else in the VM refreshes
		if n, _ := e["expires"].(json.Number); n.String() != "0" {
			v["expires"] = farFuture * 1000
		}
		out[id] = v
	}
	for _, id := range sortedKeys(m) {
		if _, done := out[id]; done {
			continue
		}
		if opencodePassed(m, id, out, pass) {
			out[id] = m[id]
			continue
		}
		out[id] = withheldEntry(obj(m, id))
	}
	return out
}

// opencodePassed reports whether opencode's entry for provider id goes into the VM's copy as it
// is: an api entry of a provider in pass that devvm doesn't bind, bound holding the bound ones.
func opencodePassed(m map[string]any, id string, bound map[string]any, pass []string) bool {
	_, isBound := bound[id]
	return !isBound && str(obj(m, id), "type") == "api" && slices.Contains(pass, id)
}

// opencodeSplit returns the providers of opencode's whose entries the VM's copy holds as they are
// (passed) and those it holds without their keys and tokens (neither bound nor passed), each
// sorted.
func opencodeSplit(m map[string]any, pass []string) (passed, withheld []string) {
	bound := map[string]any{}
	for _, b := range opencodeBindings(m, "") {
		bound[strings.TrimPrefix(b.Name, "opencode-")] = true
	}
	return splitUnbound(m,
		func(id string) bool { _, ok := bound[id]; return ok },
		func(id string) bool { return opencodePassed(m, id, bound, pass) })
}

func (f File) opencodeGet(ctx context.Context, name string) (string, error) {
	unlock, err := lockLogin(f.Path)
	if err != nil {
		return "", err
	}
	defer unlock()
	m, err := readJSON(f.Path)
	if err != nil {
		return "", err
	}
	id := strings.TrimPrefix(name, "opencode-")
	e := obj(m, id)
	switch str(e, "type") {
	case "api":
		return str(e, "key"), nil
	case "oauth":
		if id == "github-copilot" {
			return str(e, "refresh"), nil // a GitHub token, never refreshed
		}
		exp, _ := e["expires"].(json.Number)
		ms, _ := exp.Int64()
		if str(e, "access") != "" && time.Until(time.UnixMilli(ms)) > refreshMargin {
			return str(e, "access"), nil
		}
		nt, err := f.refresh(ctx, true, map[string]string{
			"grant_type": "refresh_token", "client_id": openaiClientID, "refresh_token": str(e, "refresh"),
		})
		if err != nil {
			return "", &RefreshError{Agent: agents.Opencode, Provider: id, Err: err}
		}
		e["access"] = str(nt, "access_token")
		if v := str(nt, "refresh_token"); v != "" {
			e["refresh"] = v
		}
		secs := 3600.0
		if v, ok := nt["expires_in"].(float64); ok {
			secs = v
		}
		e["expires"] = time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
		return str(e, "access"), writeRefreshed(f.Path, m)
	}
	return "", fmt.Errorf("no opencode login %q", id)
}

// tokenURL is where f's OAuth tokens are refreshed (File.TokenURL).
func (f File) tokenURL() string {
	switch {
	case f.TokenURL != "":
		return f.TokenURL
	case f.Kind == agents.ClaudeOAuth:
		return ClaudeTokenURL
	}
	return OpenAITokenURL
}

// refresh posts an OAuth refresh request to f.tokenURL(), as JSON (Codex) or a form (opencode),
// within refreshTimeout. Its caller's end doesn't cancel it: the server may already have rotated
// the refresh token, and only the reply, written back to the login file, holds the new one.
func (f File) refresh(ctx context.Context, form bool, fields map[string]string) (map[string]any, error) {
	u := f.tokenURL()
	var body []byte
	ctype := "application/json"
	if form {
		v := url.Values{}
		for k, f := range fields {
			v.Set(k, f)
		}
		body, ctype = []byte(v.Encode()), "application/x-www-form-urlencoded"
	} else {
		body, _ = json.Marshal(fields)
	}
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	client := fetch.NewClient(refreshTimeout, fetch.HTTPSOnly)
	if route := proxy.RouteOf(ctx); route != nil {
		// The VM's proxy's company proxy, password included (proxy.WithRoute).
		client.Transport().Proxy = route
	}
	// The VM's proxy's roots: the system's and the user's CAs (proxy.WithRoots).
	client.Trust(proxy.RootsOf(ctx))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &fetch.StatusError{URL: u, Code: resp.StatusCode, Status: resp.Status}
	}
	data, err := fetch.ReadCapped(resp.Body, replyMax)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if str(out, "access_token") == "" {
		return nil, errors.New("no access_token in the reply")
	}
	return out, nil
}

// --- Claude Code: MCP servers' secrets in ~/.claude.json ---
//
// `claude mcp add -s user` keeps a server's env values (stdio servers) and HTTP headers (remote
// servers) in ~/.claude.json, in plain text. The VM's copy gets placeholders instead. An HTTPS
// server's header becomes a binding for its host, so the proxy puts the real value in (read from
// the host's file each time); env values can't be put in by a proxy, so stdio servers that need them
// don't get them. Per-project entries (.projects) never go to VMs.

// The MCP servers, sorted by name, and each one's settings.
func claudeServers(m map[string]any) ([]string, map[string]any) {
	servers := obj(m, "mcpServers")
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, servers
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// claudeBindingName: a binding's name from its server and header (names are free text; this is
// safe in the proxy's log and as its cache's key).
func claudeBindingName(server, header string) string {
	h := sha256.Sum256([]byte(server + "\x00" + strings.ToLower(header)))
	return "claude-mcp-" + hex.EncodeToString(h[:6])
}

// claudeHost: the host of an HTTPS server on the default port (where the proxy intercepts), or "".
func claudeHost(s map[string]any) string {
	u, err := url.Parse(str(s, "url"))
	if err != nil || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	// A name only: it goes into the VM's allowlist, where an address, a wildcard or a range would
	// open more than the one server.
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if _, err := netip.ParseAddr(h); err != nil && proxy.HostOK(h) && strings.Contains(h, ".") {
		return h
	}
	return ""
}

func claudeBindings(m map[string]any, salt string) []proxy.Binding {
	var bs []proxy.Binding
	names, servers := claudeServers(m)
	for _, n := range names {
		s := obj(servers, n)
		host := claudeHost(s)
		if host == "" {
			continue
		}
		for _, h := range sortedKeys(obj(s, "headers")) {
			name := claudeBindingName(n, h)
			bs = append(bs, login(agents.Claude, name, []string{host}, h, salted("devvm-proxy-managed-"+name, salt)))
		}
	}
	return bs
}

// What of ~/.claude.json the VM's copy keeps: settings, onboarding and account state (not
// credentials), and MCP servers. Anything else (primaryApiKey, keys a later Claude Code adds) stays
// on the host; the VM keeps its own value of it.
var claudeTopKeys = map[string]bool{
	"mcpServers": true, "hasCompletedOnboarding": true, "lastOnboardingVersion": true, "theme": true,
	"editorMode": true, "verbose": true, "autoCompactEnabled": true, "preferredNotifChannel": true,
	"messageIdleNotifThresholdMs": true, "shiftEnterKeyBindingInstalled": true, "optionAsMetaKeyInstalled": true,
	"hasAcknowledgedCostThreshold": true, "bypassPermissionsModeAccepted": true, "hasSeenTasksHint": true,
	"hasUsedBackslashReturn": true, "oauthAccount": true, "userID": true, "numStartups": true,
	"firstStartTime": true, "tipsHistory": true, "isQualifiedForDataSharing": true, "hasIdeOnboardingBeenShown": true,
	"lastReleaseNotesSeen": true, "subscriptionNoticeCount": true, "hasAvailableSubscription": true,
	"diffTool": true, "todoFeatureEnabled": true, "showExpandedTodos": true, "autoConnectIde": true,
	"autoInstallIdeExtension": true, "fallbackAvailableWarningThreshold": true,
}

// An MCP server's settings the VM's copy keeps (oauth: only its client ID and callback port).
var claudeServerKeys = map[string]bool{
	"type": true, "url": true, "command": true, "args": true, "env": true,
	"headers": true, "headersHelper": true, "oauth": true,
}

// Secret-looking names (query parameters, flags) and values (known key shapes, long random strings).
var (
	secretName  = regexp.MustCompile(`(?i)(key|token|secret|passw|pwd|auth|bearer|credential|session|sig)`)
	secretShape = regexp.MustCompile(`^(sk-|sk_|pk_|rk_|ghp_|gho_|ghs_|ghu_|github_pat_|glpat-|xox[abpr]-|AKIA|ASIA|AIza|ya29\.|eyJ)`)
	uuidShape   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	tokenShape  = regexp.MustCompile(`^[A-Za-z0-9_\-+=~]{24,}$`)
	// In free text, such as a hook's command: a URL; a known key's shape after a word's start (not
	// across a /, which no key has, so a path keeps its other folders); and a Bearer or Basic
	// credential, quoted or not, of 16 or more token characters with a digit or one of +/= in it,
	// which words don't have, and not a path.
	urlInText    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]+`)
	shapeInText  = regexp.MustCompile(`(^|[^A-Za-z0-9_])(?:sk-|sk_|pk_|rk_|ghp_|gho_|ghs_|ghu_|github_pat_|glpat-|xox[abpr]-|AKIA|ASIA|AIza|ya29\.|eyJ)[A-Za-z0-9_\-.+=~]{8,}`)
	schemeInText = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+['"]?([A-Za-z0-9_\-.+/=~]{16,})`)
)

// scrubText: s with the URLs in it scrubbed (scrubURL) and the keys and credentials it holds
// among other words replaced (shapeInText, schemeInText), the words around them kept.
func scrubText(s string) string {
	s = urlInText.ReplaceAllStringFunc(s, scrubURL)
	s = shapeInText.ReplaceAllString(s, "${1}devvm-redacted")
	return schemeInText.ReplaceAllStringFunc(s, func(m string) string {
		cred := schemeInText.FindStringSubmatch(m)[1]
		if !strings.ContainsAny(cred, "0123456789+/=") || strings.HasPrefix(cred, "/") || strings.HasPrefix(cred, "~/") {
			return m
		}
		return strings.TrimSuffix(m, cred) + "devvm-redacted"
	})
}

// scrubCommand: a shell command (an MCP server's command or headersHelper) as the VM's copy
// holds it: a known key's shape whole, else scrubText's.
func scrubCommand(s string) string {
	if secretShape.MatchString(s) {
		return "devvm-redacted"
	}
	return scrubText(s)
}

// secretLike: v looks like a key or token: a known prefix, a UUID, or 24+ token characters mixing
// letters and digits.
func secretLike(v string) bool {
	if secretShape.MatchString(v) || uuidShape.MatchString(v) {
		return true
	}
	return tokenShape.MatchString(v) && strings.ContainsAny(v, "0123456789") &&
		strings.ContainsAny(strings.ToLower(v), "abcdefghijklmnopqrstuvwxyz")
}

// scrubURL: the URL without user info, and with secret-looking query values and path segments
// replaced (the VM's copy can't have a placeholder there: the proxy only fills in headers).
func scrubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return raw
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k, vs := range q {
			for i, v := range vs {
				if v != "" && (secretName.MatchString(k) || secretLike(v)) {
					vs[i] = "devvm-redacted"
					changed = true
				}
			}
			q[k] = vs
		}
		u.RawQuery = q.Encode()
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		if secretLike(s) {
			segs[i] = "devvm-redacted"
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.Path, u.RawPath = strings.Join(segs, "/"), ""
	return u.String()
}

// scrubArgs: a server's arguments with secret values replaced: after a secret-named flag
// (--api-key X), in --flag=X with one, any that look like a key, and URLs' (scrubURL).
func scrubArgs(v any) (any, bool) {
	as, ok := v.([]any)
	if !ok {
		return v, false
	}
	out, changed := make([]any, len(as)), false
	for i, a := range as {
		out[i] = a
		s, ok := a.(string)
		if !ok {
			continue
		}
		var prev string
		if i > 0 {
			prev, _ = as[i-1].(string)
		}
		switch {
		case strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=") && secretName.MatchString(prev) && !strings.HasPrefix(s, "-"):
			out[i], changed = "devvm-redacted", true
		case strings.HasPrefix(s, "-") && strings.Contains(s, "="):
			k, val, _ := strings.Cut(s, "=")
			if val != "" && (secretName.MatchString(k) || secretLike(val)) {
				out[i], changed = k+"=devvm-redacted", true
			} else if u := scrubURL(val); strings.Contains(val, "://") && u != val {
				out[i], changed = k+"="+u, true
			}
		case secretLike(s):
			out[i], changed = "devvm-redacted", true
		case strings.Contains(s, "://") && scrubURL(s) != s:
			out[i], changed = scrubURL(s), true
		}
		if t, ok := out[i].(string); ok && scrubText(t) != t {
			out[i], changed = scrubText(t), true
		}
	}
	return out, changed
}

// claudeTop drops what of ~/.claude.json no copy of devvm's keeps: all but claudeTopKeys.
func claudeTop(m map[string]any) map[string]any {
	for k := range m {
		if !claudeTopKeys[k] {
			delete(m, k)
		}
	}
	return m
}

// ClaudeCopy returns the copy of the ~/.claude.json at path that devvm's agent store keeps: only
// claudeTopKeys, so never primaryApiKey or the projects' history, and its MCP servers as they are
// (a VM's copy takes their secrets out: VMCopy).
func ClaudeCopy(path string) ([]byte, error) {
	m, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	return encode("json", claudeTop(m))
}

func claudeVM(m map[string]any, salt string) map[string]any {
	claudeTop(m)
	names, servers := claudeServers(m)
	for _, n := range names {
		s := obj(servers, n)
		if s == nil {
			delete(servers, n)
			continue
		}
		for k := range s {
			if !claudeServerKeys[k] {
				delete(s, k)
			}
		}
		if o := obj(s, "oauth"); o != nil {
			for k := range o {
				if k != "clientId" && k != "callbackPort" {
					delete(o, k)
				}
			}
		}
		if u, ok := s["url"].(string); ok {
			s["url"] = scrubURL(u)
		}
		for _, k := range []string{"command", "headersHelper"} {
			if c, ok := s[k].(string); ok {
				s[k] = scrubCommand(c)
			}
		}
		if a, ok := s["args"]; ok {
			s["args"], _ = scrubArgs(a)
		}
		for k := range obj(s, "env") {
			obj(s, "env")[k] = "devvm-redacted"
		}
		https := claudeHost(s) != ""
		for _, h := range sortedKeys(obj(s, "headers")) {
			if https {
				obj(s, "headers")[h] = salted("devvm-proxy-managed-"+claudeBindingName(n, h), salt)
			} else {
				obj(s, "headers")[h] = "devvm-redacted"
			}
		}
	}
	return m
}

func (f File) claudeGet(name string) (string, error) {
	m, err := readJSON(f.Path)
	if err != nil {
		return "", err
	}
	names, servers := claudeServers(m)
	for _, n := range names {
		hs := obj(obj(servers, n), "headers")
		for _, h := range sortedKeys(hs) {
			if claudeBindingName(n, h) == name {
				return str(hs, h), nil
			}
		}
	}
	return "", fmt.Errorf("no MCP server header %s in %s", name, f.Path)
}

// Bindings returns the login's secret bindings, of proxy.KindLogin with f.Kind their Agent, each
// placeholder ending in f.Salt; none for a file that holds no login devvm knows.
func (f File) Bindings() ([]proxy.Binding, error) {
	m, err := readJSON(f.Path)
	if err != nil {
		return nil, err
	}
	fm, err := f.formatOf()
	if err != nil {
		return nil, err
	}
	return fm.bindings(m, f.Salt), nil
}

// VMFile is the VM's copy of an agent's file (File.VMCopy).
type VMFile struct {
	Data []byte
	// Kept: what of the file the copy holds as it is, sorted: Codex's auth.json keys, or
	// opencode's or pi's providers that File.Pass lists.
	Kept []string
	// Withheld: every opencode or pi login devvm neither binds nor passes, sorted, which the
	// copy holds without their keys and tokens.
	Withheld []string
}

// VMCopy returns the VM's copy of the file, with placeholders ending in f.Salt in place of its keys
// and tokens.
func (f File) VMCopy() (VMFile, error) {
	m, err := readJSON(f.Path)
	if err != nil {
		return VMFile{}, err
	}
	fm, err := f.formatOf()
	if err != nil {
		return VMFile{}, err
	}
	m, c := fm.vmCopy(m, f)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return VMFile{}, err
	}
	c.Data = b
	c.Data = append(c.Data, '\n')
	return c, nil
}

// Get returns the value of the binding named name: from the host's file, read each time, refreshing
// an OAuth token there when it's due, under the file's lock.
func (f File) Get(ctx context.Context, name string) (string, error) {
	fm, err := f.formatOf()
	if err != nil {
		return "", err
	}
	return fm.get(ctx, f, name)
}
