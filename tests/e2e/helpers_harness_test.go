//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/proxy"
)

// The shared helpers' own tests, which need no VM.

// harnessS is a section of a sandbox in dir, its run's window open: what
// the helpers' tests run them in.
func harnessS(t *testing.T) *S {
	t.Helper()
	dir := t.TempDir()
	sb := &sandbox{work: dir, env: os.Environ()}
	r := &runner{ctx: t.Context(), out: newOutput(io.Discard), sb: sb, mark: time.Now()}
	r.open(time.Minute, "check")
	s := &S{r: r, sb: sb, Paths: sb.paths()}
	t.Cleanup(func() { runEnd(t, sb) })
	return s
}

// runEnd is what the run's end does that the helpers count on, what
// sections deferred and the files they made, without sandbox.cleanup's
// pkill, which would match this machine's own forwarders here.
func runEnd(t *testing.T, sb *sandbox) {
	t.Helper()
	for i := len(sb.deferred) - 1; i >= 0; i-- {
		sb.deferred[i](t.Context())
	}
	sb.deferred = nil
	for _, f := range sb.made {
		_ = os.RemoveAll(f)
	}
}

func TestHarnessNoLeak(t *testing.T) {
	dir := t.TempDir()
	secret := "e2e-secret-7f3a" //nolint:gosec // G101: a made-up secret for the scan to find
	write := func(name string, at int64, size int64, text string) string {
		t.Helper()
		f := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		fh, err := os.Create(f)
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		if err := fh.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteAt([]byte(text), at); err != nil {
			t.Fatal(err)
		}
		return f
	}
	clean := filepath.Join(dir, "clean")
	_ = write("clean/a.txt", 0, 0, "nothing here")
	_ = write("clean/sub/b.txt", 0, 0, "nor here")
	// A link to a file with the secret, outside the folder: never followed.
	outside := write("outside.txt", 0, 0, secret)
	if err := os.Symlink(outside, filepath.Join(clean, "link")); err != nil {
		t.Fatal(err)
	}
	if err := NoLeak([]string{secret}, clean, filepath.Join(dir, "missing")); err != nil {
		t.Errorf("a clean folder: got %v", err)
	}
	// A sparse file of 1 GiB, the secret in its only data: found, and the
	// holes never read.
	disk := write("disk/disk.img", 700<<20, 1<<30, secret)
	read, err := scanLeaks([]string{"other", secret}, filepath.Dir(disk))
	if err == nil || !strings.Contains(err.Error(), disk) || strings.Contains(err.Error(), secret) {
		t.Errorf("a sparse disk: got %v, want an error naming the file, not the secret", err)
	}
	if read >= 64<<20 {
		t.Errorf("a sparse disk: read %d bytes, want its data alone", read)
	}
	// A secret across two chunks of the scan.
	dense := filepath.Join(dir, "dense", "f")
	if err := os.MkdirAll(filepath.Dir(dense), 0o700); err != nil {
		t.Fatal(err)
	}
	b := []byte(strings.Repeat("x", leakChunk+1000))
	copy(b[leakChunk-5:], secret)
	if err := os.WriteFile(dense, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NoLeak([]string{secret}, dense); err == nil {
		t.Error("a secret across two chunks: not found")
	}
	if err := NoLeak(nil, clean); err == nil {
		t.Error("no secrets: want an error")
	}
	if err := NoLeak([]string{""}, clean); err == nil {
		t.Error("an empty secret: want an error")
	}
	if os.Getuid() != 0 {
		locked := write("locked/f", 0, 0, "x")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		if err := NoLeak([]string{secret}, filepath.Dir(locked)); err == nil {
			t.Error("an unreadable file: want an error")
		}
	}
}

func TestHarnessToolVersion(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mise.lock")
	lock := `lockfile_version = 3

[[tools.bun]]
version = "1.4.4"
backend = "core:bun"

[tools.bun."platforms.linux-arm64"]
url = "x"

[[tools.node]]
backend = "core:node"

[[tools.go]]
version = "1.26.0"
`
	if err := os.WriteFile(f, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Paths{ToolLock: f}
	for tool, want := range map[string]string{"bun": "1.4.4", "go": "1.26.0", "node": "", "rust": ""} {
		if got := p.ToolVersion(tool); got != want {
			t.Errorf("ToolVersion(%q): got %q, want %q", tool, got, want)
		}
	}
}

// A background command: Start runs it past its window, Stop ends its
// process group, and the run's end stops what's left.
func TestHarnessStart(t *testing.T) {
	s := harnessS(t)
	p, err := s.Start(Cmd{Args: []string{"sh", "-c", "echo up; sleep 60 & wait"}})
	if err != nil {
		t.Fatal(err)
	}
	s.r.open(time.Minute, "setup") // the window it started in ends
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.Out(), "up") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !p.Running() || p.Out() != "up\n" {
		t.Fatalf("got running=%v %q", p.Running(), p.Out())
	}
	start := time.Now()
	p.Stop()
	if p.Running() || time.Since(start) > 10*time.Second {
		t.Errorf("Stop: running=%v after %v", p.Running(), time.Since(start))
	}
	p.Stop() // again: nothing
	q, err := s.Start(Cmd{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	runEnd(t, s.sb)
	if q.Running() {
		t.Error("the run's end: still running")
	}
}

// The process watch sees a command line that was there while it ran, and
// not one that never was; ProcsHave sees one now.
func TestHarnessProcWatch(t *testing.T) {
	s := harnessS(t)
	mark := "e2e-mark-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	w, err := WatchProcs(s)
	if err != nil {
		t.Fatal(err)
	}
	// The shell stays, its arguments the mark: it doesn't exec sleep.
	p, err := s.Start(Cmd{Args: []string{"sh", "-c", "echo up; sleep 60 & wait", mark}})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); p.Out() == "" && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if have, err := ProcsHave(s, mark); err != nil || !have {
		t.Errorf("ProcsHave while it runs: got %v %v", have, err)
	}
	time.Sleep(500 * time.Millisecond)
	p.Stop()
	if have, err := ProcsHave(s, mark); err != nil || have {
		t.Errorf("ProcsHave once it ended: got %v %v", have, err)
	}
	if saw, err := w.Saw(mark); err != nil || !saw {
		t.Errorf("Saw: got %v %v", saw, err)
	}
	if saw, err := w.Saw("e2e-never-" + mark); err != nil || saw {
		t.Errorf("Saw a command line that never was: got %v %v", saw, err)
	}
	w.Stop() // again: nothing
	if !slices.Contains(s.sb.made, filepath.Join(s.sb.work, "ps.log")) {
		t.Error("ps.log isn't deleted at the run's end")
	}
}

// The company proxy's config is one the proxy reads, mode 600, its
// password only there and in Env, never in the proxy's arguments.
func TestHarnessCorp(t *testing.T) {
	s := harnessS(t)
	// What listens on its port stands in for the proxy, which a fake devvm
	// that only waits never would.
	ln := listen(t)
	defer ln.Close()
	s.sb.ports = ln.Addr().(*net.TCPAddr).Port - corpPort
	fake := filepath.Join(s.sb.work, "devvm")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil { //nolint:gosec // G306: the fake devvm runs
		t.Fatal(err)
	}
	s.sb.devvm = fake
	c, err := StartCorp(s)
	if err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(s.sb.work, "corp.json")
	cfg, err := proxy.ReadConfig(cfgFile)
	if err != nil {
		t.Fatalf("the proxy wouldn't read its config: %v", err)
	}
	if cfg.Password != c.Password() || cfg.Listen != ln.Addr().String() || cfg.Log != c.Events || len(cfg.Policies) != 1 {
		t.Errorf("config: got %+v", cfg)
	}
	if b, _ := os.ReadFile(cfg.Policies[0]); string(b) != "**\n" {
		t.Errorf("policy: got %q", b)
	}
	if args := strings.Join(c.proc.cmd.Args, " "); strings.Contains(args, c.Password()) || args != fake+" _proxy -config "+cfgFile {
		t.Errorf("arguments: got %q", args)
	}
	want := "http://devvm:" + c.Password() + "@" + ln.Addr().String()
	if c.URL() != want || !slices.Equal(c.Env(), []string{"HTTPS_PROXY=" + want, "HTTP_PROXY=" + want}) || strings.Contains(c.Addr(), c.Password()) {
		t.Errorf("URL %q, Env %v, Addr %q", c.URL(), c.Env(), c.Addr())
	}
	if err := os.WriteFile(c.Events, []byte(`{"event":"tunnel","host":"github.com","port":443}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.Saw(`"event":"tunnel","host":"github\.com"`) || c.Saw(`example\.com`) {
		t.Error("Saw: wrong match")
	}
	c.Stop()
	if c.Running() {
		t.Error("Stop: still running")
	}
	// A proxy that ends before it listens fails the start, with what it said.
	_ = ln.Close()
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho no config >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: the fake devvm runs
		t.Fatal(err)
	}
	if _, err := StartCorp(s); err == nil || !strings.Contains(err.Error(), "no config") {
		t.Errorf("a proxy that ends: got %v", err)
	}
}

// The run's CA: a client trusting it reaches Serve's server by
// localhost and 127.0.0.1, Sign's certificates verify against it, and
// Trust puts it in devvm's CA folder.
func TestHarnessCA(t *testing.T) {
	s := harnessS(t)
	c, err := NewCA(s)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := NewCA(s); again != c {
		t.Error("NewCA twice: want the run's one")
	}
	pemb, err := os.ReadFile(c.PEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemb) {
		t.Fatal("ca.pem holds no certificate")
	}
	ln := listen(t)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if err := c.Serve(port); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		if code, err := get(t, client, "https://"+host+":"+strconv.Itoa(port)+"/"); err != nil || code != http.StatusOK {
			t.Errorf("%s: got %d %v", host, code, err)
		}
	}
	c.StopServing()
	if _, err := get(t, client, "https://localhost:"+strconv.Itoa(port)+"/"); err == nil {
		t.Error("StopServing: still served")
	}
	cf, kf, err := c.Sign("auth", "auth.openai.com", "api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(cf, kf); err != nil {
		t.Errorf("Sign's pair: %v", err)
	}
	b, _ := os.ReadFile(cf)
	cert, err := x509.ParseCertificate(pemBytes(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: roots}); err != nil {
		t.Errorf("Sign's certificate: %v", err)
	}
	if !strings.HasPrefix(c.Line(1), "-----BEGIN CERTIFICATE") || c.Line(4) == "" || c.Line(1000) != "" {
		t.Errorf("Line: got %q %q", c.Line(1), c.Line(4))
	}
	if err := c.Trust(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.Paths.CAs, "e2e.pem")); string(got) != string(pemb) {
		t.Error("Trust: devvm's CA folder doesn't hold the CA")
	}
	if err := c.Untrust(); err != nil || Exists(s.Paths.CAs) {
		t.Errorf("Untrust: %v", err)
	}
	if !slices.Contains(s.sb.made, c.Dir) || !slices.Contains(s.sb.made, s.Paths.CAs) {
		t.Error("the CA's folder and devvm's CA folder aren't deleted at the run's end")
	}
}

// listen is a listener on a free port of 127.0.0.1.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// get is the status of a GET of u.
func get(t *testing.T, c *http.Client, u string) (int, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	return res.StatusCode, nil
}

// An export's regular files, by name, and tarList's sorted list of them,
// its folders left out.
func TestHarnessExport(t *testing.T) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, h := range []*tar.Header{
		{Name: ".claude/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: ".claude/z.jsonl", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1},
		{Name: ".claude/a.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "h.tar.gz")
	if err := os.WriteFile(f, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, err := tarList(f); err != nil || list != ".claude/a.json\n.claude/z.jsonl" {
		t.Errorf("tarList: got %q, %v", list, err)
	}
	if _, err := tarList(f + "x"); err == nil {
		t.Error("tarList of no file: no error")
	}
}

func pemBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	return blk.Bytes
}

// A failed check keeps every proxy log in the sandbox, the VMs' and the builds', outside the
// folders the run deletes, and prints the end of each one written since the check began; the
// images' and the shared tools' folders aren't searched.
func TestHarnessKeepProxyLogs(t *testing.T) {
	t.Parallel()
	sb := &sandbox{work: t.TempDir()}
	write := func(rel, text string) string {
		f := filepath.Join(sb.devvmDir(), rel)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	write("vms/abc/logs/proxy.log", "devvm proxy: x.test:443: can't resolve it: lookup x.test: i/o timeout\n")
	write("run/builds/b/run/build-proxy/proxy.log", "devvm proxy: crl3.digicert.com:80: can't resolve it\n")
	old := write("vms/old/logs/proxy.log", "an earlier check's line\n")
	write("images/k/proxy.log", "an image's\n")
	write("vms/empty/logs/proxy.log", "")
	since := time.Now().Add(-time.Minute)
	if err := os.Chtimes(old, since.Add(-time.Hour), since.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	dir := filepath.Join(sb.work, "proxy-logs", "7")
	keepProxyLogs(sb, newOutput(&b), dir, since)
	kept, _ := os.ReadDir(dir)
	var names []string
	for _, e := range kept {
		names = append(names, e.Name())
	}
	want := []string{"run_builds_b_run_build-proxy_proxy.log", "vms_abc_logs_proxy.log", "vms_old_logs_proxy.log"}
	if !slices.Equal(names, want) {
		t.Errorf("kept %q, want %q", names, want)
	}
	out := b.String()
	for _, s := range []string{"i/o timeout", "crl3.digicert.com", dir} {
		if !strings.Contains(out, s) {
			t.Errorf("printed no %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "an earlier check's") || strings.Contains(out, "an image's") {
		t.Errorf("printed a log the check didn't write:\n%s", out)
	}
	var none bytes.Buffer
	keepProxyLogs(&sandbox{work: t.TempDir()}, newOutput(&none), filepath.Join(t.TempDir(), "none"), since)
	if none.Len() != 0 {
		t.Errorf("no logs, yet printed %q", none.String())
	}
}
