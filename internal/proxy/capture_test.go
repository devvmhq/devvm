package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeCapturer is IO.Capture for a test: a request's PH-ID becomes real-id on its way out, and a
// reply becomes {"access_token":"VM-PH"}, the agent the first of the exchange's; each exchange and
// reply is kept. reqErr and respErr refuse instead.
type fakeCapturer struct {
	mu              sync.Mutex
	got             []Exchange
	replies         []string
	reqErr, respErr error
}

func (f *fakeCapturer) Request(_ context.Context, x Exchange) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, x)
	switch {
	case f.reqErr != nil:
		return nil, nil, f.reqErr
	case bytes.Contains(x.Body, []byte("answer")):
		return nil, []byte(`{"access_token":"VM-PH-ANSWER"}`), nil
	}
	return bytes.ReplaceAll(x.Body, []byte("PH-ID"), []byte("real-id")), nil, nil
}

func (f *fakeCapturer) Response(_ context.Context, x Exchange, reply []byte) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, string(reply))
	switch {
	case f.respErr != nil && bytes.Contains(x.Body, []byte("saved")):
		return []byte(`{"access_token":"VM-PH"}`), x.Agents[0], f.respErr
	case f.respErr != nil:
		return nil, "", f.respErr
	}
	return []byte(`{"access_token":"VM-PH"}`), x.Agents[0], nil
}

func (f *fakeCapturer) counts() (int, int) {
	got, replies := f.seen()
	return len(got), len(replies)
}

// seen is what the capturer got so far: the exchanges and the replies.
func (f *fakeCapturer) seen() ([]Exchange, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got), slices.Clone(f.replies)
}

// tokenEndpoint is a fake token endpoint: what it got (each request's path, body and
// Accept-Encoding), and its reply, JSON with a session cookie: real tokens, gzipped when asked; a
// 400 quoting the request for a body saying fail, no tokens for one saying plain, past
// captureReplyMax for one saying long, and br-encoded for one saying br.
type tokenEndpoint struct {
	mu  sync.Mutex
	got []string
}

