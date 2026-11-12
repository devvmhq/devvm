package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failing answers code n times, then 200 with body; count is how many
// requests it got.
func failing(t *testing.T, code int, n int32, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if count.Add(1) <= n {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// get is a retried Get, as callers write it.
func get(ctx context.Context, c *Client, url string, limit int64) ([]byte, error) {
	var body []byte
	err := Retry(ctx, 2, time.Millisecond, func() (bool, error) {
		var err error
		body, err = Get(ctx, c, url, limit)
		return Transient(err), err
	})
	return body, err
}

// Failures that may pass are tried again, up to retries times; a 4xx is
// asked once.
func TestRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		code      int
		fails     int32
		wantBody  string
		wantCode  int // the StatusError's, 0 for success
		wantCalls int32
	}{
		{"503 twice, then OK", http.StatusServiceUnavailable, 2, "ok", 0, 3},
		{"429 once, then OK", http.StatusTooManyRequests, 1, "ok", 0, 2},
		{"408 once, then OK", http.StatusRequestTimeout, 1, "ok", 0, 2},
		{"500 three times: gives up", http.StatusInternalServerError, 3, "", 500, 3},
		{"404: once", http.StatusNotFound, 5, "", 404, 1},
		{"403: once", http.StatusForbidden, 5, "", 403, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, count := failing(t, tt.code, tt.fails, "ok")
			body, err := get(t.Context(), NewClient(time.Minute, HTTPSOnly), srv.URL, 10)
			var se *StatusError
			switch {
			case tt.wantCode == 0 && (err != nil || string(body) != tt.wantBody):
				t.Errorf("got %q, %v", body, err)
			case tt.wantCode != 0 && (!errors.As(err, &se) || se.Code != tt.wantCode):
				t.Errorf("got %v, want a %d", err, tt.wantCode)
			}
			if count.Load() != tt.wantCalls {
				t.Errorf("%d requests, want %d", count.Load(), tt.wantCalls)
			}
		})
	}
}

// unanswered is the URL of a server that closes each connection without
// answering. It keeps its port until the test ends, so no other test's
// server can take the port and answer in its place, as one could take a
// closed server's.
func unanswered(t *testing.T) string {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + l.Addr().String()
}

// No answer at all is tried again too.
func TestRetryUnanswered(t *testing.T) {
	t.Parallel()
	url := unanswered(t)
	tries := 0
	err := Retry(t.Context(), 2, time.Millisecond, func() (bool, error) {
		tries++
		_, err := Get(t.Context(), NewClient(time.Minute, HTTPSOnly), url, 10)
		return Transient(err), err
	})
	if err == nil || tries != 3 {
		t.Errorf("got %v after %d tries", err, tries)
	}
}

// A context that ends stops the retries with its error, before a try or
// while waiting for the next.
func TestRetryCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tries := 0
	try := func() (bool, error) { tries++; return true, errors.New("down") }
	if err := Retry(ctx, 2, time.Millisecond, try); !errors.Is(err, context.Canceled) || tries != 0 {
		t.Errorf("canceled first: %v after %d tries", err, tries)
	}
	ctx, cancel = context.WithCancel(t.Context())
	tries = 0
	err := Retry(ctx, 2, time.Hour, func() (bool, error) {
		tries++
		time.AfterFunc(time.Millisecond, cancel)
		return true, errors.New("down")
	})
	if !errors.Is(err, context.Canceled) || tries != 1 {
		t.Errorf("canceled waiting: %v after %d tries", err, tries)
	}
}

// What Transient calls a failure that may pass.
func TestTransient(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		nil:                     false,
		errors.New("reset"):     true,
		&StatusError{Code: 502}: true,
		&StatusError{Code: 429}: true,
		&StatusError{Code: 408}: true,
		&StatusError{Code: 404}: false,
		&StatusError{Code: 401}: false,
		fmt.Errorf("x: %w", &StatusError{Code: 503}): true,
		fmt.Errorf("x: %w", ErrRedirect):             false,
		fmt.Errorf("x: %w", ErrTooLarge):             false,
		fmt.Errorf("x: %w", ErrChecksum):             false,
		fmt.Errorf("x: %w", errRequest):              false,
		fmt.Errorf("x: %w", ErrProxy):                false,
	} {
		if got := Transient(err); got != want {
			t.Errorf("Transient(%v) = %v, want %v", err, got, want)
		}
	}
}

