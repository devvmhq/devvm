package proxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
)

// Cursor's hosts are its API's, its agent's (api5's, every one of them) and its indexing's.
func TestCursorHost(t *testing.T) {
	for host, want := range map[string]bool{
		"api2.cursor.sh": true, "api5.cursor.sh": true, "agent.api5.cursor.sh": true, "agentn.us.api5.cursor.sh": true,
		"repo42.cursor.sh": true, "api3.cursor.sh": false, "cursor.sh": false, "xapi5.cursor.sh": false,
		"api2.cursor.sh.example.com": false, "localhost": false,
	} {
		if cursorHost(host) != want {
			t.Errorf("cursorHost(%s) = %v", host, !want)
		}
	}
}

// On Cursor's hosts the proxy refuses every request below /auth/ but its login's poll, a GET to
// api2's /auth/poll, which it captures; the RPCs that make a credential; a part of a reply
// (Range); and any path it can't read as the hosts do. The agent's RPCs pass.
func TestCursorMints(t *testing.T) {
	for _, tc := range []struct {
		host, method, path string
		refused            bool
	}{
		{"api2.cursor.sh", "GET", "/auth/poll?uuid=u&verifier=v", false},
		{"api2.cursor.sh", "GET", "/AUTH/Poll", false},
		{"api2.cursor.sh", "POST", "/auth/poll", true},
		{"api2.cursor.sh", "GET", "/auth/exchange_user_api_key", true},
		{"api2.cursor.sh", "POST", "/auth", true},
		{"api5.cursor.sh", "GET", "/auth/poll", true},
		{"agent.api5.cursor.sh", "GET", "/auth/poll?uuid=u&verifier=v", true},
		{"agentn.us.api5.cursor.sh", "POST", "/Auth/x", true},
		{"api2.cursor.sh", "GET", "/x;/../auth/poll", true},
		{"api2.cursor.sh", "GET", "/x/..%2fauth/poll", true},
		{"api2.cursor.sh", "GET", "/auth%2fpoll", true},
		{"api2.cursor.sh", "GET", "/x/../auth/poll", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.ChatService./StreamChat", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.ChatService%20/StreamChat", true},
		{"api2.cursor.sh", "POST", "/aé/StreamChat", true},
		{"api2.cursor.sh", "POST", `/x\auth/poll`, true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AiService/StreamChat", false},
		{"agent.api5.cursor.sh", "POST", "/agent.v1.AgentService/Run", false},
		{"api2.cursor.sh", "GET", "/authx/poll", false},
		{"repo42.cursor.sh", "POST", "/auth/poll", true},
		{"repo42.cursor.sh", "POST", "/aiserver.v1.RepositoryService/SyncMerkleSubtree", false},
		// The RPCs that make or show a credential, by name, and in any service whatever says so;
		// the CLI's own pass.
		{"api2.cursor.sh", "POST", "/aiserver.v1.AiService/MintRealtimeVoiceSecret", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/CreateUserApiKey", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/RotateServiceToken", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/GetMcpOAuthTokens", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.dashboardservice/createteamapikey", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.BackgroundComposerService/KeyringGetSecret", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.BackgroundComposerService/RefreshGithubAccessTokenInBackgroundComposer", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AiService/CreateLoginLink", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AiService/GetSessionCookie", true},
		{"agent.api5.cursor.sh", "POST", "/agent.v1.AgentService/InviteMember", true},
		{"repo42.cursor.sh", "POST", "/aiserver.v1.RepositoryService/GetUploadKey", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/GetMe", false},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/GetUserPrivacyMode", false},
		// Changes to the account that outlast the VM: by name, and by a changing verb in the
		// account's services; reading them passes.
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/InstallUserPlugin", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AutomationsService/ApplyAutomationSpec", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AutomationsService/EnableAutomation", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.BackgroundComposerService/StartBackgroundComposerFromSnapshot", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.BackgroundComposerService/SetTeamEnvironmentJson", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/UpdateTeamMember", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/AddTeamRepo", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AiService/TestBedrockCredentials", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AutomationsService/ListAutomations", false},
		{"api2.cursor.sh", "POST", "/aiserver.v1.BackgroundComposerService/ListBackgroundComposers", false},
		{"api2.cursor.sh", "POST", "/aiserver.v1.DashboardService/SubmitFeedback", false},
		// The agent store, every method of it, on every host, in any case: its token, its reads
		// and its writes.
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/MintAgentStoreToken", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/PresignAgentStoreReads", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/ListAgentStoreDirectory", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/CheckAgentStoreVersions", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/RecordAgentStoreListingVersionAudit", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/PresignAgentStoreWrites", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/CompleteAgentStoreMultipartWrites", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/AbortAgentStoreMultipartWrites", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/DeleteAgentStoreFiles", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/RmdirAgentStore", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/WriteAgentStoreDocument", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/ShareAgentStore", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.agentstoreservice/listagentstoredirectory", true},
		{"agent.api5.cursor.sh", "POST", "/aiserver.v1.AgentStoreService/ListAgentStoreDirectory", true},
		// Any version or package of a store's service, as defence in depth.
		{"api2.cursor.sh", "POST", "/aiserver.v2.AgentStoreService/ListAgentStoreDirectory", true},
		{"api2.cursor.sh", "POST", "/agent.v1.AgentStoreV2Service/Read", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.agentstoresharingservice/List", true},
		{"api2.cursor.sh", "POST", "/aiserver.v1.AgentService/ListAgentStoreDirectory", false},
	} {
		u, err := url.Parse("https://" + tc.host + tc.path)
		if err != nil {
			u = &url.URL{Path: tc.path}
		}
		r := &http.Request{Method: tc.method, URL: u, Header: http.Header{}}
		if got := cursorMints(tc.host, r); (got != "") != tc.refused {
			t.Errorf("cursorMints(%s %s%s) = %q, want refused %v", tc.method, tc.host, tc.path, got, tc.refused)
		}
	}
	for _, p := range cursorMinting {
		if cursorMints(cursorAPI, &http.Request{Method: http.MethodPost, URL: &url.URL{Path: p}, Header: http.Header{}}) == "" {
			t.Errorf("%s passes", p)
		}
	}
	ranged := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/aiserver.v1.AiService/StreamChat"}, Header: http.Header{"Range": {"bytes=10-20"}}}
	if cursorMints(cursorAPI, ranged) == "" {
		t.Error("a Range passes")
	}
}

// What of a request to a Cursor host asks for a compressed reply goes: HTTP's, Connect's and
// gRPC's.
func TestCursorUnencoded(t *testing.T) {
	h := http.Header{"Accept-Encoding": {"br"}, "Connect-Accept-Encoding": {"gzip"}, "Grpc-Accept-Encoding": {"gzip"}, "Content-Type": {"application/proto"}}
	cursorUnencoded(h)
	if len(h) != 1 || h.Get("Content-Type") == "" {
		t.Errorf("left %v", h)
	}
}

// cursorJWT is a JWT as a Cursor host could hand one out, in a protobuf field.
const cursorJWT = "\x0a\x40eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.real-signature\x10"

// A Cursor host's reply passes as it comes, from a server through Go's transport as the proxy's
// forwarder has it: gzipped (which Go undoes, its length unknown), chunked, or with no length at
// all, and is cut off at the read that would complete a JWT's start, even split across chunks, with
// none of the token's claims or signature passed. Before that, what came passes. (A unary reply's
// body: a stream's is read message by message, TestCursorStreams.)
func TestCursorReplyScans(t *testing.T) {
	s := &server{captures: newCaptures(nil, nil)}
	before := strings.Repeat("x", 5000) + "eyJ"
	var gotAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/proto")
		body := before + strings.TrimPrefix(cursorJWT, "\x0a\x40eyJ")
		switch r.URL.Path {
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = io.WriteString(gz, body)
			_ = gz.Close()
		case "/chunked": // the JWT's start split across the chunks
			for i := 0; i < len(body); i += 7 {
				_, _ = io.WriteString(w, body[i:min(i+7, len(body))])
				w.(http.Flusher).Flush()
			}
		case "/plain":
			_, _ = io.WriteString(w, "\x0a\x03abc")
		}
	}))
	t.Cleanup(srv.Close)
	for _, p := range []string{"/gzip", "/chunked"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+p, nil)
		req.Header.Set("Accept-Encoding", "br") // the VM's
		cursorUnencoded(req.Header)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.cursorReply(resp); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		// Read a byte at a time, the hardest split: the JWT's header passes, harmless, and the
		// read that would complete its claims' start fails.
		got, err := io.ReadAll(iotest.OneByteReader(resp.Body))
		_ = resp.Body.Close()
		body := before + strings.TrimPrefix(cursorJWT, "\x0a\x40eyJ")
		if !errors.Is(err, errCaptureRefused) || strings.Contains(string(got), "eyJzdWIi") || strings.Contains(string(got), "real-signature") ||
			!strings.HasPrefix(body, string(got)) || len(got) < len(before) {
			t.Errorf("%s: read %d bytes (%q…), %v", p, len(got), got[max(0, len(got)-20):], err)
		}
		if gotAE == "br" {
			t.Errorf("%s: the server was asked for br", p)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/plain", nil)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := s.cursorReply(resp); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); err != nil || !bytes.Equal(b, []byte("\x0a\x03abc")) {
		t.Errorf("a reply without a JWT: %q, %v", b, err)
	}
}

