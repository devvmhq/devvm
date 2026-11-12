package proxy

// Capturing logins: an agent's OAuth token endpoint (Config.Captures) is intercepted on :443 as a
// host with a bound secret is, whatever the VM's tier. A request of its method (POST, or GET) to
// its path is read whole (at most captureRequestMax) and handed to IO.Capture, devvm's store of
// agents' logins in this process, which may put a login's real value in place of the VM's
// placeholder of it (Codex's API-key exchange sends its id_token). A POST goes on without its
// query; a GET, whose request is its query, goes on with the query IO.Capture saw, canonical, and
// no body. The endpoint's reply, when it's a success, is read whole too (at
// most captureReplyMax) and never passed on: IO.Capture saves the tokens in it on the host, in the
// agent's own login file, and gives the reply the VM gets, with placeholders in their place and no
// refresh token, and only headers that can't hold a credential (keepHeaders). A reply it can't
// read is refused (502), and so is one that's too long or encoded. An error reply holds no tokens
// and passes as it is, except to a request IO.Capture put a real value in, which gets only its
// error code. Any other successful JSON reply from a token endpoint's host is refused when it
// holds a login's tokens (guard), so no other path there hands the VM a login; on a host that
// serves an API beside the endpoint, only the paths below the endpoint's Guard. Neither the bodies
// nor anything in them is logged: the event log gets a "login" event naming the agent, the proxy's
// own log only why a capture failed.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Capture is one of an agent's OAuth token endpoints (Config.Captures): the agent whose login its
// replies hold, the host, and the path a token request goes to. agentlogin's Endpoints are every
// agent's.
type Capture struct {
	Agent string `json:"agent"`
	Host  string `json:"host"`
	Path  string `json:"path"`
	// Method is the token request's method, POST or GET: POST when empty. A GET's request is its
	// query, which goes on as IO.Capture saw it; a POST's query never goes on.
	Method string `json:"method,omitempty"`
	// Guard is the path below which guard reads the host's replies, starting and ending with /,
	// for a host that serves an API of its own beside the token endpoint, which guard would break
	// (its streams, its encodings, its long replies): every path of the host when empty. The
	// endpoint's path is below it.
	Guard string `json:"guard,omitempty"`
	// TokenKeys are the fields that hold a login's tokens in the agent's replies beside OAuth's
	// (access_token, refresh_token, id_token), such as accessToken, which guard refuses too.
	TokenKeys []string `json:"token_keys,omitempty"`
}

// method is c's token request's method.
func (c Capture) method() string { return cmp.Or(c.Method, http.MethodPost) }

// guards reports whether guard reads c's host's reply to a request for p, a clean path.
func (c Capture) guards(p string) bool {
	return c.Guard == "" || strings.HasPrefix(strings.ToLower(p+"/"), strings.ToLower(c.Guard))
}

// Exchange is one token request the proxy captures the reply to: the VM's, for the agents whose
// endpoint it went to (the VM's agents can share one: Codex and opencode both log in to OpenAI),
// with its method, User-Agent, Content-Type and body as the VM sent them, and for a GET its query
// as it goes on (canonical: its fields sorted, each once, encoded as url.Values does).
type Exchange struct {
	VM          string
	Agents      []string
	Method      string
	UserAgent   string
	ContentType string
	Body        []byte
	Query       string
}

// Capturer is IO.Capture: devvm's store of agents' logins, in this process.
type Capturer interface {
	// Request returns the token request's body as it goes to the endpoint, or answer, the reply
	// the VM gets with the request going nowhere, or an error that refuses it.
	Request(ctx context.Context, x Exchange) (out, answer []byte, err error)
	// Response saves the login in reply, the endpoint's success, on the host, and returns the
	// reply the VM gets in its place and the agent whose login it was. An error refuses the reply,
	// but with a reply for the VM it's only logged: the login was saved.
	Response(ctx context.Context, x Exchange, reply []byte) (vm []byte, agent string, err error)
}

