// Package upgradetest is a fake release server for devvm upgrade's tests:
// GitHub's releases as devvm reads them, over TLS, with a release key of
// the test's own that signs each release's SHA256SUMS as minisign does.
package upgradetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/blake2b"

	"github.com/devvmhq/devvm/internal/upgrade"
)

// A Key is a minisign key pair of the test's own.
type Key struct {
	id   [8]byte
	priv ed25519.PrivateKey
}

// NewKey makes a key pair.
func NewKey() Key {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	var k Key
	copy(k.id[:], pub[:8])
	k.priv = priv
	return k
}

// Public is the public key's base64 line, as minisign -P takes it.
func (k Key) Public() string {
	raw := append(append([]byte("Ed"), k.id[:]...), k.priv.Public().(ed25519.PublicKey)...)
	return base64.StdEncoding.EncodeToString(raw)
}

// Sign returns a minisign signature file for data, prehashed (ED), with
// trusted as its trusted comment, as minisign -S -t writes it.
func (k Key) Sign(data []byte, trusted string) []byte {
	sum := blake2b.Sum512(data)
	sig := ed25519.Sign(k.priv, sum[:])
	global := ed25519.Sign(k.priv, append(bytes.Clone(sig), trusted...))
	line := append(append([]byte("ED"), k.id[:]...), sig...)
	return []byte("untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(line) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

// Script is a devvm that only says it's version, as a release's devvm
// --version does.
func Script(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }

// An Entry is a file in an archive: its contents, or for a link, its
// target.
type Entry struct {
	Body []byte
	Link string
}

// Archive returns a tar.gz of entries, by their names, in the folder top,
// as a release's archive is.
func Archive(top string, entries map[string]Entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must(tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}))
	for name, e := range entries {
		h := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(e.Body))}
		if !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") {
			h.Name = top + "/" + name
		}
		if e.Link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.Link, 0
		}
		must(tw.WriteHeader(h))
		if e.Link == "" {
			_, err := tw.Write(e.Body)
			must(err)
		}
	}
	must(tw.Close())
	must(gz.Close())
	return buf.Bytes()
}

// A Release is a fake release's files, by name.
type Release map[string][]byte

// NewRelease returns version's release for this platform, its archive
// holding devvm (Script's when nil) and a README, its SHA256SUMS signed
// with k.
func NewRelease(k Key, version string, devvm []byte) Release {
	if devvm == nil {
		devvm = Script(version)
	}
	top := upgrade.Archive(version)
	return Signed(k, version, top+".tar.gz", Archive(top, map[string]Entry{"devvm": {Body: devvm}, "README.md": {Body: []byte("devvm\n")}}))
}

// Signed returns version's release with archive as name: SHA256SUMS
// listing it, and that signed with k as scripts/release.sh signs it.
func Signed(k Key, version, name string, archive []byte) Release {
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  devvm-" + version + ".tar.gz\n")
	return Release{name: archive, "SHA256SUMS": sums, "SHA256SUMS.minisig": k.Sign(sums, "devvm "+version)}
}

// A Server is GitHub's releases of a repository, as devvm reads them:
// releases/latest goes to the newest's tag, and releases/download/v<version>/
// has its files. Its zero Latest has no release.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	latest   string
	releases map[string]Release
}

// NewServer starts a server over TLS, closed when t ends.
func NewServer(t testing.TB) *Server {
	s := Start()
	t.Cleanup(s.Close)
	return s
}

// Start starts a server over TLS, which the caller closes.
func Start() *Server {
	s := &Server{releases: map[string]Release{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

// Add adds version's release, the newest when latest.
func (s *Server) Add(version string, r Release, latest bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[version] = r
	if latest {
		s.latest = version
	}
}

// Roots trusts the server's certificate alone.
func (s *Server) Roots() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Certificate())
	return p
}

// CA is the server's certificate as PEM.
func (s *Server) CA() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/releases/latest" && s.latest != "":
		http.Redirect(w, r, "/releases/tag/v"+s.latest, http.StatusFound)
	case p == "/releases.atom":
		s.feed(w)
	case strings.HasPrefix(p, "/releases/tag/v") && s.releases[strings.TrimPrefix(p, "/releases/tag/v")] != nil:
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(p, "/releases/download/v"):
		v, name, _ := strings.Cut(strings.TrimPrefix(p, "/releases/download/v"), "/")
		body, ok := s.releases[v][name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

// feed writes the releases feed, as GitHub's releases.atom lists them:
// the feed's own link, then an entry for each release, prereleases too,
// its link the release's page.
func (s *Server) feed(w http.ResponseWriter) {
	vs := slices.Sorted(maps.Keys(s.releases))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<feed xmlns="http://www.w3.org/2005/Atom" xml:lang="en-US">` + "\n" +
		`  <link type="text/html" rel="alternate" href="` + s.URL + `/releases"/>` + "\n")
	for _, v := range vs {
		b.WriteString(`  <entry><id>tag:github.com,2008:Repository/1/v` + v + `</id>` +
			`<link rel="alternate" type="text/html" href="` + s.URL + `/releases/tag/v` + v + `"/>` +
			`<title>devvm ` + v + `</title></entry>` + "\n")
	}
	b.WriteString("</feed>\n")
	w.Header().Set("Content-Type", "application/atom+xml")
	_, _ = w.Write([]byte(b.String()))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
