package proxy

// Capturing logins: an agent's OAuth token endpoint (Config.Captures) is intercepted on :443 as a
// host with a bound secret is, whatever the VM's tier. The child judges a request of its method
// (POST, or GET) to its path by the VM's rules as any other, reads it whole (at most
// captureRequestMax) and hands it to the broker (captureToken), which does the rest itself
// (exchanger), so a login's real tokens never enter the child: it hands the request to IO.Capture,
// devvm's store of agents' logins, which may put a login's real value in place of the VM's
// placeholder of it (Codex's API-key exchange sends its id_token), and sends it to the endpoint's
// host from its own config, on port 443. A POST goes on without its query; a GET, whose request is
// its query, goes on with the query IO.Capture saw, canonical, and no body. The endpoint's reply,
// when it's a success, is read whole too (at most captureReplyMax) and never passed on: IO.Capture
// saves the tokens in it on the host, in the agent's own login file, and gives the reply the VM
// gets, with placeholders in their place and no refresh token, and only headers that can't hold a
// credential (keepHeaders). A reply it can't read is refused (502), and so is one that's too long
// or encoded. An error reply holds no tokens and passes as it is, up to captureReplyMax, except to
// a request IO.Capture put a real value in, which gets only its error code. Any other successful
// JSON reply from a token endpoint's host is refused when it holds a login's tokens (guard), so no
// other path there hands the VM a login; on a host that serves an API beside the endpoint, only
// the paths below the endpoint's Guard. Neither the bodies nor anything in them is logged: the
// event log gets a "login" event naming the agent, the proxy's own log only why a capture failed.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/vmname"
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
	VM          vmname.VM
	Agents      []string
	Method      string
	UserAgent   string
	ContentType string
	Body        []byte
	Query       string
}

// Capturer is IO.Capture: devvm's store of agents' logins, in the broker's process.
type Capturer interface {
	// Request returns the token request's body as it goes to the endpoint, or answer, the reply
	// the VM gets with the request going nowhere, or an error that refuses it.
	Request(ctx context.Context, x Exchange) (out, answer []byte, err error)
	// Response saves the login in reply, the endpoint's success, on the host, and returns the
	// reply the VM gets in its place and the agent whose login it was. An error refuses the reply,
	// but with a reply for the VM it's only logged: the login was saved.
	Response(ctx context.Context, x Exchange, reply []byte) (vm []byte, agent string, err error)
}

// What of a token request and of its reply the proxy reads: a request is a few fields and
// headers, a reply a few tokens (agentlogin reads at most as much of a refresh's).
const (
	captureRequestMax = 64 << 10
	captureReplyMax   = 1 << 20
)

// errCaptureRefused is a capture's error that reaches the VM as a 502, logged as a refused block.
var errCaptureRefused = errors.New("devvm refused the login's reply")

// captureReads is how many requests and replies to and from token endpoints' hosts the child
// reads whole at once, each at most captureReplyMax, and how many token exchanges the broker runs
// at once: a login takes three.
const captureReads = 8

// captures is the child's token endpoints (Config.Captures), a slot for each read whole at once
// (captureReads), which a token exchange holds throughout, and token, which exchanges a captured
// request at the broker.
type captures struct {
	list  []Capture
	reads chan struct{}
	token func(ctx context.Context, r tokenRequest) (tokenReply, error)
}

func newCaptures(list []Capture, token func(ctx context.Context, r tokenRequest) (tokenReply, error)) *captures {
	return &captures{list: list, reads: make(chan struct{}, captureReads), token: token}
}

// read reads r whole, at most limit, in one of the reads' slots, waiting for one at most until ctx
// ends.
func (cs *captures) read(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	release, err := cs.slot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return readMax(r, limit)
}

