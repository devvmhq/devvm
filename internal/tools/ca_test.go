package tools

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
)

// caPEM is a CA's certificate, PEM, as a company's would be: the TLS test
// server's, which is a CA's.
func caPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// envOf is the value of key in a command's environment, the last one set,
// as exec's is.
func envOf(env []string, key string) (string, bool) {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// The host's mise trusts the user's CAs as well as this machine's own:
// MISE_EXTRA_CA_CERTS names their bundle, in mise's own folder, on Linux
// and macOS alike, and on Linux SSL_CERT_FILE names a bundle of the
// host's and theirs there too. Without CAs neither is named, and a CA
// folder devvm refuses fails it.
func TestMiseTrustsCAs(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := caPEM(srv)
	cases := []struct {
		name, os string
		cas      []byte // the CA folder's company.pem; nil for none
		want     bool   // MISE_EXTRA_CA_CERTS names devvm's bundle
		roots    bool   // SSL_CERT_FILE names the host's and the user's
		err      string // the refusal's names
	}{
		{name: "linux with a CA", os: "linux", cas: ca, want: true, roots: true},
		{name: "macos with a CA", os: "darwin", cas: ca, want: true},
		{name: "without", os: "linux"},
		{name: "a key in the folder", os: "linux", cas: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), err: "ca_not_certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.OS = c.os
			own := writeTemp(t, []byte("the host's roots"))
			e.Getenv = func(k string) string {
				if k == "SSL_CERT_FILE" {
					return own
				}
				return ""
			}
			if c.cas != nil {
				write(t, filepath.Join(e.Dirs.TrustedCAs(), "company.pem"), c.cas)
			}
			dir := t.TempDir()
			cmd, err := e.miseCmd(t.Context(), e.Mise, dir, "lock")
			if c.err != "" {
				if !strings.HasPrefix(msg.NamesOf(err), c.err+" ") {
					t.Fatalf("miseCmd = %v, want %s", msg.NamesOf(err), c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			physical, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := envOf(cmd.Env, "MISE_LOG_FILE"); got != filepath.Join(physical, miseLog) {
				t.Errorf("MISE_LOG_FILE = %q", got)
			}
			got, _ := envOf(cmd.Env, "MISE_EXTRA_CA_CERTS")
			bundle := filepath.Join(physical, "devvm-ca.pem")
			if (got == bundle) != c.want {
				t.Fatalf("MISE_EXTRA_CA_CERTS = %q", got)
			}
			if !c.want {
				if _, err := os.Stat(bundle); !errors.Is(err, os.ErrNotExist) {
					t.Error("a bundle was written")
				}
				return
			}
			if b := string(read(t, bundle)); b != string(ca) {
				t.Errorf("bundle = %q", b)
			}
			got, _ = envOf(cmd.Env, "SSL_CERT_FILE")
			roots := filepath.Join(physical, "devvm-roots.pem")
			if (got == roots) != c.roots {
				t.Fatalf("SSL_CERT_FILE = %q", got)
			}
			if c.roots {
				if b := string(read(t, roots)); b != "the host's roots\n"+string(ca) {
					t.Errorf("roots = %q", b)
				}
			} else if _, err := os.Stat(roots); !errors.Is(err, os.ErrNotExist) {
				t.Error("roots were written")
			}
		})
	}
}

// The download of the host's mise trusts the user's CAs as well as this
// machine's own: a server whose certificate only the CA folder has is
// refused without it, as tools_untrusted.
func TestHostMiseTrustsCAs(t *testing.T) {
	body := []byte("#!/bin/sh\necho mise\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	sum, err := image.SHA256File(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{false, true} {
		e := newEnv(t)
		e.Mise = ""
		e.OS, e.Arch = "linux", "amd64"
		e.miseURL = srv.URL + "/v%[1]s/mise-v%[1]s-%[2]s"
		e.miseSums = map[string]string{"linux-x64": sum}
		if trusted {
			write(t, filepath.Join(e.Dirs.TrustedCAs(), "company.pem"), caPEM(srv))
		}
		_, err := e.HostMise(t.Context())
		if trusted && err != nil {
			t.Errorf("with the CA: %v", msg.NamesOf(err))
		}
		if !trusted && msg.NamesOf(err) != "tools_untrusted dir="+e.Dirs.TrustedCAs() {
			t.Errorf("without the CA: %v", msg.NamesOf(err))
		}
	}
}

// A certificate mise doesn't trust, in its log, is tools_untrusted, which
// names the CA folder, both when the lock is resolved first and when it's
// resolved again; failing otherwise stays what it was.
func TestResolveUntrusted(t *testing.T) {
	e := newEnv(t)
	t.Setenv("MISE_STUB_FAIL", "untrusted")
	want := "tools_untrusted dir=" + e.Dirs.TrustedCAs()
	_, err := e.Lock(t.Context())
	if _, ok := errors.AsType[*backend.LoggedError](err); !ok || msg.NamesOf(err) != want {
		t.Errorf("Lock = %v", msg.NamesOf(err))
	}
	if _, err := os.Stat(e.Dirs.ToolLock()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a lock was written")
	}
	e.locked(t, fixture(t))
	if err := e.Refresh(t.Context()); msg.NamesOf(err) != want {
		t.Errorf("Refresh = %v", msg.NamesOf(err))
	}
	t.Setenv("MISE_STUB_FAIL", "1")
	if err := e.Refresh(t.Context()); msg.NamesOf(err) != "tools_resolve_failed" {
		t.Errorf("Refresh offline = %v", msg.NamesOf(err))
	}
}