// What of a token request and of its reply the proxy reads: a request is a few fields, a reply a
// few tokens (agentlogin reads at most as much of a refresh's).
const (
	captureRequestMax = 64 << 10
	captureReplyMax   = 1 << 20
)

// errCaptureRefused is a capture's error that reaches the VM as a 502, logged as a refused block.
var errCaptureRefused = errors.New("devvm refused the login's reply")

// captureReads is how many requests and replies to and from token endpoints' hosts the proxy reads
// whole at once, each at most captureReplyMax: a login takes three.
const captureReads = 8

// captures is a proxy's token endpoints (Config.Captures), its Capturer (IO.Capture), and a slot
// for each read whole at once (captureReads).
type captures struct {
	list  []Capture
	c     Capturer
	reads chan struct{}
}

func newCaptures(list []Capture, c Capturer) *captures {
	return &captures{list: list, c: c, reads: make(chan struct{}, captureReads)}
}

// read reads r whole, at most limit, in one of the reads' slots, waiting for one at most until ctx
// ends.
func (cs *captures) read(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	select {
	case cs.reads <- struct{}{}:
		defer func() { <-cs.reads }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return readMax(r, limit)
}

// has reports whether host has a token endpoint: the proxy intercepts it.
func (cs *captures) has(host string) bool {
	return slices.ContainsFunc(cs.list, func(c Capture) bool { return c.Host == host })
}

// guards reports whether guard reads host's reply to r: when host has a token endpoint whose
// Guard r's path, cleaned, is below, or one with a Guard and r's path is odd (oddPath), since the
// host could read it as below the Guard where devvm doesn't. The proxy then drops the VM's
// Accept-Encoding, so guard can read what it passes. Informational replies and upgrades are
// another matter, refused on every path of the host (captures.has).
func (cs *captures) guards(host string, r *http.Request) bool {
	p, odd := cleanPath(r.URL.Path), oddPath(r.URL)
	return slices.ContainsFunc(cs.list, func(c Capture) bool { return c.Host == host && (odd || c.guards(p)) })
}

// oddPath reports whether u's path isn't one devvm reads as every host does: cleaning it changes
// it (cleanPath), or as sent it has an escape (%), a byte past ASCII's printable ones, path
// parameters, a backslash, or a segment that ends in a dot or a space, which some servers drop.
func oddPath(u *url.URL) bool {
	p, raw := u.Path, u.EscapedPath()
	if cleanPath(p) != p || strings.ContainsAny(raw, `%;\`) || strings.ContainsFunc(raw, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return true
	}
	return slices.ContainsFunc(strings.Split(p, "/"), func(s string) bool { return strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") })
}

// tokenKeys are the fields guard refuses in a reply from host: OAuth's, and its token endpoints'
// TokenKeys.
func (cs *captures) tokenKeys(host string) []string {
	keys := slices.Clone(oauthTokenKeys)
	for _, c := range cs.list {
		if c.Host == host {
			keys = append(keys, c.TokenKeys...)
		}
	}
	return keys
}

// cleanPath is a request's path as the proxy compares it with a token endpoint's: each segment
// without its path parameters (;x), a backslash read as a slash, cleaned, so another spelling
// (/oauth//token, /x/../oauth/token, /oauth/token;x, /x;/../oauth/token, /x\..\oauth/token) is the
// same path, as a host that normalizes them reads it.
func cleanPath(p string) string {
	segs := strings.Split(strings.ReplaceAll(p, `\`, "/"), "/")
	for i, s := range segs {
		segs[i], _, _ = strings.Cut(s, ";")
	}
	return path.Clean("/" + strings.Join(segs, "/"))
}

// agents returns the agents whose token endpoint r, a request for host, is: its method to its
// path, compared once cleaned (cleanPath) and in any case, so another spelling of the path
// (/oauth//token, /OAuth/Token/, /oauth/token;x) never passes uncaptured, and the endpoint's own
// path, which the request goes to. None: it isn't one.
func (cs *captures) agents(host string, r *http.Request) ([]string, string) {
	p := cleanPath(r.URL.Path)
	var out []string
	var at string
	for _, c := range cs.list {
		if c.Host == host && r.Method == c.method() && strings.EqualFold(p, c.Path) && !slices.Contains(out, c.Agent) {
			out, at = append(out, c.Agent), c.Path
		}
	}
	return out, at
}

// capturing is a captured request as its reply needs it: its Exchange, the endpoint's path it
// goes to, whatever the VM's spelling, and whether IO.Capture changed its body, putting a real
// value in.
type capturing struct {
	x         Exchange
	path      string
	rewritten bool
}

// captureQuery is a captured GET's query as it goes on, canonical (url.Values' encoding: its
// fields sorted), so the endpoint reads what IO.Capture read. A query that isn't a form, or that
// names a field twice, which the endpoint and devvm could read apart, is refused.
func captureQuery(raw string) (string, error) {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return "", errors.New("the token request's query isn't a form")
	}
	for _, vs := range v {
		if len(vs) > 1 {
			return "", errors.New("the token request's query repeats a field")
		}
	}
	return v.Encode(), nil
}

// no1xx is a captured request's ResponseWriter: it drops the informational replies (1xx) that
// ReverseProxy passes on before ModifyResponse sees the reply, as their headers could hold a
// credential (103's Set-Cookie).
type no1xx struct{ http.ResponseWriter }

func (w no1xx) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w no1xx) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// captureKey is the context key of a captured request's capturing.
type captureKey struct{}

// captured is the capturing of a request the proxy captures the reply to, or nil.
func captured(ctx context.Context) *capturing {
	c, _ := ctx.Value(captureKey{}).(*capturing)
	return c
}

// keepHeaders drops every header of h, a captured reply's, but the few that can't hold a
// credential, as Set-Cookie could.
func keepHeaders(h http.Header) {
	keep := []string{"Content-Type", "Cache-Control", "Pragma", "Date"}
	for k := range h {
		if !slices.Contains(keep, k) {
			h.Del(k)
		}
	}
}

// setBody makes b resp's whole body, with its own Content-Length.
func setBody(resp *http.Response, b []byte) {
	resp.Body, resp.ContentLength, resp.TransferEncoding = io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
}

// encoded reports whether a reply's body is encoded: the request went without the VM's
// Accept-Encoding, so Go's transport undid any gzip.
func encoded(resp *http.Response) bool {
	ce := resp.Header.Get("Content-Encoding")
	return ce != "" && !strings.EqualFold(ce, "identity")
}

// errBodyTooLong is readMax's error for a body past its cap.
var errBodyTooLong = errors.New("too long")

// readMax reads r whole when it's at most limit bytes: the proxy's fetch.ReadCapped.
func readMax(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errBodyTooLong
	}
	return b, nil
}

// captureRequest reads r, a token request for agents' endpoint at path, and returns it as it goes
// on: its body as IO.Capture gives it, a GET's query canonical (captureQuery), its capturing in
// its context. It refuses one that's too long, a GET with a body or a query captureQuery refuses,
// and one that IO.Capture refuses, or whose GET it gives a body, and answers one that IO.Capture
// answers, writing w itself, and reports whether r goes on.
func (s *server) captureRequest(w http.ResponseWriter, r *http.Request, host string, port int, agents []string, at string) (*http.Request, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()
	body, err := s.captures.read(ctx, r.Body, captureRequestMax)
	_ = r.Body.Close()
	x := &Exchange{VM: s.vm, Agents: agents, Method: r.Method, UserAgent: r.UserAgent(), ContentType: r.Header.Get("Content-Type"), Body: body}
	get := r.Method == http.MethodGet
	switch {
	case err == nil && get && len(body) > 0:
		err = errors.New("a GET token request with a body")
	case err == nil && get:
		x.Query, err = captureQuery(r.URL.RawQuery)
	}
	var answer []byte
	if err == nil {
		body, answer, err = s.captures.c.Request(ctx, *x)
	}
	if err == nil && get && len(body) > 0 {
		err = errors.New("a GET token request given a body")
	}
	switch {
	case err != nil:
		s.refuseCapture(w, r, host, port, err)
		return nil, false
	case answer != nil:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(answer)
		return nil, false
	}
	c := &capturing{x: *x, path: at, rewritten: !bytes.Equal(body, x.Body)}
	r = r.WithContext(context.WithValue(r.Context(), captureKey{}, c))
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	r.Header.Del("Content-Length")
	return r, true
}

// captureResponse is the forwarder's ModifyResponse for a reply from host: a captured request's,
// or any reply from a token endpoint's host (guard). Its error refuses the reply
// (errCaptureRefused).
func (s *server) captureResponse(host string, port int) func(*http.Response) error {
	return func(resp *http.Response) error {
		c := captured(resp.Request.Context())
		switch {
		case (c != nil || s.captures.has(host) || cursorHost(host)) && resp.StatusCode == http.StatusSwitchingProtocols:
			// An upgrade would splice the host's bytes to the VM unread, on any of its paths.
			_ = resp.Body.Close()
			return fmt.Errorf("%w: an upgrade from a token endpoint's host", errCaptureRefused)
		case c != nil && resp.StatusCode >= 200 && resp.StatusCode <= 299:
			return s.captureReply(resp, c, host, port)
		case c != nil && c.rewritten:
			// An error to a request that carried a real value: only its error code, in case the
			// endpoint's message quotes the request.
			return s.errorReply(resp)
		case c != nil:
			keepHeaders(resp.Header)
		case s.captures.guards(host, resp.Request):
			return s.guard(resp, host)
		case cursorHost(host):
			return s.cursorReply(resp)
		}
		return nil
	}
}

// captureReply reads a captured request's successful reply whole, has IO.Capture save it and
// replaces it with what IO.Capture gives the VM, with keepHeaders' headers alone, and logs the
// login.
func (s *server) captureReply(resp *http.Response, c *capturing, host string, port int) error {
	defer func() { _ = resp.Body.Close() }()
	if encoded(resp) {
		return fmt.Errorf("%w: it's encoded (%s)", errCaptureRefused, strconv.Quote(resp.Header.Get("Content-Encoding")))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(resp.Request.Context()), lookupTimeout)
	defer cancel()
	reply, err := s.captures.read(ctx, resp.Body, captureReplyMax)
	if err != nil {
		return fmt.Errorf("%w: %w", errCaptureRefused, err)
	}
	out, agent, err := s.captures.c.Response(ctx, c.x, reply)
	switch {
	case err != nil && out == nil:
		return fmt.Errorf("%w: %w", errCaptureRefused, err)
	case err != nil: // saved, but the VM's bindings weren't written: they are at its next launch
		s.log.Printf("%s:%d: login: %v", host, port, err)
	}
	keepHeaders(resp.Header)
	setBody(resp, out)
	s.events.record(Event{Event: "login", Host: host, Port: port, Method: resp.Request.Method, Path: resp.Request.URL.Path, Login: agent})
	return nil
}

// errorCode is an OAuth error's code as the VM may see it.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// errorReply replaces an error reply's body with its OAuth error code alone, {"error":"..."},
// or devvm_refused when it has none devvm reads, and keeps keepHeaders' headers alone.
func (s *server) errorReply(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	var e struct {
		Error string `json:"error"`
	}
	if b, err := s.captures.read(resp.Request.Context(), resp.Body, captureReplyMax); encoded(resp) || err != nil || json.Unmarshal(b, &e) != nil || !errorCode.MatchString(e.Error) {
		e.Error = "devvm_refused"
	}
	b, _ := json.Marshal(e)
	keepHeaders(resp.Header)
	resp.Header.Set("Content-Type", "application/json")
	setBody(resp, b)
	return nil
}

// page reports whether a reply's Content-Type is a web page's or its parts' (HTML, CSS,
// JavaScript, images, fonts), which a browser in the VM loads from a token endpoint's host as it
// logs in, and which guard passes unread.
func page(ctype string) bool {
	mt, _, _ := mime.ParseMediaType(ctype)
	return mt == "text/html" || mt == "text/css" || strings.Contains(mt, "javascript") ||
		strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "font/")
}

// guard refuses a successful reply from a token endpoint's host to a request the proxy didn't
// capture, such as another token endpoint's there, when it names a field that holds a login's
// tokens anywhere (tokenKeys), whatever its type or shape: they reach the VM only as
// placeholders. A web page's passes unread, and one too long or encoded to read is refused. Any
// other reply passes as it was. On a host whose token endpoints have a Guard, it reads only the
// replies below it (captures.guards).
func (s *server) guard(resp *http.Response, host string) error {
	// On a host whose endpoint has a Guard, a reply below it, whatever its status, sets no cookie,
	// and on any token endpoint's host, a redirect's Location holds no token either.
	if s.captures.scoped(host) {
		resp.Header.Del("Set-Cookie")
	}
	if s.captures.holdsToken(host, []byte(resp.Header.Get("Location"))) {
		_ = resp.Body.Close()
		return fmt.Errorf("%w: a login's tokens in a redirect", errCaptureRefused)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || page(resp.Header.Get("Content-Type")) {
		return nil
	}
	if encoded(resp) {
		_ = resp.Body.Close()
		return fmt.Errorf("%w: an encoded reply from a token endpoint's host", errCaptureRefused)
	}
	b, err := s.captures.read(resp.Request.Context(), resp.Body, captureReplyMax)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("%w: a reply from a token endpoint's host: %w", errCaptureRefused, err)
	}
	if s.captures.holdsToken(host, b) {
		return fmt.Errorf("%w: a login's tokens from a path that isn't its endpoint's", errCaptureRefused)
	}
	setBody(resp, b)
	return nil
}

// oauthTokenKeys are the fields of an OAuth token reply that hold a login's tokens.
var oauthTokenKeys = []string{"access_token", "refresh_token", "id_token"}

// jwtShape is a JWT, a token's usual shape whatever its field is named: two base64url parts of
// JSON objects ({" is eyJ) and a dot after each.
var jwtShape = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.`)

// holdsToken reports whether b, a reply's body or Location from host, holds a login's tokens:
// a field that tokenKeys names, or on a host whose endpoints name token keys of their own, a JWT
// by its shape, as such an agent's tokens can come under other names.
func (cs *captures) holdsToken(host string, b []byte) bool {
	for _, k := range cs.tokenKeys(host) {
		if bytes.Contains(b, []byte(k)) {
			return true
		}
	}
	return len(cs.tokenKeys(host)) > len(oauthTokenKeys) && jwtShape.Match(b)
}

// scoped reports whether one of host's token endpoints has a Guard: the host serves an API of
// its own beside it.
func (cs *captures) scoped(host string) bool {
	return slices.ContainsFunc(cs.list, func(c Capture) bool { return c.Host == host && c.Guard != "" })
}

// refuseCapture answers a token request, or its reply, that a capture refused (err, which names
// no value): 502, logged as a refused block, and why in the proxy's own log.
func (s *server) refuseCapture(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	s.log.Printf("%s:%d: login: %v", host, port, err)
	const why = "devvm couldn't read the login's token request or reply, so it went no further"
	s.events.record(Event{Event: "blocked", Host: host, Port: port, Method: r.Method, Path: r.URL.Path, Refused: true, Reason: why})
	http.Error(w, "devvm proxy: "+why, http.StatusBadGateway)
}