// A Cursor host's reply that the proxy can't read is refused: a part of one (206), one encoded as
// the request didn't ask, by HTTP, Connect or gRPC, and a redirect to a token.
func TestCursorReplyRefuses(t *testing.T) {
	s := &server{captures: newCaptures(nil, nil)}
	for name, h := range map[string]http.Header{
		"a part":                   {},
		"br":                       {"Content-Encoding": {"br"}},
		"Connect's gzip":           {"Connect-Content-Encoding": {"gzip"}},
		"gRPC's gzip":              {"Grpc-Encoding": {"gzip"}},
		"a token in its redirect":  {"Location": {"https://cursor.com/x#t=eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"}},
		"a token in its redirect.": {"Location": {"/x?t=eyJhbGciOiJSUzI1NiJ9.eyJzdWIi"}},
	} {
		code := http.StatusOK
		if name == "a part" {
			code = http.StatusPartialContent
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/X", nil)
		resp := &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
		if err := s.cursorReply(resp); !errors.Is(err, errCaptureRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, h := range map[string]http.Header{
		"identity":   {"Connect-Content-Encoding": {"identity"}},
		"a redirect": {"Location": {"https://cursor.com/settings"}},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/X", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("ok")), Request: req}
		if err := s.cursorReply(resp); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		_ = resp.Body.Close()
	}
}

// The agent store gets nothing from a VM, and its token never reaches one: a request to mint it is
// refused, and a reply that holds one, from that method or any other, is cut off at it. Every
// method cursorPersists names is refused too.
func TestCursorStoreToken(t *testing.T) {
	const mint = "/aiserver.v1.AgentStoreService/MintAgentStoreToken"
	if cursorMints(cursorAPI, &http.Request{Method: http.MethodPost, URL: &url.URL{Path: mint}, Header: http.Header{}}) == "" {
		t.Error("minting the store's token passes")
	}
	s := &server{captures: newCaptures(nil, nil)}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+cursorAPI+mint, nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(cursorJWT)), Request: req}
	defer func() { _ = resp.Body.Close() }()
	if err := s.cursorReply(resp); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); !errors.Is(err, errCaptureRefused) || strings.Contains(string(b), "real-signature") {
		t.Errorf("the store's token: %q, %v", b, err)
	}
	for _, p := range cursorPersists {
		if cursorMints(cursorAPI, &http.Request{Method: http.MethodPost, URL: &url.URL{Path: p}, Header: http.Header{}}) == "" {
			t.Errorf("%s passes", p)
		}
	}
}