// slot takes one of the reads' slots, waiting for one at most until ctx ends.
func (cs *captures) slot(ctx context.Context) (func(), error) {
	select {
	case cs.reads <- struct{}{}:
		return func() { <-cs.reads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

// agents returns the agents whose token endpoint r, a request for host, is (endpointAgents), and
// the endpoint's own path, which the request goes to. None: it isn't one.
func (cs *captures) agents(host string, r *http.Request) ([]string, string) {
	return endpointAgents(cs.list, host, r.Method, r.URL.Path)
}

// endpointAgents returns the agents whose token endpoint in list a request of method for host's
// path p is: its method to its path, compared once cleaned (cleanPath) and in any case, so another
// spelling of the path (/oauth//token, /OAuth/Token/, /oauth/token;x) never passes uncaptured, and
// the endpoint's own path. None: it isn't one.
func endpointAgents(list []Capture, host, method, p string) ([]string, string) {
	p = cleanPath(p)
	var out []string
	var at string
	for _, c := range list {
		if c.Host == host && method == c.method() && strings.EqualFold(p, c.Path) && !slices.Contains(out, c.Agent) {
			out, at = append(out, c.Agent), c.Path
		}
	}
	return out, at
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

// no1xx is the ResponseWriter of a request to a token endpoint's host: it drops the informational
// replies (1xx) that ReverseProxy passes on before ModifyResponse sees the reply, as their headers
// could hold a credential (103's Set-Cookie).
type no1xx struct{ http.ResponseWriter }

func (w no1xx) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w no1xx) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// keptHeaderMax bounds each header keepHeaders keeps: its first value, at most 1 KiB, as no
// endpoint's Content-Type or Date is longer, so a captured reply always fits in the broker's
// frame to the child.
const keptHeaderMax = 1 << 10

// keepHeaders drops every header of h, a captured reply's, but the few that can't hold a
// credential, as Set-Cookie could, each its first value, dropped past keptHeaderMax.
func keepHeaders(h http.Header) {
	keep := []string{"Content-Type", "Cache-Control", "Pragma", "Date"}
	for k, vs := range h {
		switch {
		case !slices.Contains(keep, k), len(vs) == 0, len(vs[0]) > keptHeaderMax:
			h.Del(k)
		default:
			h[k] = vs[:1]
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

// --- the child's side ---

// tokenRequest is a captured token request as the child hands it to the broker: its host, its
// endpoint's clean path, its method, its query as the VM sent it, its headers and its body.
type tokenRequest struct {
	host, path, method, query string
	header                    http.Header
	body                      []byte
}

// tokenReply is what the VM gets for a captured token request: its status, its few headers and
// its body, and the agent whose login the broker saved ("" for none). sent: the request went to
// the endpoint, rather than IO.Capture answering it.
type tokenReply struct {
	status int
	header http.Header
	body   []byte
	agent  string
	sent   bool
}

// headerSize is how many bytes h's names and values take.
func headerSize(h http.Header) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v)
		}
	}
	return n
}

// captureToken reads r, a token request for the endpoint at at on host, whole (at most
// captureRequestMax, its headers as much) and has the broker exchange it (captures.token), in one
// of the reads' slots throughout, so the child runs at most captureReads at once, as the broker
// allows. It answers with the broker's reply, logging the request as forwarded when it went to the
// endpoint and the login when one was saved, or refuses it (refuseCapture).
func (s *server) captureToken(w http.ResponseWriter, r *http.Request, host string, port int, at string) {
	// No one request's end cancels an exchange: its 20 seconds run whatever happens to the VM's
	// request, as the broker's do.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()
	release, err := s.captures.slot(ctx)
	if err != nil {
		s.refuseCapture(w, r, host, port, err)
		return
	}
	defer release()
	body, err := readMax(r.Body, captureRequestMax)
	_ = r.Body.Close()
	if err == nil && headerSize(r.Header) > captureRequestMax {
		err = errors.New("the token request's headers are too long")
	}
	var reply tokenReply
	if err == nil {
		reply, err = s.captures.token(ctx, tokenRequest{
			host: host, path: at, method: r.Method, query: r.URL.RawQuery, header: r.Header, body: body,
		})
	}
	if err != nil {
		s.refuseCapture(w, r, host, port, err)
		return
	}
	if reply.sent {
		e := Event{
			Event: "forward", Host: host, Port: port, Method: r.Method, Path: r.URL.Path, Agent: r.UserAgent(),
			Rule: ruleOf(r.Context()), Via: viaOf(r.Context()),
		}
		if up := s.egress.up.route(host, port, false); up != nil {
			e.Upstream = up.addr
		}
		s.events.record(e)
	}
	for k, vs := range reply.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(reply.body)))
	w.WriteHeader(reply.status)
	_, _ = w.Write(reply.body)
	if reply.agent != "" {
		s.events.record(Event{Event: "login", Host: host, Port: port, Method: r.Method, Path: r.URL.Path, Login: reply.agent})
	}
}