// redirector redirects /to-<scheme> to that scheme's address, and answers
// anything else.
func redirector(t *testing.T, target map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := target[r.URL.Path]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("here"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An HTTPS-only client refuses a redirect to http (or anywhere but https),
// and isn't retried; a same-scheme client follows one on its own scheme
// only.
func TestRedirects(t *testing.T) {
	t.Parallel()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("tls")) }))
	t.Cleanup(tls.Close)
	srv := redirector(t, map[string]string{"/to-http": "/here", "/to-file": "file:///etc/passwd", "/to-https": tls.URL})
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound) //nolint:gosec // the test's own server
	}))
	t.Cleanup(loop.Close)

	https := NewClient(time.Minute, HTTPSOnly)
	trusting, _ := tls.Client().Transport.(*http.Transport)
	https.Transport().TLSClientConfig = trusting.TLSClientConfig // trusts the test server's certificate
	same := NewClient(time.Minute, SameScheme)
	tests := []struct {
		name    string
		c       *Client
		url     string
		want    string // the body, "" for a refused redirect
		wantErr bool
	}{
		{"https only: to http", https, srv.URL + "/to-http", "", true},
		{"https only: to file", https, srv.URL + "/to-file", "", true},
		{"https only: to https", https, srv.URL + "/to-https", "tls", false},
		{"same scheme: to http", same, srv.URL + "/to-http", "here", false},
		{"same scheme: to file", same, srv.URL + "/to-file", "", true},
		{"same scheme: to https", same, srv.URL + "/to-https", "", true},
		{"same scheme: too many", same, loop.URL + "/", "", true},
	}
	for _, tt := range tests {
		tries := 0
		var body []byte
		err := Retry(t.Context(), 2, time.Millisecond, func() (bool, error) {
			tries++
			var err error
			body, err = Get(t.Context(), tt.c, tt.url, 100)
			return Transient(err), err
		})
		if tt.wantErr {
			if !errors.Is(err, ErrRedirect) || tries != 1 {
				t.Errorf("%s: %v after %d tries, want a refused redirect, once", tt.name, err, tries)
			}
			continue
		}
		if err != nil || string(body) != tt.want {
			t.Errorf("%s: %q, %v", tt.name, body, err)
		}
	}
}