func (e *tokenEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	e.got = append(e.got, r.Method+" "+r.URL.RequestURI()+" "+string(b)+" "+r.Header.Get("Accept-Encoding"))
	e.mu.Unlock()
	if strings.Contains(string(b), "early") {
		w.Header().Set("Set-Cookie", "session=real-early-cookie")
		w.WriteHeader(http.StatusEarlyHints)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", "session=real-cookie")
	reply := `{"access_token":"real-at","refresh_token":"real-rt"}`
	switch {
	case strings.Contains(string(b), "text"):
		w.Header().Set("Content-Type", "text/plain")
		reply = "refresh_token=real-rt"
	case strings.Contains(string(b), "fail"):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"got %s"}`, b)
		return
	case strings.Contains(string(b), "plain"):
		reply = `{"ok":true}`
	case strings.Contains(string(b), "long"):
		reply = `{"access_token":"real-at","pad":"` + strings.Repeat("x", captureReplyMax) + `"}`
	case strings.Contains(string(b), "br"):
		w.Header().Set("Content-Encoding", "br")
	case strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"):
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(reply))
		_ = gz.Close()
		return
	}
	_, _ = io.WriteString(w, reply)
}

func (e *tokenEndpoint) requests() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.got)
}

// captureSetup is interceptSetup with a token endpoint for codex and opencode at localhost's
// /oauth/token, and no bindings: the tunnel is intercepted for the endpoint alone.
func captureSetup(t *testing.T) (*server, int, *fakeCapturer, *tokenEndpoint, *http.Client, *syncBuffer) {
	t.Helper()
	te := &tokenEndpoint{}
	s, port, file, c := interceptSetup(t, te.serve)
	writeBindings(t, file)
	fc := &fakeCapturer{}
	s.captures = newCaptures([]Capture{{Agent: "codex", Host: "localhost", Path: "/oauth/token"}, {Agent: "opencode", Host: "localhost", Path: "/oauth/token"}}, fc)
	return s, port, fc, te, c, logs(s)
}

func post(t *testing.T, c *http.Client, method, url, body string) (int, string) {
	t.Helper()
	code, b, _ := postHeaders(t, c, method, url, body)
	return code, b
}

// postHeaders is post, with the reply's headers.
func postHeaders(t *testing.T, c *http.Client, method, url, body string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Bun/1.2")
	req.Header.Set("Accept-Encoding", "br") // the VM's: the proxy asks for what it reads
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.ContentLength >= 0 && resp.ContentLength != int64(len(b)) {
		t.Errorf("Content-Length %d for %d bytes", resp.ContentLength, len(b))
	}
	return resp.StatusCode, string(b), resp.Header
}

// A login at a token endpoint: the request goes out as IO.Capture gives it, without the VM's
// Accept-Encoding, the reply (gzipped, undone) goes to IO.Capture and never to the VM, which gets
// what IO.Capture gave, and the event log says whose login it was, with no value in any log.
func TestCaptureLogin(t *testing.T) {
	t.Parallel()
	s, port, fc, te, c, logged := captureSetup(t)
	code, body, h := postHeaders(t, c, http.MethodPost, fmt.Sprintf("https://localhost:%d/oauth/token", port), "grant_type=authorization_code&subject_token=PH-ID")
	if code != http.StatusOK || body != `{"access_token":"VM-PH"}` || h.Get("Set-Cookie") != "" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("the VM got %d %q %v", code, body, h)
	}
	if got := te.requests(); len(got) != 1 || got[0] != "POST /oauth/token grant_type=authorization_code&subject_token=real-id gzip" {
		t.Errorf("the endpoint got %q", got)
	}
	got, replies := fc.seen()
	if len(got) != 1 || !slices.Equal(got[0].Agents, []string{"codex", "opencode"}) || got[0].VM != s.vm ||
		got[0].UserAgent != "Bun/1.2" || got[0].ContentType != "application/x-www-form-urlencoded" ||
		string(got[0].Body) != "grant_type=authorization_code&subject_token=PH-ID" {
		t.Errorf("IO.Capture got %+v", got)
	}
	if len(replies) != 1 || replies[0] != `{"access_token":"real-at","refresh_token":"real-rt"}` {
		t.Errorf("IO.Capture got the reply %q", replies)
	}
	l := logged.String()
	if !strings.Contains(l, `"event":"login","host":"localhost","port":`+fmt.Sprint(port)+`,"method":"POST","path":"/oauth/token","login":"codex"`) ||
		!strings.Contains(l, `"event":"intercept"`) {
		t.Errorf("the log = %s", l)
	}
	for _, v := range []string{"real-at", "real-rt", "real-id", "VM-PH"} {
		if strings.Contains(l, v) {
			t.Errorf("the log holds %q: %s", v, l)
		}
	}
}

// Another spelling of the endpoint's path is captured too. A GET, or a POST elsewhere, isn't, and
// its reply passes, unless it holds a login's tokens, which only a capture hands the VM.
func TestCapturePaths(t *testing.T) {
	t.Parallel()
	_, port, fc, te, c, _ := captureSetup(t)
	u := fmt.Sprintf("https://localhost:%d", port)
	spellings := []string{"/oauth//token", "/OAuth/Token/?q=1", "/x/../oauth/token", "/oauth/token;x", "/x/%2e%2e/oauth/token"}
	for _, p := range spellings {
		if code, body := post(t, c, http.MethodPost, u+p, "grant_type=refresh_token"); code != http.StatusOK || body != `{"access_token":"VM-PH"}` {
			t.Errorf("POST %s: %d %q", p, code, body)
		}
	}
	// Each went to the endpoint's own path, without a query.
	if got := te.requests(); len(got) != len(spellings) || slices.ContainsFunc(got, func(r string) bool { return !strings.HasPrefix(r, "POST /oauth/token grant_type") }) {
		t.Errorf("the endpoint got %q", got)
	}
	for _, r := range [][2]string{
		{http.MethodGet, "/oauth/token"},
		{http.MethodPost, "/oauth/tokens"},
		{http.MethodPut, "/oauth/token"},
		{http.MethodPost, "/api/oauth/token"},
		{http.MethodPost, "/api/accounts/oauth/token"},
	} { // opencode 2's Sign in with ChatGPT
		for _, body := range []string{"x", "text"} {
			if code, got := post(t, c, r[0], u+r[1], body); code != http.StatusBadGateway || strings.Contains(got, "real-") {
				t.Errorf("%s %s %s: %d %q (tokens not captured: refused)", r[0], r[1], body, code, got)
			}
		}
		if code, body := post(t, c, r[0], u+r[1], "plain"); code != http.StatusOK || body != `{"ok":true}` {
			t.Errorf("%s %s: %d %q (no tokens: as it is)", r[0], r[1], code, body)
		}
	}
	if n, _ := fc.counts(); n != len(spellings) {
		t.Errorf("IO.Capture got %d requests, want %d", n, len(spellings))
	}
}

// A token endpoint's informational replies never reach the VM, as their headers could hold a
// cookie; a request IO.Capture answers goes nowhere; a login saved whose bindings weren't written
// still reaches the VM, and the proxy's log says why.
func TestCaptureEarlyHintsAnswerAndSaved(t *testing.T) {
	t.Parallel()
	s, port, fc, te, c, logged := captureSetup(t)
	u := fmt.Sprintf("https://localhost:%d/oauth/token", port)
	var early []string
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		early = append(early, fmt.Sprint(code, h))
		return nil
	}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("grant_type=refresh_token&early"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(early) != 0 || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("the VM got %d, %q early, %v", resp.StatusCode, early, resp.Header)
	}
	n := len(te.requests())
	if code, body := post(t, c, http.MethodPost, u, "grant_type=refresh_token&answer"); code != http.StatusOK || body != `{"access_token":"VM-PH-ANSWER"}` || len(te.requests()) != n {
		t.Errorf("an answered request: %d %q, the endpoint got %q", code, body, te.requests())
	}
	fc.mu.Lock()
	fc.respErr = errors.New("the bindings weren't written")
	fc.mu.Unlock()
	if code, body := post(t, c, http.MethodPost, u, "grant_type=refresh_token&saved"); code != http.StatusOK || body != `{"access_token":"VM-PH"}` ||
		!strings.Contains(logged.String(), "login: the bindings weren't written") {
		t.Errorf("a saved login: %d %q, log %s", code, body, logged.String())
	}
	_ = s
}

// An error reply holds no tokens: it passes as it is, IO.Capture never asked, unless the request
// carried a real value, when the VM gets the error's code alone, never the endpoint's message,
// which could quote it.
func TestCaptureErrorPasses(t *testing.T) {
	t.Parallel()
	_, port, fc, _, c, _ := captureSetup(t)
	u := fmt.Sprintf("https://localhost:%d/oauth/token", port)
	if code, body := post(t, c, http.MethodPost, u, "grant_type=refresh_token&fail"); code != http.StatusBadRequest ||
		body != `{"error":"invalid_grant","error_description":"got grant_type=refresh_token&fail"}` {
		t.Errorf("the VM got %d %q", code, body)
	}
	code, body, h := postHeaders(t, c, http.MethodPost, u, "grant_type=x&subject_token=PH-ID&fail")
	if code != http.StatusBadRequest || body != `{"error":"invalid_grant"}` || h.Get("Set-Cookie") != "" {
		t.Errorf("a rewritten request's error: %d %q %v", code, body, h)
	}
	if _, n := fc.counts(); n != 0 {
		t.Errorf("IO.Capture got %d replies", n)
	}
}

// What a capture can't read is refused with a 502, logged as a refused block: a request too long
// or that IO.Capture refuses never goes out, and a reply too long, encoded, or that IO.Capture
// refuses never reaches the VM.
//
// (TestCaptureGet, below, is a GET endpoint's, on a host that serves an API beside it.)
func TestCaptureRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body      string
		reqErr, respErr error
		out             bool
	}{
		{"a request too long", strings.Repeat("x", captureRequestMax+1), nil, nil, false},
		{"a request refused", "grant_type=client_credentials", errors.New("a grant devvm doesn't capture"), nil, false},
		{"a reply too long", "long", nil, nil, true},
		{"an encoded reply", "br", nil, nil, true},
		{"a reply refused", "grant_type=authorization_code", nil, errors.New("no login in it"), true},
	} {
		_, port, fc, te, c, logged := captureSetup(t)
		fc.mu.Lock()
		fc.reqErr, fc.respErr = tc.reqErr, tc.respErr
		fc.mu.Unlock()
		code, body := post(t, c, http.MethodPost, fmt.Sprintf("https://localhost:%d/oauth/token", port), tc.body)
		if code != http.StatusBadGateway || strings.Contains(body, "real-") {
			t.Errorf("%s: the VM got %d %q", tc.name, code, body)
		}
		if went := len(te.requests()) > 0; went != tc.out {
			t.Errorf("%s: the request went out: %v", tc.name, went)
		}
		if l := logged.String(); !strings.Contains(l, `"event":"blocked"`) || !strings.Contains(l, `"refused":true`) ||
			!strings.Contains(l, "login: ") || strings.Contains(l, "real-") {
			t.Errorf("%s: the log = %s", tc.name, l)
		}
	}
}

// apiHost is a fake host that serves a token endpoint, GET /auth/poll, beside an API of its own:
// what it got (each request's method, URI, body and Accept-Encoding), and its replies, which all
// hold a login's tokens as its agent names them (accessToken), the API's streamed past
// captureReplyMax and gzipped when asked.
type apiHost struct{ tokenEndpoint }

func (h *apiHost) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.got = append(h.got, r.Method+" "+r.URL.RequestURI()+" "+string(b)+" "+r.Header.Get("Accept-Encoding"))
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/auth/cookie": // no token in its body: a session cookie alone
		w.Header().Set("Set-Cookie", "session=real-cookie")
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	case "/auth/redirect": // a token in its Location
		w.Header().Set("Location", "https://localhost/done#t=eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.real-sig")
		w.WriteHeader(http.StatusFound)
		return
	case "/auth/jwt": // a token under a name of its own
		_, _ = io.WriteString(w, `{"credential":"eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.real-sig"}`)
		return
	}
	if r.URL.Query().Get("uuid") == "pending" { // a login not yet approved, as Cursor answers it
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Not found")
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		_, _ = io.WriteString(w, `{"accessToken":"real-at","refreshToken":"real-rt"}`)
		return
	}
	w.Header().Set("Content-Type", "application/connect+json")
	_, _ = io.WriteString(w, `{"accessToken":"minted","pad":"`+strings.Repeat("x", captureReplyMax)+`"}`)
}

// A GET token endpoint's login: its query goes on canonical, as IO.Capture saw it, with no body,
// and the reply goes to IO.Capture; a query that repeats a field, or a GET with a body, never goes
// out. A POST there isn't captured. On a host whose endpoint has a Guard, guard reads the replies
// below it alone, with the agent's own token keys: the API's elsewhere pass as they are, streamed,
// long and with the VM's Accept-Encoding.
func TestCaptureGet(t *testing.T) {
	t.Parallel()
	h := &apiHost{}
	s, port, file, c := interceptSetup(t, h.serve)
	writeBindings(t, file)
	fc := &fakeCapturer{}
	s.captures = newCaptures([]Capture{{
		Agent: "cursor", Host: "localhost", Path: "/auth/poll", Method: http.MethodGet, Guard: "/auth/", TokenKeys: []string{"accessToken"},
	}}, fc)
	u := fmt.Sprintf("https://localhost:%d", port)
	if code, body := post(t, c, http.MethodGet, u+"/Auth//poll?verifier=v%2Bw&uuid=u-1", ""); code != http.StatusOK || body != `{"access_token":"VM-PH"}` {
		t.Fatalf("the login: %d %q", code, body)
	}
	got, replies := fc.seen()
	if len(got) != 1 || got[0].Method != http.MethodGet || got[0].Query != "uuid=u-1&verifier=v%2Bw" || len(got[0].Body) != 0 ||
		len(replies) != 1 || !strings.Contains(replies[0], "real-at") {
		t.Errorf("IO.Capture got %+v, %q", got, replies)
	}
	// A login not yet approved: Cursor's 404 passes as it is, as the CLI polls on it, and nothing is
	// captured.
	if code, body := post(t, c, http.MethodGet, u+"/auth/poll?uuid=pending&verifier=v", ""); code != http.StatusNotFound || body != "Not found" {
		t.Errorf("a pending login: %d %q", code, body)
	}
	if _, replies := fc.seen(); len(replies) != 1 {
		t.Errorf("a pending login captured: %q", replies)
	}
	for _, tc := range []struct{ method, uri, body string }{
		{http.MethodGet, "/auth/poll?uuid=a&uuid=b", ""},
		{http.MethodGet, "/auth/poll?uuid=%zz", ""},
		{http.MethodGet, "/auth/poll?uuid=a", "smuggled"},
	} {
		if code, body := post(t, c, tc.method, u+tc.uri, tc.body); code != http.StatusBadGateway || strings.Contains(body, "real-") {
			t.Errorf("%s %s %q: %d %q", tc.method, tc.uri, tc.body, code, body)
		}
	}
	// A POST to the endpoint, and another path below the guard: their tokens never reach the VM.
	for _, r := range [][2]string{{http.MethodPost, "/auth/poll"}, {http.MethodGet, "/auth/other"}} {
		if code, body := post(t, c, r[0], u+r[1], ""); code != http.StatusBadGateway || strings.Contains(body, "real-") {
			t.Errorf("%s %s: %d %q", r[0], r[1], code, body)
		}
	}
	// Spellings a host that normalizes paths reads as the endpoint, or as below the guard: each is
	// captured, or guarded, never passed unread.
	for _, p := range []string{"/x;/../auth/poll", "/x%3B/../auth/poll", "/x/.%2e;/auth/poll", `/x\..\auth/poll`, "/x%5C..%5Cauth/poll", "/api/..;/auth/other"} {
		if code, body := post(t, c, http.MethodGet, u+p+"?uuid=s", ""); strings.Contains(body, "real-") {
			t.Errorf("GET %s: %d %q", p, code, body)
		}
	}
	// The API's reply passes as it is.
	if code, body := post(t, c, http.MethodPost, u+"/api/stream", "{}"); code != http.StatusOK || len(body) <= captureReplyMax {
		t.Errorf("the API: %d, %d bytes", code, len(body))
	}
	// Below the guard, a reply sets no cookie, and a token in a redirect or by its shape alone
	// never reaches the VM.
	if code, body, hdr := postHeaders(t, c, http.MethodGet, u+"/auth/cookie", ""); code != http.StatusOK || body != `{"ok":true}` || hdr.Get("Set-Cookie") != "" {
		t.Errorf("/auth/cookie: %d %q %v", code, body, hdr)
	}
	for _, p := range []string{"/auth/redirect", "/auth/jwt"} {
		if code, body := post(t, c, http.MethodGet, u+p, ""); code != http.StatusBadGateway || strings.Contains(body, "real-") {
			t.Errorf("%s: %d %q", p, code, body)
		}
	}
	out := h.requests()
	poll := "GET /auth/poll?uuid=s  gzip" // each spelling of the endpoint, captured
	want := []string{
		"GET /auth/poll?uuid=u-1&verifier=v%2Bw  gzip", "GET /auth/poll?uuid=pending&verifier=v  gzip", "POST /auth/poll  gzip", "GET /auth/other  gzip",
		poll, poll, poll, poll, poll, "GET /api/..;/auth/other?uuid=s  gzip", "POST /api/stream {} br",
		"GET /auth/cookie  gzip", "GET /auth/redirect  gzip", "GET /auth/jwt  gzip",
	}
	if !slices.Equal(out, want) {
		t.Errorf("the host got %q, want %q", out, want)
	}
	if n, _ := fc.counts(); n != 7 { // the login, the pending poll and each spelling
		t.Errorf("IO.Capture got %d requests", n)
	}
}