// A Cursor host's reply sets no cookie, and holds no token in any header, its name or its value,
// nor in the trailers that come after its body, as HTTP/2 and gRPC send them: the body's end fails,
// so ReverseProxy, which copies the trailers after it, never sends them.
func TestCursorReplyHeaders(t *testing.T) {
	s := &server{captures: newCaptures(nil, nil)}
	const tok = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	reply := func(h http.Header) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://agent.api5.cursor.sh/agent.v1.AgentService/Run", nil)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("ok")), Request: req, Trailer: http.Header{}}
	}
	ok := reply(http.Header{"Set-Cookie": {"session=x"}, "X-Request-Id": {"abc"}})
	if err := s.cursorReply(ok); err != nil || ok.Header.Get("Set-Cookie") != "" || ok.Header.Get("X-Request-Id") != "abc" {
		t.Errorf("a plain reply: %v, %v", err, ok.Header)
	}
	_ = ok.Body.Close()
	for _, h := range []http.Header{{"X-Session": {tok}}, {"Grpc-Message": {"token " + tok}}, {"X-" + tok: {"1"}}} {
		r := reply(h)
		if err := s.cursorReply(r); !errors.Is(err, errCaptureRefused) {
			t.Errorf("a token in %v: %v", h, err)
		}
		_ = r.Body.Close()
	}
	// Trailers: filled in once the body has ended, as Go's transport does.
	trailed := reply(http.Header{})
	if err := s.cursorReply(trailed); err != nil {
		t.Fatal(err)
	}
	trailed.Trailer.Set("Grpc-Status-Details-Bin", tok)
	if _, err := io.ReadAll(trailed.Body); !errors.Is(err, errCaptureRefused) {
		t.Errorf("a token in a trailer: %v", err)
	}
	_ = trailed.Body.Close()
	clean := reply(http.Header{})
	if err := s.cursorReply(clean); err != nil {
		t.Fatal(err)
	}
	clean.Trailer.Set("Grpc-Status", "0")
	if b, err := io.ReadAll(clean.Body); err != nil || string(b) != "ok" {
		t.Errorf("plain trailers: %q, %v", b, err)
	}
	_ = clean.Body.Close()
}
