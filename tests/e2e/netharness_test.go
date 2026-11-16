//go:build e2e

package e2e

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/shell"
)

// The network and nix sections' helpers: devvm with an environment of
// its own, a VM's output searched in the VM, JSON and files read, and
// this machine's HTTP requests.

// python3Tool is a host command the network section uses beyond
// baseTools: python3, for the fake token endpoints and the LAN server.
func python3Tool() tool { return tool{"python3", "python", "python3", "python3"} }

// devvmCmd is devvm args in dir, input on its stdin and env on top of
// the sandbox's.
func devvmCmd(s *S, dir, input string, env []string, args ...string) Cmd {
	return Cmd{Args: append([]string{s.sb.devvm}, args...), Dir: dir, Stdin: input, Env: env}
}

// launchWith is Launch with env in devvm's environment (a fake token, a
// HOME of its own), its shell given exit.
func launchWith(s *S, dir string, env []string, args ...string) (string, Res, error) {
	return s.launch(devvmCmd(s, dir, "exit\n", env, args...))
}

// vmRun runs args in the VM from /, with no login shell and no input:
// limactl shell --workdir / VM ARGS.
func vmRun(s *S, vm string, args ...string) (Res, error) {
	return s.Limactl(append([]string{"shell", "--workdir", "/", vm}, args...)...)
}

// inVM runs script in the VM's login shell: its error, or nil.
func inVM(s *S, vm, script string) error {
	_, err := s.InVM(vm, script)
	return err
}

// vmOut is what script printed in the VM's login shell, trimmed, however
// it ended ($(...)).
func vmOut(s *S, vm, script string) string {
	r, _ := s.InVM(vm, script)
	return strings.TrimSpace(r.Stdout)
}

// vmGrep is whether what script prints in the VM's login shell matches
// the regular expression re (grep -qE in the VM, so what it printed stays
// there).
func vmGrep(s *S, vm, script, re string) bool {
	return inVM(s, vm, script+" | grep -qE "+shell.Quote(re)) == nil
}