// captureResponse is the forwarder's ModifyResponse for a reply from host: any reply from a token
// endpoint's host (guard), or one of Cursor's (cursorReply). Its error refuses the reply
// (errCaptureRefused).
func (s *server) captureResponse(host string, _ int) func(*http.Response) error {
	return func(resp *http.Response) error {
		switch {
		case (s.captures.has(host) || cursorHost(host)) && resp.StatusCode == http.StatusSwitchingProtocols:
			// An upgrade would splice the host's bytes to the VM unread, on any of its paths.
			_ = resp.Body.Close()
			return fmt.Errorf("%w: an upgrade from a token endpoint's host", errCaptureRefused)
		case s.captures.guards(host, resp.Request):
			return s.guard(resp, host)
		case cursorHost(host):
			return s.cursorReply(resp)
		}
		return nil
	}
}

// --- the broker's side ---

// exchanger is the broker's side of captures: its VM's token endpoints (Config.Captures), its
// Capturer (IO.Capture), and the client it sends token requests with, to an endpoint's host from
// its own config on port 443, through the company proxy when there is one (route), trusting the
// system's roots and the user's CAs (roots), as an OAuth refresh does.
type exchanger struct {
	vm   vmname.VM
	list []Capture
	c    Capturer
	log  *log.Logger
	// route is the company proxy (upstreams.host); nil: direct.
	route func(*http.Request) (*url.URL, error)
	roots *x509.CertPool
	// dial connects to an address: a net.Dialer's, which tests replace to reach an endpoint of
	// their own.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// exchange runs a captured token request r: it finds the endpoints in the broker's own list that
// its host, its path and its method match, refusing one that matches none, has IO.Capture read it
// (with the VM's name itself), sends it to the endpoint and returns what the VM gets, or an
// error, which refuses it (502).
func (x *exchanger) exchange(ctx context.Context, r tokenRequest) (tokenReply, error) {
	agents, at := endpointAgents(x.list, r.host, r.method, r.path)
	if len(agents) == 0 || len(r.body) > captureRequestMax || headerSize(r.header) > captureRequestMax {
		return tokenReply{}, fmt.Errorf("not one of the VM's token endpoints: %w", errNotFound)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupTimeout)
	defer cancel()
	ex := Exchange{
		VM: x.vm, Agents: agents, Method: r.method, UserAgent: r.header.Get("User-Agent"),
		ContentType: r.header.Get("Content-Type"), Body: r.body,
	}
	get := r.method == http.MethodGet
	var err error
	switch {
	case get && len(r.body) > 0:
		err = errors.New("a GET token request with a body")
	case get:
		ex.Query, err = captureQuery(r.query)
	}
	var out, answer []byte
	if err == nil {
		out, answer, err = x.c.Request(ctx, ex)
	}
	if err == nil && get && len(out) > 0 {
		err = errors.New("a GET token request given a body")
	}
	switch {
	case err != nil:
		return tokenReply{}, err
	case answer != nil:
		return tokenReply{status: http.StatusOK, header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, body: answer}, nil
	}
	resp, err := x.send(ctx, r, at, ex.Query, out)
	if err != nil {
		return tokenReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := x.reply(ctx, resp, ex, !bytes.Equal(out, r.body))
	reply.sent = true
	return reply, err
}

// send sends a token request to the endpoint at at on r's host, port 443, its query a GET's
// alone, its body out, with the VM's headers but the hop's own, following no
// redirect.
func (x *exchanger) send(ctx context.Context, r tokenRequest, at, query string, out []byte) (*http.Response, error) {
	u := &url.URL{Scheme: "https", Host: r.host, Path: at, RawQuery: query}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	req.Header = r.header.Clone()
	for _, k := range req.Header.Values("Connection") {
		for f := range strings.SplitSeq(k, ",") {
			req.Header.Del(strings.TrimSpace(f))
		}
	}
	// The headers of one hop alone, which a token request never takes on, with
	// Proxy-Authorization, the VM's password, and Accept-Encoding, so Go's transport asks for gzip
	// and undoes it.
	for _, k := range []string{
		"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Accept-Encoding", "Content-Length", "Host",
	} {
		req.Header.Del(k)
	}
	dial := x.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	tr := &http.Transport{
		Proxy: x.route, DialContext: dial, TLSClientConfig: &tls.Config{RootCAs: x.roots},
		ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		x.log.Printf("%s: login: %v", r.host, err)
		return nil, errors.New("the token endpoint couldn't be reached")
	}
	return resp, nil
}

// reply is what the VM gets for resp, the endpoint's reply to ex: a success read whole, saved by
// IO.Capture and replaced with what it gives the VM; an error reply as it is, or, to a request
// IO.Capture put a real value in (rewritten), its OAuth error code alone (errorReply); each with
// keepHeaders' headers alone. An upgrade, an encoded success and one past captureReplyMax are
// refused.
func (x *exchanger) reply(ctx context.Context, resp *http.Response, ex Exchange, rewritten bool) (tokenReply, error) {
	switch {
	case resp.StatusCode < 200:
		return tokenReply{}, fmt.Errorf("%w: an upgrade from a token endpoint's host", errCaptureRefused)
	case resp.StatusCode <= 299:
		if encoded(resp) {
			return tokenReply{}, fmt.Errorf("%w: it's encoded (%s)", errCaptureRefused, strconv.Quote(resp.Header.Get("Content-Encoding")))
		}
		b, err := readMax(resp.Body, captureReplyMax)
		if err != nil {
			return tokenReply{}, fmt.Errorf("%w: %w", errCaptureRefused, err)
		}
		out, agent, err := x.c.Response(ctx, ex, b)
		switch {
		case err != nil && out == nil:
			return tokenReply{}, fmt.Errorf("%w: %w", errCaptureRefused, err)
		case err != nil: // saved, but the VM's bindings weren't written: they are at its next launch
			x.log.Printf("%s: login: %v", resp.Request.URL.Host, err)
		}
		keepHeaders(resp.Header)
		return tokenReply{status: resp.StatusCode, header: resp.Header, body: out, agent: agent}, nil
	case rewritten:
		// An error to a request that carried a real value: only its error code, in case the
		// endpoint's message quotes the request.
		b, err := readMax(resp.Body, captureReplyMax)
		keepHeaders(resp.Header)
		resp.Header.Set("Content-Type", "application/json")
		return tokenReply{status: resp.StatusCode, header: resp.Header, body: errorReply(b, err == nil && !encoded(resp))}, nil
	}
	b, err := readMax(resp.Body, captureReplyMax)
	if err != nil {
		return tokenReply{}, fmt.Errorf("%w: an error reply: %w", errCaptureRefused, err)
	}
	keepHeaders(resp.Header)
	return tokenReply{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// errorCode is an OAuth error's code as the VM may see it.
var errorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// errorReply is an error reply's body b as the VM gets it, {"error":"..."}: its OAuth error code
// alone, or devvm_refused when it has none devvm reads (read: b is the whole body, unencoded).
func errorReply(b []byte, read bool) []byte {
	var e struct {
		Error string `json:"error"`
	}
	if !read || json.Unmarshal(b, &e) != nil || !errorCode.MatchString(e.Error) {
		e.Error = "devvm_refused"
	}
	out, _ := json.Marshal(e)
	return out
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