// A read is capped: at the cap it's whole, past it ErrTooLarge.
// A Client NewClient didn't make sends nothing, and isn't tried again;
// one NewClient made has its own transport, not net/http's default.
// Trust: a server signed by a CA the system doesn't trust is refused, then accepted once the
// client trusts it; nil keeps the system's roots, and a client NewClient didn't make is left alone.
func TestTrust(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	c := NewClient(time.Minute, HTTPSOnly)
	c.Trust(nil)
	if _, err := Get(t.Context(), c, srv.URL, 10); err == nil {
		t.Fatal("a server signed by an untrusted CA was accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c = NewClient(time.Minute, HTTPSOnly)
	c.Trust(roots)
	if body, err := Get(t.Context(), c, srv.URL, 10); err != nil || string(body) != "ok" {
		t.Errorf("Get = %q, %v", body, err)
	}
	(&Client{}).Trust(roots)
	(*Client)(nil).Trust(roots)
}

// Resolve follows a HEAD's redirects to where they end, HTTPS only, and
// fails on an answer there that isn't 200 OK or an address that isn't
// one.
func TestResolve(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.URL.Path == "/latest":
			http.Redirect(w, r, "/tag/v1.0.0", http.StatusFound)
		case r.URL.Path == "/plain":
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		case r.URL.Path == "/tag/v1.0.0":
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c := NewClient(time.Minute, HTTPSOnly)
	c.Trust(roots)
	if got, err := Resolve(t.Context(), c, srv.URL+"/latest"); err != nil || got != srv.URL+"/tag/v1.0.0" {
		t.Errorf("Resolve = %q, %v", got, err)
	}
	if _, err := Resolve(t.Context(), c, srv.URL+"/gone"); err == nil || Transient(err) {
		t.Errorf("a 404: %v", err)
	}
	if _, err := Resolve(t.Context(), c, srv.URL+"/plain"); !errors.Is(err, ErrRedirect) {
		t.Errorf("a redirect to http: %v", err)
	}
	if _, err := Resolve(t.Context(), c, "%"); !errors.Is(err, errRequest) {
		t.Errorf("no address: %v", err)
	}
}

func TestClientMadeByNewClient(t *testing.T) {
	t.Parallel()
	srv, count := failing(t, 0, 0, "ok")
	for _, c := range []*Client{nil, {}} {
		if _, err := get(t.Context(), c, srv.URL, 10); !errors.Is(err, ErrNoClient) {
			t.Errorf("Get on %#v: %v, want ErrNoClient", c, err)
		}
		if c.Transport() != nil {
			t.Errorf("%#v has a transport", c)
		}
	}
	if n := count.Load(); n != 0 {
		t.Errorf("the server got %d requests, want none", n)
	}
	c := NewClient(time.Minute, HTTPSOnly)
	if c.Transport() == nil || c.Transport() == http.DefaultTransport || c.Transport() == NewClient(time.Minute, HTTPSOnly).Transport() {
		t.Errorf("NewClient's transport %p isn't its own", c.Transport())
	}
	if body, err := Get(t.Context(), c, srv.URL, 10); err != nil || string(body) != "ok" {
		t.Errorf("Get = %q, %v", body, err)
	}
}

func TestReadCapped(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in      string
		limit   int64
		wantErr bool
	}{{"", 0, false}, {"abc", 3, false}, {"abcd", 3, true}, {"a", 0, true}} {
		got, err := ReadCapped(strings.NewReader(tt.in), tt.limit)
		switch {
		case tt.wantErr && !errors.Is(err, ErrTooLarge):
			t.Errorf("ReadCapped(%q, %d) = %q, %v, want ErrTooLarge", tt.in, tt.limit, got, err)
		case !tt.wantErr && (err != nil || string(got) != tt.in):
			t.Errorf("ReadCapped(%q, %d) = %q, %v", tt.in, tt.limit, got, err)
		}
	}
	srv, count := failing(t, 0, 0, "0123456789")
	if _, err := get(t.Context(), NewClient(time.Minute, HTTPSOnly), srv.URL, 9); !errors.Is(err, ErrTooLarge) || count.Load() != 1 {
		t.Errorf("a body past its cap: %v after %d requests", err, count.Load())
	}
	if _, err := ReadCapped(brokenReader{}, 10); err == nil || errors.Is(err, ErrTooLarge) {
		t.Errorf("a failed read: %v", err)
	}
}

// brokenReader is a reader that fails.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// A download is kept only when its SHA-256 matches; one that doesn't, or
// fails, leaves nothing.
func TestDownload(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("the file"))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(time.Minute, HTTPSOnly)
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	gone := func(what string) {
		t.Helper()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: left the file: %v", what, err)
		}
	}
	for _, sum := range []string{strings.Repeat("0", 64), ""} {
		if err := Download(t.Context(), c, srv.URL+"/file", path, sum, 100); !errors.Is(err, ErrChecksum) || Transient(err) {
			t.Errorf("sum %q: %v, want ErrChecksum", sum, err)
		}
		gone("a mismatch")
	}
	if err := Download(t.Context(), c, srv.URL+"/file", path, sumOf("the file"), 7); !errors.Is(err, ErrTooLarge) || Transient(err) {
		t.Errorf("over its cap: %v, want ErrTooLarge", err)
	}
	gone("over its cap")
	var se *StatusError
	if err := Download(t.Context(), c, srv.URL+"/missing", path, sumOf("the file"), 100); !errors.As(err, &se) || se.Code != 404 {
		t.Errorf("missing: %v", err)
	}
	gone("missing")
	if err := Download(t.Context(), c, srv.URL+"/file", filepath.Join(dir, "no", "f"), sumOf("the file"), 100); err == nil {
		t.Error("into a missing folder: no error")
	}
	if err := Download(t.Context(), c, "http://[::1", path, sumOf("the file"), 100); !errors.Is(err, errRequest) || Transient(err) {
		t.Errorf("a bad address: %v", err)
	}
	if err := Download(t.Context(), c, srv.URL+"/file", path, strings.ToUpper(sumOf("the file")), 8); err != nil {
		t.Fatalf("a match: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kept: %v, %v", fi, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "the file" { //nolint:gosec // the test's own file
		t.Errorf("kept %q, %v", data, err)
	}
}