// vmHas is whether what script prints in the VM (in a login shell when
// login) holds one of needles: grep in the VM, the needles on its input,
// so neither they nor what the script printed reach a log or a command
// line.
func vmHas(s *S, vm string, login bool, script string, needles ...string) (bool, error) {
	flag := "-c"
	if login {
		flag = "-lc"
	}
	_, err := s.Run(Cmd{
		// The needles first, then the script: grep's own input is the
		// script's output, so they go to grep as its pattern, each line one.
		Args:  []string{"limactl", "shell", "--workdir", "/", vm, "bash", flag, "p=$(cat); { " + script + "\n} 2>/dev/null </dev/null | grep -qF -- \"$p\""},
		Stdin: strings.Join(needles, "\n") + "\n",
	})
	switch ExitStatus(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// failed is a check's failure, msg, with err when there's one.
func failed(msg string, err error) error {
	if err == nil {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// out1 is a command's output, trimmed, as $(...) has it, or nothing when
// it failed.
func out1(r Res, err error) string {
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// grepF is grep -qF PATTERN on text: whether one of pattern's lines is in
// it.
func grepF(text, pattern string) bool {
	for l := range strings.Lines(pattern) {
		if strings.Contains(text, strings.TrimRight(l, "\n")) {
			return true
		}
	}
	return false
}

// countLine is how many of text's lines are line.
func countLine(text, line string) int {
	n := 0
	for l := range strings.Lines(text) {
		if strings.TrimRight(l, "\n") == line {
			n++
		}
	}
	return n
}

// readText is the file f, nothing when it can't be read.
func readText(f string) string {
	b, _ := os.ReadFile(f)
	return string(b)
}

// lineWith is whether one of text's lines holds every one of subs (grep
// A | grep -q B).
func lineWith(text string, subs ...string) bool {
	return countWith(text, subs...) > 0
}

// countWith is how many of text's lines hold every one of subs (grep -c).
func countWith(text string, subs ...string) int {
	n := 0
	for l := range strings.Lines(text) {
		all := true
		for _, sub := range subs {
			all = all && strings.Contains(l, sub)
		}
		if all {
			n++
		}
	}
	return n
}

// modeOf is f's permissions in octal (stat -c %a), nothing without f.
func modeOf(f string) string {
	st, err := os.Stat(f)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(uint64(st.Mode().Perm()), 8)
}

// jsonAt is the value at key (dotted: tokens.account_id) of the JSON
// text b: a string as it is, None for
// null, anything else as JSON; nothing when there's no such key.
func jsonAt(b []byte, key string) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	for k := range strings.SplitSeq(key, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		if v, ok = m[k]; !ok {
			return ""
		}
	}
	switch v := v.(type) {
	case string:
		return v
	case nil:
		return "None"
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// jsonFileAt is jsonAt of the file f.
func jsonFileAt(f, key string) string {
	b, _ := os.ReadFile(f)
	return jsonAt(b, key)
}

// pemLine is line n of the file f, from 1: a certificate's or key's base64,
// which a check looks for elsewhere.
func pemLine(f string, n int) string {
	lines := strings.Split(readText(f), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// pids is the host processes whose command line matches pattern (pgrep
// -f), the bracket in it keeping a shell's own line out.
func pids(s *S, pattern string) []string {
	r, err := s.Run(Cmd{Args: []string{"pgrep", "-f", "--", pattern}})
	if err != nil {
		return nil
	}
	return strings.Fields(r.Stdout)
}

// proxyRuns is whether the VM's proxy runs: its broker, devvm _proxy -config pxc, and its child,
// devvm _proxy-child -for pxc.
func proxyRuns(s *S, pxc string) bool {
	b, c := brokerPair(s, pxc)
	return b != "" && c != ""
}

// proxyLeft is whether anything of the VM's proxy runs: its broker or its child, which a check
// that it's gone looks for both.
func proxyLeft(s *S, pxc string) bool {
	b, c := brokerPair(s, pxc)
	return b != "" || c != ""
}

// devvmSecret is the saved secret name: from the run's Keychain service on
// macOS, else from its file.
func devvmSecret(s *S, name string) string {
	if runtime.GOOS == "darwin" {
		return out1(s.Run(Cmd{Args: []string{"security", "find-generic-password", "-s", s.sb.service, "-a", name, "-w"}}))
	}
	return strings.TrimRight(readText(filepath.Join(s.Paths.Devvm, "secrets", name)), "\n")
}

// forgetSecrets deletes the secrets names saved: the run's Keychain items
// on macOS, their files on Linux. A section's Defer runs it.
func forgetSecrets(ctx context.Context, s *S, names ...string) {
	for _, n := range names {
		if runtime.GOOS == "darwin" {
			_ = exec.CommandContext(ctx, "security", "delete-generic-password", "-s", s.sb.service, "-a", n).Run()
		}
		_ = os.Remove(filepath.Join(s.Paths.Devvm, "secrets", n))
	}
}

// web is an HTTP request from this machine: through proxy when it's set
// (its password in the URL, never in a command line), trusting roots
// when they're set, following redirects, within timeout (30 s when it's
// 0). Its status and body.
type web struct {
	proxy   *url.URL
	roots   *x509.CertPool
	timeout time.Duration
}

func (w web) get(ctx context.Context, u string) (int, []byte, error) {
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if w.proxy != nil {
		tr.Proxy = http.ProxyURL(w.proxy)
	}
	if w.roots != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: w.roots, MinVersion: tls.VersionTLS12}
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(w.timeout, 30*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, err
}

// proxyURL is http://devvm:PASSWORD@127.0.0.1:PORT, a VM's proxy as this
// machine reaches it; no password, no user.
func proxyURL(port, password string) *url.URL {
	u := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
	if password != "" {
		u.User = url.UserPassword("devvm", password)
	}
	return u
}

// rootsOf is a pool of the certificates in the files.
func rootsOf(files ...string) *x509.CertPool {
	p := x509.NewCertPool()
	for _, f := range files {
		p.AppendCertsFromPEM([]byte(readText(f)))
	}
	return p
}

// unverified is whether err is a certificate that didn't verify (curl's
// exit status 60).
func unverified(err error) bool {
	var ua x509.UnknownAuthorityError
	var cv *tls.CertificateVerificationError
	return errors.As(err, &ua) || errors.As(err, &cv)
}

// lanAddr is this machine's LAN address, the source of its route to the
// internet (no packet is sent), nothing without one.
func lanAddr(ctx context.Context) string {
	c, err := (&net.Dialer{}).DialContext(ctx, "udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP.IsLoopback() {
		return ""
	}
	return a.IP.String()
}

// limaListens is whether Lima's own processes (limactl's hostagent) have
// the port, tcp or udp, on this machine.
func limaListens(s *S, proto string, port int) bool {
	p := strconv.Itoa(port)
	if runtime.GOOS == "darwin" {
		args := []string{"lsof", "-a", "-nP", "-c", "limactl", "-iUDP:" + p}
		if proto == "tcp" {
			args = []string{"lsof", "-a", "-nP", "-c", "limactl", "-iTCP:" + p, "-sTCP:LISTEN"}
		}
		_, err := s.Run(Cmd{Args: args})
		return err == nil
	}
	r, err := s.Run(Cmd{Args: []string{"ss", "-Hlnp", "-" + proto[:1], "sport = :" + p}})
	return err == nil && strings.Contains(r.Stdout, `"limactl"`)
}

// loopbackOnly is whether something on this machine listens on TCP port,
// and only on 127.0.0.1 or ::1.
func loopbackOnly(s *S, port int) error {
	p := strconv.Itoa(port)
	var addrs []string
	if runtime.GOOS == "darwin" {
		r, _ := s.Run(Cmd{Args: []string{"lsof", "-nP", "-iTCP:" + p, "-sTCP:LISTEN", "-F", "n"}})
		for l := range strings.Lines(r.Stdout) {
			if a, ok := strings.CutPrefix(strings.TrimSpace(l), "n"); ok {
				addrs = append(addrs, a)
			}
		}
	} else {
		r, _ := s.Run(Cmd{Args: []string{"ss", "-Hltn", "sport = :" + p}})
		for l := range strings.Lines(r.Stdout) {
			if f := strings.Fields(l); len(f) >= 4 {
				addrs = append(addrs, f[3])
			}
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("nothing listens on %s", p)
	}
	for _, a := range addrs {
		if a != "127.0.0.1:"+p && a != "[::1]:"+p {
			return fmt.Errorf("not on loopback alone: %s", strings.Join(addrs, " "))
		}
	}
	return nil
}

// The network and nix sections' readers.
func TestHarnessNetReaders(t *testing.T) {
	b := []byte(`{"tokens": {"account_id": "acct", "n": 3}, "none": null, "s": "x"}`)
	for key, want := range map[string]string{"tokens.account_id": "acct", "tokens.n": "3", "none": "None", "s": "x", "tokens.no": "", "s.x": ""} {
		if got := jsonAt(b, key); got != want {
			t.Errorf("jsonAt(%q): got %q, want %q", key, got, want)
		}
	}
	text := "a b c\nb c\nexact\n"
	switch {
	case !grepF(text, "zz\nb c") || grepF(text, "zz"):
		t.Error("grepF: one of the pattern's lines")
	case countWith(text, "b", "c") != 2 || lineWith(text, "a", "zz"):
		t.Error("countWith, lineWith: lines holding every one")
	case countLine("x\nx\ny", "x") != 2 || countLine(text, "exac") != 0:
		t.Error("countLine: whole lines")
	case FirstLine("one\ntwo") != "one" || tailLines("1\n2\n3\n", 2) != "2\n3" || LastLine("1\n2\n") != "2" || LastLine("") != "":
		t.Error("FirstLine, tailLines, LastLine")
	case !(Res{Out: "a\nb c\n"}).Has("b c") || (Res{Out: "a\nb c\n"}).Has("b"):
		t.Error("Res.Has: whole lines")
	case !hostResolverOff("a: 1\nhostResolver:\n  enabled: false\n") || hostResolverOff("hostResolver:\n  enabled: true\n"):
		t.Error("hostResolverOff: on its line or the next")
	case failed("x", nil).Error() != "x" || !errors.Is(failed("x", os.ErrNotExist), os.ErrNotExist):
		t.Error("failed: the error wrapped when there's one")
	case strings.Join(unproxied("devvm", "up"), " ") != "env -u HTTPS_PROXY -u HTTP_PROXY -u https_proxy -u http_proxy devvm up":
		t.Error("unproxied")
	}
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("l1\nl2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if modeOf(f) != "600" || modeOf(f+"x") != "" || pemLine(f, 2) != "l2" || pemLine(f, 9) != "" {
		t.Errorf("modeOf, pemLine: got %q %q", modeOf(f), pemLine(f, 2))
	}
}

// web reaches a server directly and through a proxy with its password,
// and a certificate the roots don't hold doesn't verify.
func TestHarnessWeb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pw, _ := r.BasicAuth(); r.Header.Get("Proxy-Authorization") == "" && pw == "" && r.URL.Host != "" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	ctx := t.Context()
	if code, body, err := (web{}).get(ctx, srv.URL); err != nil || code != 200 || string(body) != "ok" {
		t.Errorf("direct: got %d %q %v", code, body, err)
	}
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	if code, _, err := (web{proxy: proxyURL(port, "")}).get(ctx, "http://devvm.proxy/x"); err != nil || code != http.StatusProxyAuthRequired {
		t.Errorf("a proxy without its password: got %d %v", code, err)
	}
	if code, _, err := (web{proxy: proxyURL(port, "pw")}).get(ctx, "http://devvm.proxy/x"); err != nil || code != 200 {
		t.Errorf("a proxy with its password: got %d %v", code, err)
	}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()
	if _, _, err := (web{roots: x509.NewCertPool()}).get(ctx, tlsSrv.URL); !unverified(err) {
		t.Errorf("an untrusted certificate: got %v", err)
	}
	if lanAddr(ctx) == "127.0.0.1" {
		t.Error("lanAddr: a loopback address")
	}
}
