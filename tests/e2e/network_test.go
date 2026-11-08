//go:build e2e

package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/shell"
)

// netRun is the network section's: its project folder and the home of its
// own (a fake ~/.claude.json and Codex login, which devvm agent import
// copies into the sandbox's agent store, so no real token or login is
// read), its VM, the VM's proxy and the fake token endpoints.
type netRun struct {
	s      *S
	n, nh  string // the project folder, its HOME
	logins string // the sandbox's agent store
	vm     string
	vmd    string // the VM's folder
	pxc    string // its proxy's config
	pp     string // its proxy's port on this machine
	salt   string // its placeholders' salt
	events string // its proxy's events
	tokLog string // what the fake token endpoints got
	tokURL string // the fake endpoints, as a company proxy
	tok    *Proc
	cap1   string            // the VM's first captured login, as its curl got it
	lan    string            // this machine's LAN address
	before map[string]string // the home folder's login files
	// lanServer answers on every address of this machine, for the tiers.
	lanServer *Proc
}

// The fake Claude Code token devvm's environment holds, which its proxy
// inherits: never a real one.
const fakeToken = "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-e2efake" //nolint:gosec // G101: a fake token, which no service takes

// networkSection: a VM with Claude Code and Codex behind its proxy, its
// tokens bound to it as placeholders; direct connections, DNS and this
// machine's loopback refused; a company proxy upstream; each VM's CAs;
// logins made in the VM captured; devvm agent login, save and secret;
// the self-check; the hostagent's sandbox (macOS); and the tiers.
func networkSection(s *S) {
	s.Section("network policy")
	n := &netRun{s: s}
	netSetup(n)
	netLaunch(n)
	netBlocks(n)
	netPiTools(n)
	netLoopback(n)
	netCorp(n)
	netCAs(n)
	netTokens(n)
	netCapture(n)
	netCodexLogin(n)
	netOpencodeLogin(n)
	netOpencodeHistory(n)
	netOpencodeKeys(n)
	netPi(n)
	netClaudeLogins(n)
	netSave(n)
	netGithub(n)
	netMise(n)
	netSelfChecks(n)
	if runtime.GOOS == "darwin" {
		netSandbox(n)
	} else {
		for _, t := range sandboxTitles() {
			s.Skip(t, "Seatbelt is macOS's")
		}
	}
	netUnenforced(n)
	netOpen(n)
	netTiers(n)
}

// home is the VM's HOME, with env after it.
func (n *netRun) home(env ...string) []string { return append([]string{"HOME=" + n.nh}, env...) }

// dv runs devvm args in the project folder, input on its stdin.
func (n *netRun) dv(input string, env []string, args ...string) (Res, error) {
	return n.s.Run(devvmCmd(n.s, n.n, input, env, args...))
}

// tier is devvm network's first line: the VM's tier.
func (n *netRun) tier() string {
	r, _ := n.s.Devvm(n.n, "network")
	return FirstLine(r.Stdout)
}

// key is the VM's proxy's password.
func (n *netRun) key() string { return n.s.Paths.State(n.vm)["net.made.proxy_key"] }

func netSetup(n *netRun) {
	s := n.s
	n.nh = filepath.Join(s.sb.work, "net-home")
	_ = os.RemoveAll(n.nh)
	madeFile(s.sb, n.nh)
	_ = os.MkdirAll(filepath.Join(n.nh, ".codex"), 0o700)
	n.before = map[string]string{
		"claude": `{"mcpServers":{"e2e":{"type":"http","url":"https://api.github.com/devvm-e2e-mcp","headers":{"X-Devvm-E2e":"e2efake-mcp"}}}}` + "\n",
		"codex":  `{"OPENAI_API_KEY":"sk-e2efake-codex","last_refresh":null}` + "\n",
	}
	_ = os.WriteFile(filepath.Join(n.nh, ".claude.json"), []byte(n.before["claude"]), 0o600)
	_ = os.WriteFile(filepath.Join(n.nh, ".codex", "auth.json"), []byte(n.before["codex"]), 0o600)
	n.logins = filepath.Join(s.Paths.Devvm, "logins")
	s.Defer(func(ctx context.Context) { forgetSecrets(ctx, s, "github", "claude") })
}

func netLaunch(n *netRun) {
	s := n.s
	s.Check("devvm agent import copies the logins into the sandbox's agent store, only yours to read", func() error {
		if _, err := s.Run(devvmCmd(s, "", "", n.home(), "agent", "import", "--force", "claude", "codex")); err != nil {
			return err
		}
		a := filepath.Join(n.logins, "codex", ".codex", "auth.json")
		switch {
		case readText(a) != n.before["codex"]:
			return errors.New("the store's Codex login isn't the home folder's")
		case !strings.Contains(readText(filepath.Join(n.logins, "claude", ".claude.json")), "e2efake-mcp"):
			return errors.New("the store's ~/.claude.json has no MCP header")
		case modeOf(filepath.Join(n.logins, "codex")) != "700" || modeOf(a) != "600":
			return errors.New("the store's Codex login isn't yours alone to read")
		}
		return nil
	})
	n.n, _ = s.NewProject("net")
	w, werr := WatchProcs(s)
	n.vm, _, _ = launchWith(s, n.n, n.home(fakeToken), "--network", "allowlist", "--no-git-protect", "--agents", "claude,codex,opencode,pi")
	if werr == nil {
		w.Stop()
	}
	n.vmd = s.Paths.VMDir(n.vm)
	st := s.Paths.State(n.vm)
	n.pp, n.salt = st["ports.host.proxy"], st["net.made.proxy_salt"]
	n.pxc, n.events = filepath.Join(n.vmd, "run", "proxy.json"), filepath.Join(n.vmd, "logs", "network.jsonl")
	s.Check("--network allowlist: the VM runs behind its proxy, and the self-check passed", func() error {
		st := s.Paths.State(n.vm)
		switch {
		case n.vm == "" || s.Status(n.vm) != "Running":
			return errors.New("no VM, or it isn't running")
		case st["net.tier"] != "allowlist" || st["net.made.check"] != "ok":
			return fmt.Errorf("its tier is %q, its self-check %q", st["net.tier"], st["net.made.check"])
		case !proxyRuns(s, n.pxc):
			return errors.New("its proxy doesn't run")
		case !strings.Contains(readText(n.pxc), `"listen": "127.0.0.1:`+n.pp+`"`):
			return errors.New("its proxy's config doesn't listen on its port")
		}
		return nil
	})
	s.Check("the proxy's config is yours alone, holds its password, and is all its arguments name", func() error {
		cfg, key := readText(n.pxc), n.key()
		switch {
		case modeOf(n.pxc) != "600" || !Matches(cfg, `^  "version": 1,$`):
			return errors.New("the config isn't mode 600, or of version 1")
		case key == "" || !strings.Contains(cfg, `"password": "`+key+`"`):
			return errors.New("the config doesn't hold the VM's password")
		}
		p := pids(s, "[_]proxy -config "+n.pxc)
		if len(p) == 0 {
			return errors.New("the proxy doesn't run")
		}
		r, _ := s.Run(Cmd{Args: []string{"ps", "-ww", "-o", "command=", "-p", p[0]}})
		if f := strings.Fields(r.Stdout); len(f) < 2 || strings.Join(f[1:], " ") != "_proxy -config "+n.pxc {
			return errors.New("the proxy's arguments name more than its config")
		}
		return nil
	})
	// The control: the launch set the VM's environment (limactl edit, its
	// password's stand-in written as <password> in the run log), and the
	// guest has the password.
	s.Check("the proxy's password was in no host process's arguments while devvm launched the VM, nor after; the guest has it", func() error {
		key := n.key()
		if len(key) != 64 || werr != nil {
			return errors.New("no 64-character password, or no process watch")
		}
		if saw, err := w.Saw(key); err != nil || saw {
			return failed("a host process's arguments held the password while devvm launched the VM", err)
		}
		if have, err := ProcsHave(s, key); err != nil || have {
			return failed("a host process's arguments hold the password", err)
		}
		edited := false
		for _, f := range s.Paths.RunLogs() {
			edited = edited || Matches(readText(f), `limactl edit .*devvm:<password>@`)
		}
		if !edited {
			return errors.New("no run log shows limactl edit setting the proxy, its password's stand-in in its place")
		}
		if has, err := vmHas(s, n.vm, true, "printenv HTTPS_PROXY", ":"+key+"@"); err != nil || !has {
			return failed("the guest's HTTPS_PROXY doesn't hold the password", err)
		}
		return nil
	}, ProcsFlaky())
	s.Check("ssh devvm-net reaches it, confined, its host key checked", func() error {
		cfg := filepath.Join(s.Paths.Devvm, "ssh_config")
		if !Matches(readText(cfg), `^Host devvm-net `+regexp.QuoteMeta(n.vm)+`$`) {
			return errors.New("ssh_config has no Host devvm-net " + n.vm)
		}
		if h := out1(s.Run(Cmd{Args: []string{"ssh", "-F", cfg, "devvm-net", "hostname"}})); h != "net" {
			return fmt.Errorf("ssh devvm-net hostname: got %q", h)
		}
		return nil
	})
	s.Check("devvm host runs the self-check, which passes, and names it", func() error {
		r, err := n.dv("", n.home(fakeToken), "host")
		switch {
		case err != nil:
			return err
		case s.Paths.State(n.vm)["net.made.check"] != "ok":
			return errors.New("the self-check didn't pass")
		case !grepF(r.Out, FirstLine(s.Msg("ssh_host_info", "dir="+n.n, "host=devvm-net"))):
			return errors.New("devvm host didn't name devvm-net")
		}
		return nil
	})
}

func netBlocks(n *netRun) {
	s, vm := n.s, n.vm
	s.Check("direct connections fail, even as root: an address, other ports", func() error {
		for _, args := range [][]string{
			{"sudo", "curl", "--noproxy", "*", "-sS", "-m", "5", "-o", "/dev/null", "https://1.1.1.1"},
			{"sudo", "curl", "--noproxy", "*", "-sS", "-m", "5", "-o", "/dev/null", "http://1.1.1.1"},
			{"sudo", "timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/53"},
			{"sudo", "timeout", "5", "bash", "-c", "exec 3<>/dev/tcp/140.82.112.3/22"},
		} {
			if _, err := vmRun(s, vm, args...); err == nil {
				return errors.New("connected: " + strings.Join(args, " "))
			}
		}
		return nil
	})
	s.Check("an allowed host works, through the proxy", func() error {
		return inVM(s, vm, "curl -fsS -m 20 -o /dev/null https://github.com")
	})
	// Before anything is blocked: a tool that skips the proxy fails for an
	// allowed host, and devvm-why, with nothing to show, says why; the
	// agents' note says the same.
	s.Check("a request that skips the proxy fails for an allowed host, and devvm-why points at the tool's proxy setting", func() error {
		if _, err := vmRun(s, vm, "curl", "--noproxy", "*", "-sS", "-m", "5", "-o", "/dev/null", "https://github.com"); err == nil {
			return errors.New("github.com was reached without the proxy")
		}
		r, err := s.InVM(vm, "devvm-why")
		switch {
		case err != nil:
			return err
		case !r.Has("devvm: nothing blocked recently") || !strings.Contains(r.Stdout, "skipped the proxy: turn on its own proxy setting") ||
			!strings.Contains(r.Stdout, "#a-host-on-the-allowlist-doesnt-resolve"):
			return errors.New("devvm-why doesn't point at the tool's proxy setting: " + r.Stdout)
		}
		if !vmGrep(s, vm, "cat ~/.claude/CLAUDE.md", "while `devvm-why` shows nothing means the tool skipped the proxy") {
			return errors.New("~/.claude/CLAUDE.md's note doesn't say a tool may skip the proxy")
		}
		return nil
	})
	// opencode 2 reads its model catalog from models.opencode.ai and its
	// own service (Zen, the console, MCP's OAuth client) from opencode.ai.
	s.Check("opencode's policy allows the hosts opencode 2 reaches at start", func() error {
		for _, u := range []string{"https://models.opencode.ai/experimental/v2.0/api.json", "https://opencode.ai/v2/cli.json"} {
			if err := inVM(s, vm, "curl -fsS -m 20 -o /dev/null "+u); err != nil {
				return errors.New(u + " wasn't reached through the proxy")
			}
		}
		return nil
	})
	// pi reads its model catalog from pi.dev, its policy's one host.
	s.Check("pi's policy allows its model catalog", func() error {
		if err := inVM(s, vm, "curl -fsS -m 20 -o /dev/null https://pi.dev/api/models/providers/anthropic"); err != nil {
			return errors.New("pi.dev's catalog wasn't reached through the proxy")
		}
		return nil
	})
	s.Check("another host is blocked by the proxy, and so is another port of an allowed one", func() error {
		r, _ := s.InVM(vm, "curl -sS -m 20 -o /dev/null https://example.com")
		switch {
		case !strings.Contains(r.Out, "403"):
			return errors.New("https://example.com wasn't refused with a 403")
		case vmOut(s, vm, "curl -s -m 20 -o /dev/null -w %{http_code} http://example.com/") != "403":
			return errors.New("http://example.com/ wasn't refused with a 403")
		case inVM(s, vm, "curl -fsS -m 20 -o /dev/null https://github.com:8443") == nil:
			return errors.New("github.com:8443 was reached")
		}
		return nil
	})
	s.Check("devvm network log shows the block and the allowed connection", func() error {
		r, err := s.Devvm(n.n, "network", "log")
		if err != nil || !strings.Contains(r.Stdout, "  BLOCKED  example.com:443  ") || !strings.Contains(r.Stdout, "  allowed  github.com:443  rule ") {
			return errors.New("devvm network log doesn't show example.com blocked and github.com allowed")
		}
		return nil
	})
	s.Check("devvm network log --blocked groups it, with the command that allows it", func() error {
		r, _ := s.Devvm(n.n, "network", "log", "--blocked")
		if !Matches(r.Stdout, `^[0-9TZ:-]+ +[0-9]+ +example\.com:443 +devvm network allow example\.com$`) {
			return errors.New("devvm network log --blocked doesn't group example.com:443 with its command")
		}
		return nil
	})
	// The command devvm network log suggests, run as it is: the same proxy
	// allows the host at once, and after devvm network rm blocks it again.
	s.Check("devvm network allow applies at once to the running VM's proxy, devvm network shows it after on, and rm takes it back", n.allowAtOnce)
	// A line that reads as a deny but isn't a rule: the running proxy
	// denies everything, refused with no allow command, devvm network rm
	// warns of it, and once it's gone the allowed host works again. The
	// VM's own file is put back whatever happens, so the checks after
	// this one see its rules.
	s.Check("a deny that isn't a rule makes the running proxy refuse everything until it's gone", n.brokenDeny)
	s.Check("DNS fails at once, not after a timeout", func() error {
		return inVM(s, vm, "timeout 3 getent hosts example.com; [ $? -eq 2 ]")
	})
	// Lima's network's DNS (192.168.5.3) and the host's address on it,
	// asked directly, as root could: a refusal or no answer passes, an
	// address for example.com fails.
	s.Check("... and Lima's network's DNS addresses, asked directly, answer nothing", func() error {
		return inVM(s, vm, limaDNSAsked)
	})
}

func (n *netRun) allowAtOnce() error {
	s := n.s
	p := pids(s, "[_]proxy -config "+n.pxc)
	if len(p) == 0 {
		return errors.New("the proxy doesn't run")
	}
	r, _ := s.Devvm(n.n, "network", "log", "--blocked")
	cmd := ""
	for l := range strings.Lines(r.Stdout) {
		if f := strings.Fields(l); len(f) >= 3 && f[2] == "example.com:443" {
			if i := strings.LastIndex(l, " devvm "); i >= 0 {
				cmd = strings.TrimSpace(l[i+len(" devvm "):])
			}
		}
	}
	if cmd != "network allow example.com" {
		return fmt.Errorf("devvm network log --blocked suggests %q", cmd)
	}
	if _, err := s.Devvm(n.n, strings.Fields(cmd)...); err != nil {
		return err
	}
	r, err := s.Devvm(n.n, "network")
	switch {
	case err != nil || FirstLine(r.Stdout) != "allowlist" || !r.Has("  example.com"):
		return errors.New("devvm network doesn't show allowlist and example.com")
	case inVM(s, n.vm, "curl -fsS -m 20 -o /dev/null https://example.com") != nil:
		return errors.New("example.com isn't allowed at once")
	}
	if _, err := s.Devvm(n.n, "network", "rm", "example.com"); err != nil {
		return err
	}
	switch {
	case vmOut(s, n.vm, "curl -s -m 20 -o /dev/null -w %{http_code} http://example.com/") != "403":
		return errors.New("example.com is still allowed")
	case strings.Join(pids(s, "[_]proxy -config "+n.pxc), " ") != p[0]:
		return errors.New("the proxy was started again")
	}
	return nil
}

func (n *netRun) brokenDeny() error {
	s := n.s
	pol := filepath.Join(n.vmd, "policy")
	keep, err := os.ReadFile(pol)
	if err != nil {
		return err
	}
	if err := AppendFile(pol, "! example.org\n"); err != nil {
		return err
	}
	bad := func() error {
		if vmOut(s, n.vm, "curl -s -m 20 -o /dev/null -w %{http_code} http://github.com/") != "403" {
			return errors.New("github.com wasn't refused")
		}
		r, _ := s.Devvm(n.n, "network", "log")
		if !Matches(r.Stdout, `  BLOCKED  github\.com:80  .*reads as a deny`) {
			return errors.New("devvm network log doesn't say the deny refused github.com:80")
		}
		r, _ = s.Devvm(n.n, "network", "log", "--blocked")
		if !Matches(r.Stdout, ` github\.com:80 +-$`) {
			return errors.New("devvm network log --blocked suggests a command for github.com:80")
		}
		r, _ = s.DevvmNames(n.n, "network", "rm", "nothere.test")
		if !Matches(r.Out, `^devvm: policy_broken_deny `) {
			return errors.New("devvm network rm didn't warn of the broken deny")
		}
		return nil
	}()
	if err := os.WriteFile(pol, keep, 0o600); err != nil {
		return err
	}
	if bad != nil {
		return bad
	}
	return inVM(s, n.vm, "curl -fsS -m 20 -o /dev/null https://github.com")
}

// A service on the host's own loopback (a devvm _proxy, which answers
// anything with 407): unreachable directly, and refused by the VM's proxy
// (internal addresses need a rule naming them).
func netLoopback(n *netRun) {
	s := n.s
	port := strconv.Itoa(s.Port(18765))
	f := filepath.Join(s.sb.work, "listener.json")
	var listener *Proc
	err := WriteProxyConfig(s, f, proxy.Config{VM: "e2e-listener", Listen: "127.0.0.1:" + port, Password: "listener-key"})
	if err == nil {
		listener, err = s.Start(Cmd{Args: []string{s.sb.devvm, "_proxy", "-config", f}})
	}
	if err == nil {
		err = listener.listening(s.r.window().ctx, "127.0.0.1:"+port, 15*time.Second)
	}
	s.Check("the host's loopback services are unreachable, directly or through the proxy", func() error {
		if err != nil {
			return fmt.Errorf("the listener: %w", err)
		}
		code, _, gerr := web{}.get(s.r.window().ctx, "http://127.0.0.1:"+port+"/")
		switch {
		case gerr != nil || code != 407:
			return failed(fmt.Sprintf("the listener answered %d, not 407", code), gerr)
		case func() bool {
			_, e := vmRun(s, n.vm, "curl", "--noproxy", "*", "-sS", "-m", "5", "-o", "/dev/null", "http://192.168.5.2:"+port+"/")
			return e == nil
		}():
			return errors.New("the VM reached it directly")
		case vmOut(s, n.vm, "curl -s -m 10 -o /dev/null -w %{http_code} http://192.168.5.2:"+port+"/") != "403":
			return errors.New("the VM's proxy didn't refuse 192.168.5.2")
		case vmOut(s, n.vm, `curl -s -m 10 -o /dev/null -w %{http_code} --noproxy "" -x "$HTTP_PROXY" http://127.0.0.1:`+port+"/") != "403":
			return errors.New("the VM's proxy didn't refuse 127.0.0.1")
		}
		return nil
	})
	if listener != nil {
		listener.Stop()
	}
}

// A company proxy: the VM's next launch with HTTPS_PROXY and HTTP_PROXY
// in devvm's environment starts its proxy again through it, which sends
// an allowed host there by name, and refuses one the VM's policy doesn't
// allow before it gets there. The launch after, without them, starts it
// again going directly.
func netCorp(n *netRun) {
	s := n.s
	corp, cerr := StartCorp(s)
	s.Check("behind a company proxy, an allowed host goes through it by name, and a blocked one never reaches it", func() error {
		if cerr != nil {
			return cerr
		}
		return n.corpThrough(corp)
	}, Tail(60))
	s.Check("the company proxy's password is in no process's arguments, log or VM", func() error {
		if cerr != nil {
			return cerr
		}
		pw := []string{corp.Password()}
		if have, err := ProcsHave(s, pw[0]); err != nil || have {
			return failed("a process's arguments hold it", err)
		}
		if err := NoLeak(pw, filepath.Join(s.Paths.Devvm, "logs"), filepath.Join(n.vmd, "logs")); err != nil {
			return err
		}
		if err := GuestHasNone(s, n.vm, pw, "/etc", "/home", "/root"); err != nil {
			return err
		}
		if has, err := vmHas(s, n.vm, true, "env", pw...); err != nil || has {
			return failed("the guest's environment holds it", err)
		}
		return nil
	})
	s.Check("without them, the next launch starts the VM's proxy again, going directly", func() error {
		if cerr != nil {
			return cerr
		}
		return n.corpDirect(corp)
	}, Tail(60))
	if corp != nil {
		corp.Stop()
	}
	s.Check("the proxy refuses anything on the host without the VM's password", func() error {
		code, _, err := web{proxy: proxyURL(n.pp, "")}.get(s.r.window().ctx, "http://devvm.proxy/blocked")
		if err != nil || code != 407 {
			return failed(fmt.Sprintf("it answered %d, not 407", code), err)
		}
		return nil
	})
	s.Check("the token never reaches the VM", func() error {
		if err := GuestHasNone(s, n.vm, []string{"e2efake"}, "/etc", "/home", "/root"); err != nil {
			return err
		}
		if has, err := vmHas(s, n.vm, true, "env", "e2efake"); err != nil || has {
			return failed("the guest's environment holds the token", err)
		}
		return nil
	})
}

// corpUpstream is whether the VM's proxy runs, its config naming a
// company proxy.
func (n *netRun) corpUpstream() bool {
	return proxyRuns(n.s, n.pxc) && Matches(readText(n.pxc), `^  "upstream": \{`)
}

func (n *netRun) corpThrough(corp *Corp) error {
	s := n.s
	r, err := n.dv("exit\n", n.home(append([]string{fakeToken}, corp.Env()...)...))
	what := ""
	switch {
	case err != nil:
		what = "the launch"
	case !n.corpUpstream():
		what = "the VM's proxy doesn't go upstream"
	case inVM(s, n.vm, "curl -fsS -m 30 -o /dev/null https://github.com") != nil:
		what = "github.com in the VM"
	case !corp.Saw(`"event":"tunnel","host":"github.com","port":443`):
		what = "the company proxy didn't carry github.com"
	case !Matches(readText(n.events), `"host":"github.com".*"upstream":"`+regexp.QuoteMeta(corp.Addr())+`"`):
		what = "the VM's events show no upstream for github.com"
	case !func() bool {
		o, _ := s.InVM(n.vm, "curl -sS -m 20 -o /dev/null https://example.com")
		return strings.Contains(o.Out, "403")
	}():
		what = "example.com wasn't refused"
	case corp.Saw(`"host":"example.com"`):
		what = "example.com reached the company proxy"
	default:
		return nil
	}
	return errors.New(what + "\n" + n.corpWhy(corp, r))
}

func (n *netRun) corpDirect(corp *Corp) error {
	s := n.s
	r, err := s.Run(Cmd{Args: unproxied(s.sb.devvm), Dir: n.n, Stdin: "exit\n", Env: n.home(fakeToken)})
	what := func() string {
		if err != nil {
			return "the launch"
		}
		if n.corpUpstream() {
			return "the VM's proxy still goes upstream"
		}
		n0 := strings.Count(readText(corp.Events), "\n")
		if inVM(s, n.vm, "curl -fsS -m 30 -o /dev/null https://github.com") != nil {
			return "github.com in the VM"
		}
		if strings.Count(readText(corp.Events), "\n") != n0 {
			return "the company proxy saw it"
		}
		return ""
	}()
	if what == "" {
		return nil
	}
	return errors.New(what + "\n" + n.corpWhy(corp, r))
}

// unproxied is args with no proxy settings in its environment: env -u
// each of them, then args.
func unproxied(args ...string) []string {
	return append([]string{"env", "-u", "HTTPS_PROXY", "-u", "HTTP_PROXY", "-u", "https_proxy", "-u", "http_proxy"}, args...)
}

// corpWhy is what a company proxy check's failure shows: the VM's status
// and proxy, the launch's output, the end of the VM's proxy's log and
// events, and the company proxy's own.
func (n *netRun) corpWhy(corp *Corp, launch Res) string {
	s := n.s
	var b strings.Builder
	args := ""
	if p := pids(s, "[_]proxy -config "+n.pxc); len(p) > 0 {
		r, _ := s.Run(Cmd{Args: []string{"ps", "-ww", "-o", "command=", "-p", strings.Join(p, ",")}})
		for l := range strings.Lines(r.Stdout) {
			args += " " + strings.TrimSpace(l)[:min(60, len(strings.TrimSpace(l)))]
		}
	}
	fmt.Fprintf(&b, "the VM: %s; its proxy:%s\n", s.Status(n.vm), args)
	fmt.Fprintf(&b, "the launch:\n%s\n", tailLines(launch.Out, 20))
	fmt.Fprintf(&b, "the proxy log:\n%s\n", tailLines(readText(filepath.Join(n.vmd, "logs", "proxy.log")), 10))
	fmt.Fprintf(&b, "its events:\n%s\n", tailLines(readText(n.events), 5))
	fmt.Fprintf(&b, "the company proxy's log (it runs: %v):\n%s\n", corp.Running(), tailLines(corp.Log(), 5))
	fmt.Fprintf(&b, "its events:\n%s", tailLines(readText(corp.Events), 5))
	return b.String()
}

// A second VM with network policy, small and with no agents, in the same
// home: each VM has CAs of its own, the current one and the next, which
// its launch installs; its proxy presents only its own, and it trusts no
// other VM's. A certificate's second PEM line holds its serial, and a
// key's its secret, so grep finds that certificate or key in any file
// that holds it.
func netCAs(n *netRun) {
	s := n.s
	n2, _ := s.NewProject("net2")
	vm2, _, _ := launchWith(s, n2, n.home(), "--network", "allowlist", "--no-git-protect", "--agents", "none", "--cpus", "2", "--memory", "2", "--ports", "none")
	ca1, ca2 := filepath.Join(n.vmd, "run", "ca"), filepath.Join(s.Paths.VMDir(vm2), "run", "ca")
	st2 := s.Paths.State(vm2)
	at := func(d, f string) string { return filepath.Join(d, f) }
	s.Check("each VM's launch installs its own CA and the next one, which only the host keeps the keys of", func() error {
		if vm2 == "" || readText(at(ca1, "ca.pem")) == readText(at(ca2, "ca.pem")) || readText(at(ca1, "next.pem")) == readText(at(ca2, "next.pem")) {
			return errors.New("no second VM, or the two VMs share a CA")
		}
		for _, c := range []struct{ vm, dir string }{{n.vm, ca1}, {vm2, ca2}} {
			for crt, pem := range map[string]string{"devvm-proxy.crt": "ca.pem", "devvm-proxy-next.crt": "next.pem"} {
				r, err := vmRun(s, c.vm, "cat", "/usr/local/share/ca-certificates/"+crt)
				if err != nil || r.Stdout != readText(at(c.dir, pem)) {
					return fmt.Errorf("%s's %s isn't its %s", c.vm, crt, pem)
				}
			}
		}
		if err := GuestHasNone(s, n.vm, []string{pemLine(at(ca1, "ca.key"), 2), pemLine(at(ca1, "next.key"), 2)}, "/etc", "/home", "/root", "/usr/local"); err != nil {
			return err
		}
		if modeOf(at(ca1, "ca.key")) != "600" || modeOf(at(ca1, "next.key")) != "600" {
			return errors.New("the CA keys aren't mode 600")
		}
		return nil
	})
	s.Check("a VM trusts no other VM's CA", func() error {
		bundle := []string{"/etc/ssl", "/usr/local/share/ca-certificates"}
		if err := GuestHasNone(s, vm2, []string{pemLine(at(ca1, "ca.pem"), 2), pemLine(at(ca1, "next.pem"), 2)}, bundle...); err != nil {
			return err
		}
		if err := GuestHasNone(s, n.vm, []string{pemLine(at(ca2, "ca.pem"), 2), pemLine(at(ca2, "next.pem"), 2)}, bundle...); err != nil {
			return err
		}
		_, err := s.Run(Cmd{
			Args:  []string{"limactl", "shell", "--workdir", "/", n.vm, "sudo", "grep", "-qsF", "-f", "/dev/stdin", "/etc/ssl/devvm.pem"},
			Stdin: pemLine(at(ca1, "next.pem"), 2) + "\n",
		})
		if err != nil {
			return errors.New("the VM's /etc/ssl/devvm.pem doesn't hold its next CA")
		}
		return nil
	})
	// The proxies' passwords stay in this process: in no command's
	// arguments.
	s.Check("each VM's proxy serves its own CAs, and presents its own CA's certificates, which the other VM's CAs don't verify", func() error {
		ctx := s.r.window().ctx
		p1, p2 := proxyURL(n.pp, n.key()), proxyURL(st2["ports.host.proxy"], st2["net.made.proxy_key"])
		for _, c := range []struct {
			p   web
			dir string
		}{{web{proxy: p1}, ca1}, {web{proxy: p2}, ca2}} {
			code, b, err := c.p.get(ctx, "http://devvm.proxy/ca.pem")
			if err != nil || code != 200 || string(b) != readText(at(c.dir, "ca.pem"))+readText(at(c.dir, "next.pem")) {
				return failed(fmt.Sprintf("a proxy's ca.pem isn't its VM's CAs (%d)", code), err)
			}
		}
		if _, _, err := (web{proxy: p1, roots: rootsOf(at(ca1, "ca.pem"), at(ca1, "next.pem"))}).get(ctx, "https://api.anthropic.com/devvm-e2e-ca"); err != nil {
			return fmt.Errorf("the VM's own CAs don't verify its proxy: %w", err)
		}
		if _, _, err := (web{proxy: p1, roots: rootsOf(at(ca2, "ca.pem"), at(ca2, "next.pem"))}).get(ctx, "https://api.anthropic.com/devvm-e2e-ca"); !unverified(err) {
			return failed("the other VM's CAs verified its proxy, or something else failed", err)
		}
		return nil
	})
	s.Check("devvm rm deletes a VM's CA with its folder, and leaves the other VM's", func() error {
		if _, err := s.Run(devvmCmd(s, n2, "", n.home(), "rm", "--force", "--no-export")); err != nil {
			return err
		}
		if Exists(ca2) || readText(at(ca1, "ca.key")) == "" {
			return errors.New("the removed VM's CA is still there, or the other VM's is gone")
		}
		return nil
	})
}

func netTokens(n *netRun) {
	s, vm := n.s, n.vm
	s.Check("the VM gets its own placeholder, not the token", func() error {
		return inVM(s, vm, "[[ $CLAUDE_CODE_OAUTH_TOKEN == sk-ant-oat01-devvm-proxy-managed-"+n.salt+" ]]")
	})
	s.Check("the VM's Codex and Claude Code (MCP) logins hold only placeholders", func() error {
		r, _ := vmRun(s, vm, "bash", "-c", "cat ~/.codex/auth.json ~/.claude.json")
		c, _ := vmRun(s, vm, "bash", "-c", "cat ~/.codex/auth.json")
		if strings.Contains(r.Stdout, "e2efake") || !strings.Contains(c.Stdout, "devvm-proxy-managed") {
			return errors.New("a login holds a token, or Codex's no placeholder")
		}
		return nil
	})
	// Requests verify the proxy's certificates: no -k.
	s.Check("the proxy puts the token in for its host", func() error {
		if err := inVM(s, vm, `curl -sS -m 20 -o /dev/null https://api.anthropic.com/v1/models -H "Authorization: Bearer $CLAUDE_CODE_OAUTH_TOKEN"`); err != nil {
			return err
		}
		if !lineWith(readText(n.events), `"path":"/v1/models"`, `"secrets":["claude"]`) {
			return errors.New("no event shows the claude secret put in")
		}
		return nil
	})
	s.Check("but not for another VM's placeholder", func() error {
		other := "sk-ant-oat01-devvm-proxy-managed-" + strings.Repeat("0", 32)
		if err := inVM(s, vm, `curl -sS -m 20 -o /dev/null https://api.anthropic.com/devvm-e2e-other -H "Authorization: Bearer `+other+`"`); err != nil {
			return err
		}
		ev := readText(n.events)
		if !strings.Contains(ev, "/devvm-e2e-other") || lineWith(ev, "/devvm-e2e-other", "secrets") {
			return errors.New("no event for the request, or one with a secret put in")
		}
		return nil
	})
	s.Check("and an MCP server's header for its host, from the VM's copy of the login", func() error {
		if err := inVM(s, vm, `curl -sS -m 20 -o /dev/null https://api.github.com/devvm-e2e-mcp -H "X-Devvm-E2e: $(jq -r '.mcpServers.e2e.headers["X-Devvm-E2e"]' ~/.claude.json)"`); err != nil {
			return err
		}
		if !lineWith(readText(n.events), "/devvm-e2e-mcp", `"secrets":["claude-mcp-`) {
			return errors.New("no event shows the MCP header put in")
		}
		return nil
	})
	s.Check("and Codex's API key, from the VM's copy of its login", func() error {
		if err := inVM(s, vm, `curl -sS -m 20 -o /dev/null https://api.openai.com/v1/models -H "Authorization: Bearer $(jq -r .OPENAI_API_KEY ~/.codex/auth.json)"`); err != nil {
			return err
		}
		if !lineWith(readText(n.events), `"path":"/v1/models"`, `"secrets":["codex-key"]`) {
			return errors.New("no event shows Codex's key put in")
		}
		return nil
	})
	s.Check("the home folder's and the store's login files are unchanged", func() error {
		if readText(filepath.Join(n.nh, ".codex", "auth.json")) != n.before["codex"] || readText(filepath.Join(n.nh, ".claude.json")) != n.before["claude"] ||
			readText(filepath.Join(n.logins, "codex", ".codex", "auth.json")) != n.before["codex"] {
			return errors.New("a login file changed")
		}
		return nil
	})
}

// A login made in the VM, captured by its proxy. OpenAI's token endpoint
// is faked by a company proxy of this run's that answers a CONNECT to
// auth.openai.com itself, with a certificate from the run's CA, which the
// launch puts among your CAs, so the VM's proxy trusts it: fake tokens
// for a code (each holding e2efake, which no file in the VM may), and a
// fake API key for an exchange of its fake id_token alone. The VM's
// requests are curl's, as Codex sends them.
func netCapture(n *netRun) {
	s := n.s
	terr := netTokenServer(n)
	s.Check("a login made in the VM is captured: the store gets the real tokens, the VM placeholders and no refresh token, and nothing is logged", func() error {
		if terr != nil {
			return fmt.Errorf("the fake token endpoints: %w", terr)
		}
		if what := n.captured(); what != "" {
			return errors.New(what + "\n" + n.tokenWhy())
		}
		return nil
	}, Tail(60))
}

// netTokenServer starts the fake token endpoints (fakeTokenPy) on slot 0's
// port 47199 as the slot has it, with a certificate the run's CA signed,
// which devvm's CA folder holds until they stop.
func netTokenServer(n *netRun) error {
	s := n.s
	ca, err := NewCA(s)
	if err != nil {
		return err
	}
	cf, kf, err := ca.Sign("auth", "auth.openai.com", "platform.claude.com", "api.anthropic.com")
	if err != nil {
		return err
	}
	if err := ca.Trust(); err != nil {
		return err
	}
	n.tokLog = filepath.Join(s.sb.work, "token.log")
	madeFile(s.sb, n.tokLog)
	if err := os.WriteFile(n.tokLog, nil, 0o600); err != nil {
		return err
	}
	port := strconv.Itoa(s.Port(47199))
	n.tokURL = "http://127.0.0.1:" + port
	if n.tok, err = s.Start(Cmd{Args: []string{"python3", "-I", "-c", fakeTokenPy, port, cf, kf, n.tokLog}}); err != nil {
		return err
	}
	return n.tok.listening(s.r.window().ctx, "127.0.0.1:"+port, 15*time.Second)
}

// tokenEnv is the VM's HOME and the fake endpoints as devvm's company
// proxy, with env after them. Without a Claude Code token in devvm's
// environment (the sandbox's has none), which the VM's proxy inherits and
// which would win over the Claude Code logins it captures.
func (n *netRun) tokenEnv(env ...string) []string {
	return n.home(append([]string{"HTTPS_PROXY=" + n.tokURL, "HTTP_PROXY=" + n.tokURL}, env...)...)
}

// tokenWhy is what the capture's checks look at, when one fails.
func (n *netRun) tokenWhy() string {
	out := ""
	if n.tok != nil {
		out = tailLines(n.tok.Out(), 5)
	}
	var auth []string
	for l := range strings.Lines(readText(n.events)) {
		if strings.Contains(l, "auth.openai.com") {
			auth = append(auth, strings.TrimRight(l, "\n"))
		}
	}
	return "the fake endpoint got:\n" + readText(n.tokLog) + out +
		"\nthe proxy log:\n" + tailLines(readText(filepath.Join(n.vmd, "logs", "proxy.log")), 10) +
		"\nits events:\n" + strings.Join(auth[max(0, len(auth)-5):], "\n")
}

// codexCode is the code Codex sends OpenAI's token endpoint.
const codexCode = "grant_type=authorization_code&code=e2e-code&client_id=app_EMoamEEZ73f0CkXaXp7hrann&code_verifier=e2e&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback"

// tokenPost is the VM's curl of OpenAI's token endpoint with body.
func tokenPost(body string) string {
	return "curl -fsS -m 30 https://auth.openai.com/oauth/token -d '" + body + "'"
}

// captured is the capture's first step that failed, nothing when none.
func (n *netRun) captured() string {
	s := n.s
	if _, err := n.dv("exit\n", n.tokenEnv()); err != nil {
		return "the launch through the fake endpoint"
	}
	r, err := s.InVM(n.vm, tokenPost(codexCode))
	if err != nil {
		return "the code's reply"
	}
	n.cap1 = r.Stdout
	c := []byte(n.cap1)
	if strings.Contains(n.cap1, "e2efake") || jsonAt(c, "refresh_token") != "devvm-proxy-managed" ||
		!strings.HasSuffix(jsonAt(c, "access_token"), ".devvm-placeholder") || !strings.HasSuffix(jsonAt(c, "id_token"), ".devvm-placeholder") {
		return "the reply holds placeholders, and no refresh token"
	}
	a := filepath.Join(n.logins, "codex", ".codex", "auth.json")
	if jsonFileAt(a, "tokens.refresh_token") != "rt-e2efake-capture" || jsonFileAt(a, "tokens.account_id") != "acct-e2e" ||
		!strings.Contains(jsonFileAt(a, "tokens.access_token"), "e2efake") || modeOf(a) != "600" {
		return "the store has the real tokens, only yours to read"
	}
	ex := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Atoken-exchange&client_id=app_EMoamEEZ73f0CkXaXp7hrann&requested_token=openai-api-key&subject_token=" +
		jsonAt(c, "id_token") + "&subject_token_type=urn%3Aietf%3Aparams%3Aoauth%3Atoken-type%3Aid_token"
	if r, err = s.InVM(n.vm, tokenPost(ex)); err != nil {
		return "the API-key exchange of the VM's id_token"
	}
	if jsonAt([]byte(r.Stdout), "access_token") != "sk-devvm-proxy-managed-codex-"+n.salt || jsonFileAt(a, "OPENAI_API_KEY") != "sk-e2efake-captured-key" {
		return "the key is the VM's placeholder, the store's the real one"
	}
	if sec := readText(filepath.Join(n.vmd, "run", "secrets.json")); !strings.Contains(sec, `"name":"codex"`) || !strings.Contains(sec, `"name":"codex-key"`) {
		return "the proxy binds the captured login at once"
	}
	login := `"event":"login","host":"auth.openai.com","port":443,"method":"POST","path":"/oauth/token","login":"codex"`
	if countWith(readText(n.events), login) < 2 ||
		NoLeak([]string{"rt-e2efake-capture", "sk-e2efake-captured-key", "e2efake-sig"}, filepath.Join(n.vmd, "logs"), filepath.Join(s.Paths.Devvm, "logs")) != nil {
		return "the events name the logins, and no log holds a value"
	}
	return ""
}

// devvm agent login codex: the real Codex logs in in the VM, and the
// browser's callback comes back through the forward of this machine's
// localhost:1455, which a request of this process stands in for here,
// with the state Codex's address names; the fake endpoint answers its
// code and its API-key exchange. The forward lasts only while the login
// runs, interrupted too.
func netCodexLogin(n *netRun) {
	s := n.s
	s.Check("devvm agent login codex logs the VM's Codex in through the forward of localhost:1455, and the store saves the login", func() error {
		p, err := s.Start(devvmCmd(s, n.n, "", n.tokenEnv("DEVVM_MESSAGES=names"), "agent", "login", "codex"))
		if err != nil {
			return err
		}
		if what := n.codexLoggedIn(p); what != "" {
			p.Stop()
			return errors.New(what + "\n" + n.loginWhy(p))
		}
		return nil
	}, Tail(60))
	s.Check("an interrupted devvm agent login takes its forward away", func() error {
		p, err := s.Start(devvmCmd(s, n.n, "", n.tokenEnv("DEVVM_MESSAGES=names"), "agent", "login", "codex"))
		if err != nil {
			return err
		}
		ctx := s.r.window().ctx
		what := func() string {
			if loginState(s, p) == "" {
				return "Codex shows its address"
			}
			if forwardGone(ctx) {
				return "the forward listens"
			}
			if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
				return "the interrupt: " + err.Error()
			}
			if rc, ok := waitProc(p, time.Minute); !ok || rc != 130 {
				return fmt.Sprintf("devvm agent login exited %d, not 130", rc)
			}
			if !forwardGone(ctx) {
				return "the forward is gone"
			}
			return ""
		}()
		if what != "" {
			p.Stop()
			return errors.New(what + "\n" + n.loginWhy(p))
		}
		return nil
	})
}

// devvm agent login opencode --device-code: the real opencode 2 logs in
// in the VM with its Codex device code login, the fake endpoint
// answering its code and the code's exchange, whose reply the VM's proxy
// captures, so opencode's database in the VM holds placeholders alone.
func netOpencodeLogin(n *netRun) {
	s := n.s
	s.Check("devvm agent login opencode --device-code logs opencode 2 in in the VM, and the store saves the login", func() error {
		p, err := s.Start(devvmCmd(s, n.n, "", n.tokenEnv("DEVVM_MESSAGES=names"), "agent", "login", "opencode", "--device-code"))
		if err != nil {
			return err
		}
		if what := n.opencodeLoggedIn(p); what != "" {
			p.Stop()
			return errors.New(what + "\n" + n.loginWhy(p))
		}
		return nil
	}, Tail(60))
}

// devvm agent login opencode --api-key saves a key from the network VM;
// a second opencode 2 VM, whose database was made before, gets the key's
// placeholder at its next launch, beside a credential of its own that
// stays.
func netOpencodeKeys(n *netRun) {
	s := n.s
	oc, _ := s.NewProject("net-oc")
	vm, _, _ := launchWith(s, oc, n.home(), "--network", "allowlist", "--no-git-protect", "--agents", "opencode", "--cpus", "2", "--memory", "2", "--ports", "none")
	s.Check("opencode's self-update is off in an opencode VM's login shell", func() error {
		if got := vmOut(s, vm, "printenv OPENCODE_DISABLE_AUTOUPDATE"); vm == "" || got != "1" {
			return fmt.Errorf("OPENCODE_DISABLE_AUTOUPDATE is %q", got)
		}
		return nil
	})
	own := `[{"id":"cred_e2e_own","integrationID":"anthropic","label":"mine","active":true,"value":{"type":"key","key":"sk-ant-e2e-own"}}]`
	s.Check("an opencode 2 VM whose database exists gets a key saved from another VM at its next launch", func() error {
		switch {
		case vm == "":
			return errors.New("no second VM")
		case inVM(s, vm, "opencode auth list >/dev/null && ls ~/.local/share/opencode/opencode*.db >/dev/null") != nil:
			return errors.New("the second VM's opencode made no database")
		case inVM(s, vm, "echo "+shell.Quote(own)+" | opencode auth import") != nil:
			return errors.New("the second VM's opencode didn't import a credential of its own")
		}
		// The company proxy's environment, as the logins before: the save brings the VM up as it was.
		if r, err := n.dv("sk-or-e2efake-saved\n", n.tokenEnv("DEVVM_MESSAGES=names"), "agent", "login", "opencode", "--api-key", "--provider", "openrouter", "--stdin"); err != nil {
			// The run log's end holds opencode.sh's stderr: the import's own, or the command that failed.
			return failed("devvm agent login opencode --api-key failed:\n"+r.Out+"\n"+n.runLogEnd(r.Out), err)
		}
		if has, err := vmHas(s, vm, true, "opencode auth export", "devvm-proxy-managed-opencode-openrouter"); err != nil || has {
			return errors.New("the second VM had the key before its launch")
		}
		if _, _, err := launchWith(s, oc, n.home()); err != nil {
			return err
		}
		export := "opencode auth export openrouter"
		if has, err := vmHas(s, vm, true, export, `"id": "cred_devvm_openrouter"`); err != nil || !has {
			return errors.New("the second VM's opencode has no devvm entry for the key\n" + n.opencodeWhy(vm))
		}
		if has, err := vmHas(s, vm, true, export, "devvm-proxy-managed-opencode-openrouter"); err != nil || !has {
			return errors.New("the second VM's opencode has no placeholder of the key")
		}
		if has, err := vmHas(s, vm, true, "opencode auth export", "e2efake"); err != nil || has {
			return errors.New("the second VM's opencode holds the key itself")
		}
		if has, err := vmHas(s, vm, true, "opencode auth export anthropic", "sk-ant-e2e-own"); err != nil || !has {
			return errors.New("the second VM's own credential is gone")
		}
		return nil
	}, Tail(40))
	s.Check("... and gets it again at its next launch once its database is made again", func() error {
		// opencode's service keeps the old database open: it goes first, within 10 s.
		remake := "pkill -x opencode; i=0; while pgrep -x opencode >/dev/null && [ $i -lt 20 ]; do sleep 0.5; i=$((i + 1)); done; " +
			"! pgrep -x opencode >/dev/null && rm -f ~/.local/share/opencode/opencode.db* && opencode auth list >/dev/null"
		if vm == "" || inVM(s, vm, remake) != nil {
			return errors.New("the second VM's opencode didn't make its database again")
		}
		if has, err := vmHas(s, vm, true, "opencode auth export", "cred_devvm_openrouter"); err != nil || has {
			return errors.New("the new database already holds devvm's entry")
		}
		if _, _, err := launchWith(s, oc, n.home()); err != nil {
			return err
		}
		if has, err := vmHas(s, vm, true, "opencode auth export openrouter", "devvm-proxy-managed-opencode-openrouter"); err != nil || !has {
			return errors.New("the new database has no placeholder of the key after the launch\n" + n.opencodeWhy(vm))
		}
		return nil
	}, Tail(40))
	netOpencodePassKeys(n, oc, vm)
	if vm != "" {
		_, _ = s.Run(devvmCmd(s, oc, "", n.home(), "rm", "--force", "--no-export"))
	}
}

// opencodeLocalKey is the fake key of a local model server's in the
// store's opencode login, which devvm withholds unless config.toml's
// [pass-keys] passes it.
const opencodeLocalKey = "sk-local-e2efake-ollama"

// netOpencodePassKeys: a key of opencode's that the proxy can't put in, a
// local model server's, reaches the VM, vm in the folder oc, only once
// config.toml's [pass-keys] lists its provider, and then opencode 2's
// database gets it too; an OAuth login it doesn't bind never does, listed
// or not. The store's login and config.toml are put back after.
func netOpencodePassKeys(n *netRun, oc, vm string) {
	s := n.s
	store := filepath.Join(n.logins, "opencode", ".local", "share", "opencode", "auth.json")
	cfg := filepath.Join(s.sb.config(), "devvm", "config.toml")
	savedLogin, _ := os.ReadFile(store)
	savedCfg, _ := os.ReadFile(cfg)
	defer func() {
		_ = os.WriteFile(store, savedLogin, 0o600)
		_ = os.WriteFile(cfg, savedCfg, 0o600)
	}()
	launch := func() (Res, error) {
		_, r, err := launchWith(s, oc, n.home("DEVVM_MESSAGES=names"))
		return r, err
	}
	s.Check("opencode's key for a provider devvm doesn't bind is withheld from the VM, warned, and the OAuth login it doesn't bind holds no token", func() error {
		var m map[string]any
		if vm == "" || json.Unmarshal(savedLogin, &m) != nil {
			return errors.New("no second VM, or no store login to add to")
		}
		m["ollama"] = map[string]any{"type": "api", "key": opencodeLocalKey}
		m["copilot-ghe"] = map[string]any{"type": "oauth", "access": "at-e2efake-ghe", "refresh": "rt-e2efake-ghe", "expires": 1, "enterpriseUrl": "ghe.example.com"}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(store, b, 0o600); err != nil {
			return err
		}
		r, err := launch()
		switch {
		case err != nil:
			return failed("the launch failed:\n"+r.Out, err)
		case !Matches(r.Out, `^devvm: login_withheld agent=opencode name=opencode providers=(.*, )?copilot-ghe, (.*, )?ollama(, .*)?$`):
			return errors.New("no warning names the withheld logins:\n" + r.Out)
		}
		if has, err := vmHas(s, vm, false, "cat ~/.local/share/opencode/auth.json", "e2efake-ghe", opencodeLocalKey); err != nil || has {
			return failed("the VM's auth.json holds a withheld key or token", err)
		}
		if has, err := vmHas(s, vm, true, "opencode auth export", opencodeLocalKey); err != nil || has {
			return failed("the VM's opencode holds the withheld key", err)
		}
		return nil
	}, Tail(40))
	s.Check("... and once config.toml's [pass-keys] lists ollama, the VM and its opencode 2 get that key, noted, but never the OAuth login", func() error {
		if vm == "" {
			return errNo
		}
		if err := os.WriteFile(cfg, append(bytes.Clone(savedCfg), []byte("\n[pass-keys]\nopencode = [\"ollama\", \"copilot-ghe\"]\n")...), 0o600); err != nil {
			return err
		}
		r, err := launch()
		switch {
		case err != nil:
			return failed("the launch failed:\n"+r.Out, err)
		case !Matches(r.Out, `^login_passed agent=opencode providers=ollama$`):
			return errors.New("no note names the passed key:\n" + r.Out)
		case vmOut(s, vm, "jq -r .ollama.key ~/.local/share/opencode/auth.json") != opencodeLocalKey:
			return errors.New("the VM's auth.json lacks the passed key")
		}
		if has, err := vmHas(s, vm, false, "cat ~/.local/share/opencode/auth.json", "e2efake-ghe"); err != nil || has {
			return failed("the VM's auth.json holds the OAuth login's tokens", err)
		}
		// Every credential: auth export TARGET resolves the target among opencode's own
		// integrations, which a local server's isn't.
		if has, err := vmHas(s, vm, true, "opencode auth export", opencodeLocalKey); err != nil || !has {
			return errors.New("the VM's opencode didn't import the passed key\n" + n.opencodeWhy(vm))
		}
		return nil
	}, Tail(40))
}

// piKey is the fake Anthropic API key devvm agent login pi saves.
const piKey = "sk-ant-e2efake-pi"

// pi in the network VM: it runs from its read-only shared image with its
// version check and install report off, and its start finds base's fd
// (fdfind) and rg rather than downloading its own. devvm agent login pi --api-key saves a fake Anthropic key in
// the store; the VM's pi gets only its placeholder, and pi's own request
// to Anthropic's API, which the run's fake answers, carries the key the
// VM's proxy put in, logged nowhere.
func netPi(n *netRun) {
	s, vm := n.s, n.vm
	s.Check("pi runs in the VM from its read-only shared image, its version check and install report off", func() error {
		switch {
		case inVM(s, vm, "pi --version </dev/null") != nil:
			return errors.New("pi --version failed")
		case vmOut(s, vm, "printenv PI_SKIP_VERSION_CHECK") != "1" || vmOut(s, vm, "printenv PI_TELEMETRY") != "0":
			return errors.New("PI_SKIP_VERSION_CHECK isn't 1, or PI_TELEMETRY isn't 0")
		case nonLogin(s, vm, "printenv PI_SKIP_VERSION_CHECK PI_TELEMETRY") != "1\n0":
			return errors.New("a non-login bash -c, as ssh VM pi runs pi, lacks PI_SKIP_VERSION_CHECK=1 or PI_TELEMETRY=0")
		case inVM(s, vm, "fdfind --version && rg --version") != nil:
			return errors.New("base has no fdfind or rg for pi")
		case inVM(s, vm, `d=$(dirname "$(mise which pi)") && [ -d "$d" ] && ! touch "$d/devvm-e2e" 2>/dev/null`) != nil:
			return errors.New("pi's install can be written to")
		}
		return nil
	})
	s.Check("devvm agent login pi --api-key: the store keeps the key, the VM's pi its placeholder, and the proxy puts the key in pi's request", func() error {
		r, err := n.dv(piKey+"\n", n.tokenEnv("DEVVM_MESSAGES=names"), "agent", "login", "pi", "--api-key", "--provider", "anthropic", "--stdin")
		if err != nil || !Matches(r.Out, `^==> agent_key_saved agent=pi provider=anthropic vm=`) {
			return failed("devvm agent login pi --api-key didn't save the key:\n"+r.Out, err)
		}
		store := filepath.Join(n.logins, "pi", ".pi", "agent", "auth.json")
		ph := "devvm-proxy-managed-pi-anthropic-" + n.salt
		switch {
		case jsonFileAt(store, "anthropic.key") != piKey || modeOf(store) != "600":
			return errors.New("the store's pi login isn't the key, only yours to read")
		case vmOut(s, vm, "jq -r .anthropic.key ~/.pi/agent/auth.json") != ph:
			return errors.New("the VM's pi login doesn't hold the key's placeholder")
		}
		if has, err := vmHas(s, vm, false, "cat ~/.pi/agent/auth.json", "e2efake"); err != nil || has {
			return failed("the VM's pi login holds the key", err)
		}
		// The fake answers with a 404, so pi fails, after its request.
		_, _ = s.InVM(vm, `timeout 120 pi --no-session --no-tools -p --provider anthropic --model haiku "e2e" </dev/null`)
		switch {
		case !lineWith(readText(n.tokLog), "api /v1/messages", "x-api-key "+piKey):
			return errors.New("pi's request didn't reach Anthropic's API with the key the proxy put in\n" + n.tokenWhy())
		case !lineWith(readText(n.events), `"host":"api.anthropic.com"`, `"secrets":["pi-anthropic"]`):
			return errors.New("no event shows pi's key put in")
		}
		return NoLeak([]string{piKey}, filepath.Join(n.vmd, "logs"), filepath.Join(s.Paths.Devvm, "logs"))
	}, Tail(40))
}

// pi's terminal UI looks for fd (as fd or fdfind) and rg as it starts,
// and downloads each it doesn't find into ~/.pi/agent/bin, unpinned, from
// GitHub, which the allowlist has. With base's on the PATH it downloads
// neither. The control: with a PATH that has no fdfind, the same start
// downloads fd, so the start did reach that check. It runs before the
// checks behind the run's company proxies, which refuse GitHub.
func netPiTools(n *netRun) {
	s, vm := n.s, n.vm
	s.Check("pi's start finds base's fdfind and rg, and downloads neither; without fdfind it downloads fd", func() error {
		const bin = "~/.pi/agent/bin"
		if err := inVM(s, vm, "rm -rf "+bin+" && "+piStart(`"$PATH"`)); err != nil {
			return failed("pi's start", err)
		}
		if got := vmOut(s, vm, "ls -A "+bin+" 2>/dev/null"); got != "" {
			return errors.New("pi downloaded " + got + " into " + bin + " with base's tools on the PATH")
		}
		hidden := vmOut(s, vm, `d=$(mktemp -d) && for b in /usr/local/bin/* /usr/bin/* /bin/*; do n=${b##*/}; `+
			`case $n in fd|fdfind) continue ;; esac; [ -e "$d/$n" ] || ln -s "$b" "$d/$n"; done && echo "$HOME/.local/share/mise/shims:$d"`)
		if hidden == "" {
			return errors.New("no PATH without fdfind")
		}
		defer func() { _ = inVM(s, vm, "rm -rf "+bin+" "+shell.Quote(hidden[strings.LastIndex(hidden, ":")+1:])) }()
		if err := inVM(s, vm, piStart(shell.Quote(hidden))); err != nil {
			return failed("pi's start without fdfind", err)
		}
		if inVM(s, vm, "test -x "+bin+"/fd") != nil {
			var gh []string
			for l := range strings.Lines(readText(n.events)) {
				if strings.Contains(l, "github") {
					gh = append(gh, strings.TrimRight(l, "\n"))
				}
			}
			return errors.New("the control failed: without fdfind, pi's start downloaded no fd, so it may never have looked\n" +
				"the proxy's last GitHub events:\n" + strings.Join(gh[max(0, len(gh)-5):], "\n"))
		}
		return nil
	}, Tail(40))
}

// nonLogin is what script printed in a non-login bash -c in the VM, as
// ssh VM COMMAND and editors run one, trimmed.
func nonLogin(s *S, vm, script string) string {
	r, _ := vmRun(s, vm, "bash", "-c", script)
	return strings.TrimSpace(r.Stdout)
}

// piStart is the script that starts pi's terminal UI in the VM with PATH
// path (shell-quoted), in a terminal of script's (util-linux), lets it
// run for 30 s, then ends it.
func piStart(path string) string {
	return "cd ~ && { TERM=xterm-256color PATH=" + path + ` script -qfec 'stty cols 120 rows 40; exec pi --no-session' /dev/null </dev/null >/dev/null 2>&1 & p=$!; }; ` +
		`sleep 30; pkill -x pi; kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; true`
}

// opencodeWhy is what the VM's opencode got: the IDs of opencode.sh's
// record and of opencode's credentials (never their values), and the
// lines of the newest run log that name opencode.
func (n *netRun) opencodeWhy(vm string) string {
	s := n.s
	ids := vmOut(s, vm, `jq -c '[.[].id]' ~/.local/state/devvm/opencode-import.json; ls ~/.local/share/opencode; opencode auth export 2>&1 | jq -c '[.[] | [.id, .integrationID, .active]]'`)
	runs := s.Paths.RunLogs()
	var lines []string
	for _, f := range runs[max(0, len(runs)-4):] {
		lines = append(lines, "== "+filepath.Base(f))
		for l := range strings.Lines(readText(f)) {
			if strings.Contains(l, "opencode") || strings.Contains(l, "Imported") || strings.Contains(l, "sync") {
				lines = append(lines, strings.TrimRight(l, "\n"))
			}
		}
	}
	return "in the VM: " + ids + "\nthe last run logs' opencode lines:\n" + tailLines(strings.Join(lines, "\n"), 60)
}

// runLogEnd is the last lines of the run log that out's log_end line
// names, nothing when it names none.
func (n *netRun) runLogEnd(out string) string {
	m := regexp.MustCompile(`log_end file=(\S+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return "the run log ends:\n" + tailLines(readText(m[1]), 40)
}

// opencodeLoggedIn is the opencode login's first step that failed,
// nothing when none.
func (n *netRun) opencodeLoggedIn(p *Proc) string {
	s := n.s
	if rc, ok := waitProc(p, 2*time.Minute); !ok || rc != 0 {
		return "devvm agent login ends, saying what the store saved"
	}
	// The fake's access token names no email, and opencode keeps no id_token.
	if !Matches(p.Out(), `^==> agent_login_saved agent=opencode vm=`) {
		return "it says the store saved the login"
	}
	a := filepath.Join(n.logins, "opencode", ".local", "share", "opencode", "auth.json")
	if jsonFileAt(a, "openai.refresh") != "rt-e2efake-capture" || !strings.Contains(jsonFileAt(a, "openai.access"), "e2efake") {
		return "the store has the real tokens"
	}
	if has, err := vmHas(s, n.vm, true, "opencode auth export openai", "devvm-placeholder"); err != nil || !has {
		return "the VM's opencode holds its login's placeholders"
	}
	if has, err := vmHas(s, n.vm, true, "opencode auth export openai", "e2efake"); err != nil || has {
		return "the VM's opencode holds no token of the login"
	}
	if !lineWith(readText(n.events), `"event":"login","host":"auth.openai.com","port":443,"method":"POST","path":"/oauth/token","login":"opencode"`) {
		return "the events name opencode's login"
	}
	return ""
}

// codexLoggedIn is the Codex login's first step that failed, nothing when
// none.
func (n *netRun) codexLoggedIn(p *Proc) string {
	s := n.s
	ctx := s.r.window().ctx
	state := loginState(s, p)
	if state == "" {
		return "Codex shows its address"
	}
	if _, _, err := (web{timeout: time.Minute}).get(ctx, "http://127.0.0.1:1455/auth/callback?code=e2e-code&state="+state); err != nil {
		return "the callback reaches Codex in the VM through the forward, and its success page too: " + err.Error()
	}
	if rc, ok := waitProc(p, time.Minute); !ok || rc != 0 {
		return "devvm agent login ends, saying what the store saved"
	}
	if !Matches(p.Out(), `^==> agent_login_saved_for agent=codex email=e2e@devvm\.test vm=`) {
		return "it names the account"
	}
	a := filepath.Join(n.logins, "codex", ".codex", "auth.json")
	if jsonFileAt(a, "tokens.refresh_token") != "rt-e2efake-capture" || jsonFileAt(a, "OPENAI_API_KEY") != "sk-e2efake-captured-key" {
		return "the store has the real tokens and key"
	}
	r, err := vmRun(s, n.vm, "bash", "-c", "cat ~/.codex/auth.json")
	if err != nil || strings.Contains(r.Stdout, "e2efake") || jsonAt([]byte(r.Stdout), "tokens.refresh_token") != "devvm-proxy-managed" {
		return "the VM's Codex holds placeholders alone"
	}
	if !forwardGone(ctx) {
		return "the forward is gone"
	}
	return ""
}

// loginState is the state in the address a login shows, once it shows one
// (60 s at most, within the check's window), nothing when it ends first.
func loginState(s *S, p *Proc) string {
	re := regexp.MustCompile(`[?&]state=([A-Za-z0-9_-]*)`)
	st := ""
	_ = s.Until(time.Minute, func() error {
		// The first line that names one, its last (sed's greedy .*).
		for l := range strings.Lines(p.Out()) {
			if m := re.FindAllStringSubmatch(l, -1); m != nil {
				if st = m[len(m)-1][1]; st != "" {
					return nil
				}
				break
			}
		}
		if !p.Running() {
			return nil
		}
		return errors.New("no address yet")
	})
	return st
}

// waitProc is p's exit status once it ends, within d: false when it
// hasn't.
func waitProc(p *Proc, d time.Duration) (int, bool) {
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode(), true
	case <-time.After(d):
		return -1, false
	}
}

// forwardGone is whether nothing answers on this machine's
// localhost:1455.
func forwardGone(ctx context.Context) bool {
	_, _, err := (web{timeout: 5 * time.Second}).get(ctx, "http://127.0.0.1:1455/")
	return err != nil
}

// loginWhy is what devvm agent login said, then tokenWhy.
func (n *netRun) loginWhy(p *Proc) string {
	return "devvm agent login said:\n" + tailLines(p.Out(), 20) + "\n" + n.tokenWhy()
}

// claudeSetup is claude setup-token's code, as Claude Code 2.1 sends it.
const claudeSetup = `{"grant_type":"authorization_code","code":"e2e-code","redirect_uri":"https://platform.claude.com/oauth/code/callback",` +
	`"client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e","code_verifier":"e2e","state":"e2e","expires_in":31536000}`

// apiGot is whether the fake API got a request for path that the VM's
// proxy put tok in.
func (n *netRun) apiGot(path, tok string) bool {
	return HasLine(readText(n.tokLog), "api "+path+" Bearer "+tok)
}

// Claude Code's logins, captured at Claude's token endpoint, which the
// fake answers too, with Anthropic's API behind it logging the
// Authorization each request brings. This login path is unofficial: the
// requests are Claude Code 2.1's, read from its bundle. First claude
// setup-token's, as curl: the store keeps its token as the claude
// secret, and the VM gets the secret's placeholder. The proxy saves the
// token in the Keychain under the run's service, which macOS finds
// through HOME, so the VM's home links the Keychains folder.
func netClaudeLogins(n *netRun) {
	s := n.s
	if runtime.GOOS == "darwin" {
		_ = os.MkdirAll(filepath.Join(n.nh, "Library"), 0o700)
		link := filepath.Join(n.nh, "Library", "Keychains")
		_ = os.Remove(link)
		_ = os.Symlink(filepath.Join(os.Getenv("HOME"), "Library", "Keychains"), link)
	}
	s.Check("claude setup-token's token in the VM is captured: the store keeps it as the claude secret, the VM a placeholder", func() error {
		if what := n.setupToken(); what != "" {
			return errors.New(what + "\n" + n.tokenWhy())
		}
		return nil
	}, Tail(60))
	// devvm agent login claude: the real Claude Code's claude auth login in
	// the VM, its code pasted on devvm's input as code#state, the state
	// from the address it shows. The proxy saves the subscription's login
	// in the store, which takes the claude secret away, and the VM is set
	// up again: its ~/.claude/.credentials.json holds placeholders, and
	// its environment no token.
	s.Check("devvm agent login claude logs the VM's Claude Code in, and the store keeps the login, the VM placeholders", n.claudeLogin, Tail(60))
	// The host refreshes the login, through the company proxy: the store's
	// access token made due, the VM's next request (past the proxy's 30 s
	// cache of a login's value) makes the host refresh it at Claude's
	// endpoint, through the run's company proxy. The refresh trusts the
	// VM's proxy's CAs, the run's among them, so it succeeds: the store
	// gets the new tokens, the VM's request goes on with the refreshed
	// access token, and no log holds a value.
	s.Check("the host refreshes Claude Code's login at Claude's endpoint, through the company proxy, when it's due", func() error {
		if what := n.refreshed(); what != "" {
			return errors.New(what + "\n" + n.tokenWhy())
		}
		return nil
	}, Tail(40))
	if n.tok != nil {
		n.tok.Stop()
	}
	if ca, err := NewCA(s); err == nil {
		_ = ca.Untrust()
	}
	s.Check("the next launch, going directly, gives the VM the captured login as placeholders", func() error {
		if _, err := s.Run(Cmd{Args: unproxied(s.sb.devvm), Dir: n.n, Stdin: "exit\n", Env: n.home(fakeToken)}); err != nil {
			return err
		}
		if Matches(readText(n.pxc), `^  "upstream": \{`) {
			return errors.New("the VM's proxy still goes upstream")
		}
		r, err := vmRun(s, n.vm, "bash", "-c", "cat ~/.codex/auth.json")
		c := []byte(r.Stdout)
		switch {
		case err != nil || strings.Contains(r.Stdout, "e2efake") || jsonAt(c, "tokens.refresh_token") != "devvm-proxy-managed":
			return errors.New("the VM's Codex login holds a token, or no placeholder")
		case jsonAt(c, "tokens.access_token") == "" || jsonAt(c, "tokens.access_token") != jsonAt([]byte(n.cap1), "access_token"):
			return errors.New("the VM's access token isn't the captured login's placeholder")
		case jsonAt(c, "OPENAI_API_KEY") != "sk-devvm-proxy-managed-codex-"+n.salt:
			return errors.New("the VM's API key isn't its placeholder")
		}
		return nil
	})
}

// setupToken is claude setup-token's capture's first step that failed,
// nothing when none.
func (n *netRun) setupToken() string {
	s := n.s
	r, err := s.InVM(n.vm, "curl -fsS -m 30 https://platform.claude.com/v1/oauth/token -H 'Content-Type: application/json' -d '"+claudeSetup+"'")
	switch {
	case err != nil:
		return "setup-token's code"
	case strings.Contains(r.Stdout, "e2efake") || jsonAt([]byte(r.Stdout), "access_token") != "sk-ant-oat01-devvm-proxy-managed-"+n.salt:
		return "the VM gets the claude secret's placeholder, and no token"
	case devvmSecret(s, "claude") != "sk-ant-oat01-e2efake-setup":
		return "the claude secret is the token"
	case Exists(filepath.Join(n.logins, "claude", ".claude", ".credentials.json")):
		return "and devvm keeps no OAuth login beside it"
	case inVM(s, n.vm, "curl -sS -m 20 -o /dev/null https://api.anthropic.com/v1/e2e-setup -H 'Authorization: Bearer sk-ant-oat01-devvm-proxy-managed-"+n.salt+"'") != nil:
		return "the proxy puts the token in for the placeholder"
	case !n.apiGot("/v1/e2e-setup", "sk-ant-oat01-e2efake-setup"):
		return "... which the API got"
	case !strings.Contains(readText(n.events), `"event":"login","host":"platform.claude.com","port":443,"method":"POST","path":"/v1/oauth/token","login":"claude"`):
		return "the event names the login"
	}
	return ""
}

// claudeLogin runs devvm agent login claude, its input a FIFO this
// process writes the code into once the address shows.
func (n *netRun) claudeLogin() error {
	s := n.s
	fifo := filepath.Join(s.sb.work, "claude.in")
	_ = os.Remove(fifo)
	madeFile(s.sb, fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return err
	}
	defer os.Remove(fifo)
	p, err := s.Start(Cmd{
		Args: []string{"sh", "-c", `exec "$0" agent login claude <"$1"`, s.sb.devvm, fifo},
		Dir:  n.n, Env: n.tokenEnv("DEVVM_MESSAGES=names"),
	})
	if err != nil {
		return err
	}
	var w *os.File
	_ = s.Until(30*time.Second, func() error {
		if !p.Running() {
			return nil
		}
		// Open for writing fails until devvm's shell opens it to read.
		var err error
		if w, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err != nil {
			w = nil
		}
		return err
	})
	what := "devvm agent login claude never read its input"
	if w != nil {
		what = n.claudeLoggedIn(p, w)
		_ = w.Close()
	}
	if what != "" {
		p.Stop()
		return errors.New(what + "\n" + n.loginWhy(p))
	}
	return nil
}

// claudeLoggedIn is the Claude Code login's first step that failed,
// nothing when none.
func (n *netRun) claudeLoggedIn(p *Proc, w *os.File) string {
	s := n.s
	state := loginState(s, p)
	if state == "" {
		return "Claude Code shows its address"
	}
	if _, err := w.WriteString("e2e-code#" + state + "\n"); err != nil {
		return "the code on devvm's input: " + err.Error()
	}
	if rc, ok := waitProc(p, time.Minute); !ok || rc != 0 {
		return "devvm agent login ends, saying what the store saved"
	}
	c := filepath.Join(n.logins, "claude", ".claude", ".credentials.json")
	switch {
	case !Matches(p.Out(), `^==> claude_login_saved vm=`):
		return "it says the login is saved"
	case jsonFileAt(c, "claudeAiOauth.refreshToken") != "sk-ant-ort01-e2efake-oauth" || jsonFileAt(c, "claudeAiOauth.accessToken") != "sk-ant-oat01-e2efake-oauth" || modeOf(c) != "600":
		return "the store has the real tokens, only yours to read"
	case devvmSecret(s, "claude") != "":
		return "the claude secret is gone"
	case !HasLine(readText(n.tokLog), "POST /v1/oauth/token HTTP/1.1 authorization_code"):
		return "the code went to the endpoint as a login's, not setup-token's"
	case !n.apiGot("/api/oauth/profile", "sk-ant-oat01-e2efake-oauth"):
		return "Claude Code read the profile with the login the proxy put in"
	}
	r, err := vmRun(s, n.vm, "bash", "-c", "cat ~/.claude/.credentials.json")
	v := []byte(r.Stdout)
	switch {
	case err != nil || strings.Contains(r.Stdout, "e2efake") || jsonAt(v, "claudeAiOauth.refreshToken") != "devvm-proxy-managed" ||
		jsonAt(v, "claudeAiOauth.accessToken") != "sk-ant-oat01-devvm-proxy-managed-oauth-"+n.salt:
		return "the VM's Claude Code holds placeholders alone"
	case vmOut(s, n.vm, "printenv CLAUDE_CODE_OAUTH_TOKEN") != "":
		return "and no Claude Code token in its environment"
	}
	return ""
}

// refreshed is the refresh's first step that failed, nothing when none.
func (n *netRun) refreshed() string {
	s := n.s
	c := filepath.Join(n.logins, "claude", ".claude", ".credentials.json")
	if err := expire(c); err != nil {
		return "the store's access token is due: " + err.Error()
	}
	time.Sleep(31 * time.Second)
	before := countLine(readText(n.tokLog), "CONNECT platform.claude.com")
	switch {
	case inVM(s, n.vm, `curl -sS -m 30 -o /dev/null https://api.anthropic.com/v1/e2e-oauth -H "Authorization: Bearer $(jq -r .claudeAiOauth.accessToken ~/.claude/.credentials.json)"`) != nil:
		return "the VM's request with its placeholder"
	case countLine(readText(n.tokLog), "CONNECT platform.claude.com") <= before:
		return "the host's refresh went to Claude's endpoint through the company proxy"
	case !HasLine(readText(n.tokLog), "POST /v1/oauth/token HTTP/1.1 refresh_token"):
		return "the refresh asked with the store's refresh token"
	case jsonFileAt(c, "claudeAiOauth.refreshToken") != "sk-ant-ort01-e2efake-refreshed" || jsonFileAt(c, "claudeAiOauth.accessToken") != "sk-ant-oat01-e2efake-refreshed":
		return "the store holds the refreshed login"
	case !n.apiGot("/v1/e2e-oauth", "sk-ant-oat01-e2efake-refreshed"):
		return "the VM's request went on with the refreshed access token"
	case NoLeak([]string{"e2efake-oauth", "e2efake-setup", "e2efake-refreshed"}, filepath.Join(n.vmd, "logs"), filepath.Join(s.Paths.Devvm, "logs")) != nil:
		return "no log holds a value"
	}
	return ""
}

// expire makes the Claude Code login in the file f due: its expiresAt 0.
func expire(f string) error {
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return err
	}
	o, ok := m["claudeAiOauth"].(map[string]any)
	if !ok {
		return errors.New("no claudeAiOauth")
	}
	o["expiresAt"] = 0
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(f, out, 0o600)
}

// devvm agent save from the VM with network policy: its ~/.claude.json
// holds the MCP server's header as a placeholder, which the save puts the
// store's value back for, and a changed URL around it is refused.
func netSave(n *netRun) {
	s := n.s
	store := filepath.Join(n.logins, "claude", ".claude.json")
	save := func() (Res, error) { return n.dv("y\n", n.home("DEVVM_PIPED_ANSWERS=1"), "agent", "save", "claude") }
	_ = inVM(s, n.vm, `jq ".theme = \"e2e-saved\"" ~/.claude.json > ~/.claude.json.t && mv ~/.claude.json.t ~/.claude.json`)
	s.Check("devvm agent save puts the store's MCP header back for the VM's placeholder", func() error {
		r, err := save()
		if err != nil {
			return err
		}
		c := readText(store)
		switch {
		case !grepF(r.Out, s.Msg("agent_settings_saved", "agent=claude", "dir="+filepath.Join(s.sb.config(), "devvm", "agents", "claude"), "vm="+n.vm)):
			return errors.New("devvm agent save didn't say it saved the settings")
		case !strings.Contains(c, "e2e-saved") || !strings.Contains(c, "e2efake-mcp"):
			return errors.New("the store's ~/.claude.json lacks the VM's change or the MCP header")
		case strings.Contains(c, "devvm-proxy-managed") || strings.Contains(c, "projects"):
			return errors.New("the store's ~/.claude.json holds a placeholder or the VM's projects")
		}
		return nil
	})
	_ = inVM(s, n.vm, `jq ".mcpServers.e2e.url = \"https://example.com/devvm-e2e-mcp\"" ~/.claude.json > ~/.claude.json.t && mv ~/.claude.json.t ~/.claude.json`)
	s.Check("... and refuses it when the server's URL changed, so the header never goes to another host", func() error {
		r, err := save()
		if err != nil {
			return err
		}
		c := readText(store)
		switch {
		case !grepF(r.Out, s.Msg("agent_save_placeholder", "path=.claude.json", "vm="+n.vm)):
			return errors.New("devvm agent save didn't refuse the placeholder")
		case !strings.Contains(c, "api.github.com/devvm-e2e-mcp") || strings.Contains(c, "example.com"):
			return errors.New("the store's ~/.claude.json took the changed URL")
		}
		return nil
	})
}

// devvm secret with a fake GitHub token: in the Keychain under this run's
// service on macOS, else in the sandbox's config folder; deleted at the
// end. The token goes on devvm's input, so it's in no process's
// arguments.
// netMise installs a project's own tools with mise on the allowlist tier:
// pnpm's aqua download is checked against its GitHub attestations, which
// need Sigstore's trust root.
func netMise(n *netRun) {
	s := n.s
	s.Check("mise installs a project's own pnpm and node, attestations checked, on the allowlist tier", func() error {
		r, err := s.InVM(n.vm, `d=$(mktemp -d) && cd "$d" && printf '[tools]\npnpm = "12.8.1"\nnode = "22"\n' > mise.toml && mise trust -q && mise install && mise exec -- pnpm --version`)
		if err != nil {
			return failed("mise install failed: "+tailLines(r.Out, 8), err)
		}
		if !strings.Contains(r.Stdout, "12.8.1") {
			return errors.New("pnpm isn't 12.8.1: " + tailLines(r.Out, 4))
		}
		return nil
	})
}

func netGithub(n *netRun) {
	s := n.s
	const tok = "ghp_E2EFAKE"
	secrets := filepath.Join(n.vmd, "run", "secrets.json")
	list := func() string { r, _ := s.Run(devvmCmd(s, "", "", n.home(), "secret")); return r.Stdout }
	s.Check("devvm secret set saves a token, and devvm secret lists it saved", func() error {
		if _, err := s.Run(devvmCmd(s, "", tok+"\n", n.home(), "secret", "set", "github", "--stdin")); err != nil {
			return err
		}
		if !Matches(list(), `^github +yes `) {
			return errors.New("devvm secret doesn't list github saved")
		}
		if runtime.GOOS == "darwin" && devvmSecret(s, "github") != tok {
			return errors.New("the Keychain doesn't hold the token")
		}
		return nil
	})
	s.Check("no devvm command prints a secret", func() error {
		r, err := s.Run(devvmCmd(s, "", "", n.home(), "secret", "get", "github"))
		if err == nil || strings.Contains(r.Out, tok) {
			return errors.New("devvm secret get printed it, or didn't fail")
		}
		return nil
	})
	s.Check("secret use github: its proxy gets the binding at once, the VM its own placeholder on its next launch", func() error {
		if _, err := n.dv("", n.home(), "secret", "use", "github"); err != nil {
			return err
		}
		if !strings.Contains(readText(secrets), `"name":"github"`) {
			return errors.New("the proxy has no github binding")
		}
		if _, err := n.dv("exit\n", n.home()); err != nil {
			return err
		}
		if vmOut(s, n.vm, "echo $GH_TOKEN") != "devvm-proxy-managed-github-"+n.salt {
			return errors.New("the VM's GH_TOKEN isn't its placeholder")
		}
		if has, err := vmHas(s, n.vm, true, "env; cat ~/.config/devvm/env", tok); err != nil || has {
			return failed("the VM holds the token", err)
		}
		if !Matches(list(), `^github +yes +`+regexp.QuoteMeta(n.vm)+`$`) {
			return errors.New("devvm secret doesn't list the VM using github")
		}
		return nil
	})
	s.Check("the proxy puts the GitHub token in for its host", func() error {
		if err := inVM(s, n.vm, `curl -sS -m 20 -o /dev/null https://api.github.com/user -H "Authorization: token $GH_TOKEN"`); err != nil {
			return err
		}
		if !lineWith(readText(n.events), `"path":"/user"`, `"secrets":["github"]`) {
			return errors.New("no event shows the github secret put in")
		}
		return nil
	})
	s.Check("secret unuse and rm take the binding and the token away at once", func() error {
		if _, err := n.dv("", n.home(), "secret", "unuse", "github"); err != nil {
			return err
		}
		if strings.Contains(readText(secrets), `"name":"github"`) {
			return errors.New("the proxy still has the github binding")
		}
		if _, err := s.Run(devvmCmd(s, "", "", n.home(), "secret", "rm", "--force", "github")); err != nil {
			return err
		}
		if !Matches(list(), `^github +no `) {
			return errors.New("devvm secret doesn't list github unsaved")
		}
		if runtime.GOOS == "darwin" {
			if _, err := s.Run(Cmd{Args: []string{"security", "find-generic-password", "-s", s.sb.service, "-a", "github"}}); err == nil {
				return errors.New("the Keychain still holds the token")
			}
		}
		return nil
	})
}

func netSelfChecks(n *netRun) {
	s, vm := n.s, n.vm
	s.Check("the self-check passes on the VM devvm started confined", func() error {
		return s.GoE2E("TestE2ESelfCheck", "netpolicy", "DEVVM_E2E_VM="+vm, "DEVVM_E2E_WANT=")
	})
	s.Check("a VM started outside devvm is unenforced, and the self-check stops it", func() error {
		if _, err := s.Limactl("stop", vm); err != nil {
			return err
		}
		if _, err := s.Limactl("start", "--tty=false", vm); err != nil {
			return err
		}
		if err := s.GoE2E("TestE2ESelfCheck", "netpolicy", "DEVVM_E2E_VM="+vm, "DEVVM_E2E_WANT=net_unenforced_stop vm="+vm); err != nil {
			return err
		}
		if s.Status(vm) != "Stopped" || s.Paths.State(vm)["net.made.check"] != "unenforced" {
			return errors.New("the VM isn't stopped, or its self-check isn't unenforced")
		}
		return nil
	})
	s.Check("devvm starts it again confined, and the self-check passes", func() error {
		if _, err := n.dv("exit\n", n.home()); err != nil {
			return err
		}
		if s.Status(vm) != "Running" || s.Paths.State(vm)["net.made.check"] != "ok" {
			return errors.New("the VM isn't running, or its self-check didn't pass")
		}
		return nil
	})
	s.Check("devvm stop stops it and its proxy at once, and devvm starts both again", func() error {
		if !proxyRuns(s, n.pxc) {
			return errors.New("its proxy doesn't run")
		}
		if _, err := n.dv("", n.home(), "stop"); err != nil {
			return err
		}
		if s.Status(vm) != "Stopped" || proxyRuns(s, n.pxc) {
			return errors.New("the VM or its proxy still runs")
		}
		if _, err := n.dv("exit\n", n.home()); err != nil {
			return err
		}
		if s.Status(vm) != "Running" || !proxyRuns(s, n.pxc) || s.Paths.State(vm)["net.made.check"] != "ok" {
			return errors.New("the VM or its proxy doesn't run again, or its self-check didn't pass")
		}
		return nil
	})
}

// sandboxTitles are the checks of the hostagent's sandbox, macOS's alone.
func sandboxTitles() []string {
	return []string{
		"the hostagent's profile denies every bind but its ssh port's (TCP) and its own unix sockets'; it has no other",
		"with Lima's own forwards on, the sandbox refuses the hostagent a VM server's port, TCP or UDP",
		"... and the VM works: ssh, devvm's forwarder (on loopback alone), the proxy, the self-check",
		"the next start puts Lima's ignore rules back",
	}
}

// limaDNSAsked asks Lima's network's DNS (192.168.5.3) and the host's
// address on it for example.com directly: a refusal or no answer passes.
const limaDNSAsked = `python3 - <<'PY'
import socket, sys
q = bytes.fromhex("123401000001000000000000076578616d706c6503636f6d0000010001")
for h in ("192.168.5.3", "192.168.5.2"):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(3)
    try:
        s.sendto(q, (h, 53))
        d = s.recv(512)
    except OSError as e:
        print(h, e)
        continue
    print(h, "rcode", d[3] & 15, "answers", int.from_bytes(d[6:8], "big"))
    if d[3] & 15 == 0 and int.from_bytes(d[6:8], "big") > 0:
        sys.exit("example.com answered by " + h)
PY`

// macOS: the hostagent's Seatbelt profile denies it every bind but its ssh
// port's, so Lima forwards nothing even without its ignore rules. A
// limactl first on PATH puts Lima's own rule (forward everything) back
// for one start, inside the sandbox devvm starts limactl in: Lima tries
// the guest's ports (its log says so), the sandbox refuses them, and
// devvm's forwarder has them.
func netSandbox(n *netRun) {
	s, vm := n.s, n.vm
	sp := s.Paths.State(vm)["ports.host.ssh"]
	lvm := filepath.Join(s.sb.lima, vm)
	ha := filepath.Join(lvm, "ha.stderr.log")
	p95, p96 := s.Port(47195), s.Port(47196)
	haListens := func() string {
		pid := strings.TrimSpace(readText(filepath.Join(lvm, "ha.pid")))
		r, _ := s.Run(Cmd{Args: []string{"lsof", "-nP", "-a", "-p", pid, "-iTCP", "-sTCP:LISTEN", "-iUDP"}})
		var l []string
		for i, line := range strings.Split(strings.TrimRight(r.Stdout, "\n"), "\n") {
			if f := strings.Fields(line); i > 0 && len(f) >= 9 {
				l = append(l, f[7]+" "+f[8])
			}
		}
		return strings.Join(l, "\n")
	}
	titles := sandboxTitles()
	s.Check(titles[0], func() error {
		sbf := filepath.Join(n.vmd, "run", "sandbox.sb")
		prof := readText(sbf)
		physical, _ := filepath.EvalSymlinks(lvm)
		switch {
		case sp == "" || countWith(prof, "network-bind") != 3:
			return errors.New("no ssh port, or the profile doesn't have three network-bind lines")
		case !HasLine(prof, "(deny network-bind)") || !HasLine(prof, `(allow network-bind (local tcp "localhost:`+sp+`"))`):
			return errors.New("the profile doesn't deny every bind but the ssh port's")
		case !HasLine(prof, `(allow network-bind (local unix-socket (subpath "`+physical+`")))`):
			return errors.New("the profile doesn't allow the VM's own unix sockets")
		}
		bind := fmt.Sprintf(`import socket; socket.socket().bind(("127.0.0.1", %d))`, s.Port(47197))
		udp := `import socket; socket.socket(socket.AF_INET, socket.SOCK_DGRAM).bind(("127.0.0.1", ` + sp + `))`
		for _, py := range []string{bind, udp} {
			if _, err := s.Run(Cmd{Args: []string{"python3", "-c", py}}); err != nil {
				return err
			}
			if _, err := s.Run(Cmd{Args: []string{"/usr/bin/sandbox-exec", "-f", sbf, "python3", "-c", py}}); err == nil {
				return errors.New("the profile let python3 bind: " + py)
			}
		}
		if l := haListens(); l != "TCP 127.0.0.1:"+sp {
			return errors.New("hostagent's: " + l)
		}
		if !hostResolverOff(readText(filepath.Join(lvm, "lima.yaml"))) {
			return errors.New("lima.yaml's hostResolver isn't off")
		}
		return nil
	})
	bypass := filepath.Join(s.sb.work, "lima-bypass")
	madeFile(s.sb, bypass)
	berr := limaBypass(bypass)
	s.Check(titles[1], func() error {
		if berr != nil {
			return berr
		}
		if _, err := n.dv("", n.home(), "stop"); err != nil {
			return err
		}
		if _, err := n.dv("exit\n", n.home("PATH="+bypass+":"+os.Getenv("PATH"))); err != nil {
			return err
		}
		switch {
		case s.Status(vm) != "Running":
			return errors.New("the VM isn't running")
		case !HasLine(readText(filepath.Join(lvm, "lima.yaml")), "portForwards: []") || s.Paths.State(vm)["ports.host.ssh"] != sp:
			return errors.New("Lima's own rule isn't on, or the ssh port changed")
		}
		err := inVM(s, vm, fmt.Sprintf(`setsid -f python3 -m http.server -b 0.0.0.0 %d </dev/null >/dev/null 2>&1; `+
			`setsid -f python3 -c 'import socket,time; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",%d)); time.sleep(600)' devvm-e2e-udp </dev/null >/dev/null 2>&1`, p95, p96))
		if err != nil {
			return err
		}
		err = s.Until(20*time.Second, func() error {
			t := readText(ha)
			if strings.Contains(t, fmt.Sprintf("listen tcp 127.0.0.1:%d: bind: operation not permitted", p95)) &&
				Matches(t, fmt.Sprintf(`listen udp [0-9.]+:%d: bind: operation not permitted`, p96)) {
				return nil
			}
			return errors.New("the hostagent's log doesn't show its binds refused")
		})
		switch {
		case err != nil:
			return err
		case !strings.Contains(readText(ha), fmt.Sprintf("Forwarding TCP from 0.0.0.0:%d", p95)):
			return errors.New("Lima didn't try to forward the server's port")
		case limaListens(s, "tcp", p95) || limaListens(s, "udp", p96):
			return errors.New("limactl listens on the server's port")
		}
		if l := haListens(); l != "TCP 127.0.0.1:"+sp {
			return errors.New("hostagent's: " + l)
		}
		return nil
	})
	s.Check(titles[2], func() error {
		switch {
		case !ReachesWithin(s, 20*time.Second, Local(p95)):
			return fmt.Errorf("nothing forwards %d", p95)
		case loopbackOnly(s, p95) != nil:
			return loopbackOnly(s, p95)
		case len(pids(s, "[_]ports -vm "+vm+" ")) == 0:
			return errors.New("no forwarder")
		case limaListens(s, "tcp", p95):
			return errors.New("limactl listens")
		case out1(s.Run(Cmd{Args: []string{"ssh", "-F", filepath.Join(s.Paths.Devvm, "ssh_config"), "devvm-net", "hostname"}})) != "net":
			return errors.New("ssh failed")
		case inVM(s, vm, "curl -fsS -m 20 -o /dev/null https://github.com") != nil:
			return errors.New("the proxy failed")
		case s.Paths.State(vm)["net.made.check"] != "ok":
			return fmt.Errorf("no self-check: %q", s.Paths.State(vm)["net.made.check"])
		}
		return nil
	})
	_ = inVM(s, vm, fmt.Sprintf("pkill -f '[h]ttp.server -b 0.0.0.0 %s'; pkill -f '[d]evvm-e2e-udp'", strconv.Itoa(p95)[:len(strconv.Itoa(p95))-1]))
	s.Check(titles[3], func() error {
		if _, err := n.dv("", n.home(), "stop"); err != nil {
			return err
		}
		if _, err := n.dv("exit\n", n.home()); err != nil {
			return err
		}
		r, _ := s.Limactl("list", "--format", "{{range .Config.PortForwards}}{{.GuestIP}}={{.Ignore}} {{end}}", vm)
		if got := strings.TrimRight(r.Stdout, "\n"); got != "127.0.0.1=true 0.0.0.0=true ::=true " {
			return fmt.Errorf("Lima's rules: %q", got)
		}
		if s.Paths.State(vm)["net.made.check"] != "ok" {
			return errors.New("the self-check didn't pass")
		}
		return nil
	})
}

// hostResolverOff is whether lima.yaml's hostResolver says enabled: false,
// on its line or the next (grep -A1).
func hostResolverOff(yaml string) bool {
	l := strings.Split(yaml, "\n")
	for i, line := range l {
		if strings.HasPrefix(line, "hostResolver:") {
			if strings.Contains(line, "enabled: false") || (i+1 < len(l) && strings.Contains(l[i+1], "enabled: false")) {
				return true
			}
		}
	}
	return false
}

// limaBypass writes dir/limactl: Lima's own rule for the start it runs,
// set where limactl start runs, inside devvm's sandbox, then the real
// limactl.
func limaBypass(dir string) error {
	lima, err := exec.LookPath("limactl")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	q := shell.Quote(lima)
	script := "#!/bin/bash\n# e2e: Lima's own rule for this start, set where limactl start runs: inside devvm's sandbox.\n" +
		"if [[ $1 == start ]]; then " + q + " edit --tty=false --set '.portForwards = []' \"${@: -1}\" >/dev/null 2>&1 || exit 1; fi\n" +
		"exec " + q + " \"$@\"\n"
	return os.WriteFile(filepath.Join(dir, "limactl"), []byte(script), 0o700) //nolint:gosec // G306: the stand-in runs
}

// Ctrl+C while Lima's start waits for the VM, its hostagent running it:
// devvm stops the VM and its proxy, so it never runs without the record
// of its confined start, and ls shows its policy on.
func netUnenforced(n *netRun) {
	s, vm := n.s, n.vm
	lsRe := func(status, policy string) string {
		return `^` + regexp.QuoteMeta(vm) + ` +` + status + ` +base +[a-z,]*claude[a-z,]* +off +` + policy + ` +- +/`
	}
	ls := func() string { r, _ := s.Devvm("", "ls"); return r.Stdout }
	_, _ = n.dv("", n.home(), "stop")
	rc, out := -1, ""
	if p, err := s.Start(devvmCmd(s, n.n, "", n.home(fakeToken, "DEVVM_MESSAGES=names"))); err == nil {
		sock := filepath.Join(s.sb.lima, vm, "ha.sock")
		for i := 0; i < 300 && !isSocket(sock); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		_ = p.cmd.Process.Signal(os.Interrupt)
		rc, _ = waitProc(p, 5*time.Minute)
		out = p.Out()
	}
	s.Check("Ctrl+C during its start stops it and its proxy, and ls shows its policy on", func() error {
		switch {
		case rc != 130:
			return fmt.Errorf("devvm exited %d, not 130:\n%s", rc, tailLines(out, 10))
		case !HasLine(out, "==> interrupted_start vm="+vm):
			return errors.New("devvm didn't say it stopped the start")
		case s.Status(vm) != "Stopped" || proxyRuns(s, n.pxc):
			return errors.New("the VM or its proxy runs")
		case !Matches(ls(), lsRe("Stopped", "allowlist")):
			return errors.New("devvm ls doesn't show it stopped, its policy on")
		}
		return nil
	})
	_, _ = n.dv("exit\n", n.home(fakeToken))
	outside := func() error {
		if _, err := s.Limactl("stop", vm); err != nil {
			return err
		}
		_, err := s.Limactl("start", "--tty=false", vm)
		return err
	}
	stopped := FirstLine(s.Msg("net_unenforced_stop", "vm="+vm))
	s.Check("ls shows its policy on, and unenforced once started outside devvm", func() error {
		if !Matches(ls(), lsRe("Running", "allowlist")) {
			return errors.New("devvm ls doesn't show it running, its policy on")
		}
		if err := outside(); err != nil {
			return err
		}
		if !Matches(ls(), lsRe("Running", "unenforced")) {
			return errors.New("devvm ls doesn't show it unenforced")
		}
		return nil
	})
	s.Check("started outside devvm again: without a terminal, devvm stops it and opens nothing", func() error {
		if err := outside(); err != nil {
			return err
		}
		r, err := n.dv("echo e2e-opened\n", n.home())
		switch {
		case err == nil || !grepF(r.Out, stopped):
			return errors.New("devvm didn't fail, saying it stopped the VM")
		case strings.Contains(r.Out, "e2e-opened") || s.Status(vm) != "Stopped":
			return errors.New("devvm opened a shell, or the VM runs")
		}
		return nil
	})
	s.Check("so does devvm host, which names nothing", func() error {
		if _, err := s.Limactl("start", "--tty=false", vm); err != nil {
			return err
		}
		r, err := n.dv("", n.home(), "host")
		switch {
		case err == nil || !grepF(r.Out, stopped):
			return errors.New("devvm host didn't fail, saying it stopped the VM")
		case grepF(r.Out, FirstLine(s.Msg("ssh_host_info", "dir="+n.n, "host=devvm-net"))) || s.Status(vm) != "Stopped":
			return errors.New("devvm host named the VM, or it runs")
		}
		return nil
	})
}

// isSocket is whether f is a unix socket.
func isSocket(f string) bool {
	st, err := os.Stat(f)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// A server on this machine, on every address, and this machine's LAN
// address, for the tiers' checks: an open VM reaches both through its
// proxy, a confined one neither.
func netOpen(n *netRun) {
	s, vm := n.s, n.vm
	lp := strconv.Itoa(s.Port(47198))
	lan, lerr := s.Start(Cmd{Args: []string{"python3", "-I", "-c", lanServerPy, lp}})
	if lerr == nil {
		lerr = lan.listening(s.r.window().ctx, "127.0.0.1:"+lp, 15*time.Second)
	}
	n.lan = lanAddr(s.r.window().ctx)
	s.r.out.line("  e2e: this machine's LAN address: %s", cmp.Or(n.lan, "none"))
	n.lanServer = lan
	proxied := `^http://devvm:[0-9a-f]*@192\.168\.5\.2:` + n.pp + `$`
	// Stopped by the self-check, it's still unenforced: its last self-check
	// failed. Open from its next start: unconfined, so a direct connection
	// works, and its proxy, an open one, set in it and answering its
	// self-check, reaches this machine's loopback and its LAN address for
	// it.
	s.Check("devvm network open: its next start is open: direct connections work, its proxy is set and answers, and through it the VM reaches this machine's loopback and LAN", func() error {
		if lerr != nil {
			return fmt.Errorf("the LAN server: %w", lerr)
		}
		if n.tier() != "unenforced" {
			return errors.New("devvm network doesn't say unenforced")
		}
		if _, err := s.Devvm(n.n, "network", "open"); err != nil {
			return err
		}
		r, err := n.dv("exit\n", n.home(fakeToken, "DEVVM_MESSAGES=names"))
		switch {
		case err != nil:
			return err
		case n.tier() != "open" || strings.Contains(r.Out, "net_proxy_unreachable") || s.Status(vm) != "Running":
			return errors.New("it isn't open and running, or its proxy is unreachable")
		case !proxyRuns(s, n.pxc) || !strings.Contains(readText(n.pxc), `"open": true`) || !strings.Contains(readText(n.pxc), `"ca"`):
			return errors.New("its proxy doesn't run open, with its CA")
		}
		if _, err := vmRun(s, vm, "curl", "--noproxy", "*", "-sS", "-m", "10", "-o", "/dev/null", "https://1.1.1.1"); err != nil {
			return errors.New("a direct connection failed")
		}
		switch {
		case !vmGrep(s, vm, "printenv HTTPS_PROXY", proxied):
			return errors.New("the guest's HTTPS_PROXY isn't its proxy")
		case vmOut(s, vm, `curl -fsS -m 10 --noproxy "" http://127.0.0.1:`+lp+"/") != "host-lan":
			return errors.New("the VM didn't reach this machine's loopback through its proxy")
		case n.lan == "" || vmOut(s, vm, "curl -fsS -m 10 http://"+n.lan+":"+lp+"/") != "host-lan":
			return errors.New("the VM didn't reach this machine's LAN address")
		}
		if r, _ := s.Devvm(n.n, "network", "log"); !strings.Contains(r.Stdout, "  allowed  127.0.0.1:"+lp+" ") {
			return errors.New("devvm network log doesn't show the loopback allowed")
		}
		return nil
	})
	// Open, it holds no real secret either: the token is nowhere in it,
	// its environment and Codex's login hold only its own placeholders,
	// and its proxy, whose certificates it verifies with its own CA (no
	// -k), puts the token in. A request that goes around the proxy
	// carries only the placeholder, which the token's host refuses with a
	// 401, and the proxy never sees it.
	s.Check("an open VM holds no real secret: its proxy puts the token in, and a direct request carries only the placeholder and gets a 401", func() error {
		if err := GuestHasNone(s, vm, []string{"e2efake"}, "/etc", "/home", "/root"); err != nil {
			return err
		}
		if has, err := vmHas(s, vm, true, "env", "e2efake"); err != nil || has {
			return failed("the guest's environment holds the token", err)
		}
		c, _ := vmRun(s, vm, "bash", "-c", "cat ~/.codex/auth.json")
		switch {
		case vmOut(s, vm, "printenv CLAUDE_CODE_OAUTH_TOKEN") != "sk-ant-oat01-devvm-proxy-managed-"+n.salt:
			return errors.New("the guest's token isn't its placeholder")
		case !strings.Contains(c.Stdout, "devvm-proxy-managed"):
			return errors.New("Codex's login holds no placeholder")
		case inVM(s, vm, `curl -sS -m 20 -o /dev/null https://api.anthropic.com/v1/models/devvm-e2e-open -H "Authorization: Bearer $CLAUDE_CODE_OAUTH_TOKEN"`) != nil:
			return errors.New("the request through the proxy failed")
		case !lineWith(readText(n.events), `"path":"/v1/models/devvm-e2e-open"`, `"secrets":["claude"]`):
			return errors.New("no event shows the claude secret put in")
		}
		code := vmOut(s, vm, `curl --noproxy "*" -sS -m 20 -o /dev/null -w %{http_code} https://api.anthropic.com/v1/models/devvm-e2e-direct -H "Authorization: Bearer $CLAUDE_CODE_OAUTH_TOKEN"`)
		if code != "401" || strings.Contains(readText(n.events), "devvm-e2e-direct") {
			return fmt.Errorf("direct: %s, or the proxy saw it", code)
		}
		return nil
	})
	// Open, behind the run's company proxy on this machine's loopback, its
	// password fake: the shell's settings reach the VM's proxy, which goes
	// through the company proxy, and never the guest or Lima. The password
	// is in devvm's environment alone, in no command's arguments.
	corp, cerr := StartCorp(s)
	s.Check("open behind a company proxy: the guest gets its own proxy, never the shell's settings, its proxy goes through the company proxy, and no log or guest file keeps the password", func() error {
		if cerr != nil {
			return cerr
		}
		r, err := n.dv("exit\n", n.home(corp.Env()...), "restart")
		switch {
		case err != nil:
			return err
		case n.tier() != "open":
			return errors.New("it isn't open")
		case !vmGrep(s, vm, "printenv HTTPS_PROXY", proxied):
			return errors.New("the guest's HTTPS_PROXY isn't its own proxy")
		case inVM(s, vm, "curl -fsS -m 30 -o /dev/null https://github.com") != nil:
			return errors.New("github.com in the VM")
		case !corp.Saw(`"event":"tunnel","host":"github.com","port":443`):
			return errors.New("the company proxy didn't carry github.com")
		case strings.Contains(r.Out, corp.Password()):
			return errors.New("devvm restart printed the password")
		}
		lvm := filepath.Join(s.sb.lima, vm)
		pw := []string{corp.Password()}
		if err := NoLeak(pw, filepath.Join(s.Paths.Devvm, "logs"), filepath.Join(n.vmd, "logs"), filepath.Join(lvm, "ha.stderr.log"), filepath.Join(lvm, "lima.yaml")); err != nil {
			return err
		}
		return GuestHasNone(s, vm, pw, "/etc", "/root", "/home")
	}, Tail(60))
	if corp != nil {
		corp.Stop()
	}
}

func netTiers(n *netRun) {
	s, vm := n.s, n.vm
	lp := strconv.Itoa(s.Port(47198))
	direct := func() bool {
		_, err := vmRun(s, vm, "curl", "--noproxy", "*", "-sS", "-m", "5", "-o", "/dev/null", "https://1.1.1.1")
		return err == nil
	}
	code := func(u string) string { return vmOut(s, vm, "curl -s -m 20 -o /dev/null -w %{http_code} "+u) }
	s.Check("devvm network allowlist while it runs, without a terminal: unenforced until devvm restart starts it confined", func() error {
		r, err := s.Devvm(n.n, "network", "allowlist")
		if err != nil || !grepF(r.Out, FirstLine(s.Msg("next_start", "count=1", "vm="+vm))) {
			return errors.New("devvm network allowlist didn't say it applies at the next start")
		}
		if n.tier() != "unenforced" {
			return errors.New("devvm network doesn't say unenforced")
		}
		if _, err := n.dv("exit\n", n.home(), "restart"); err != nil {
			return err
		}
		switch {
		case n.tier() != "allowlist" || s.Paths.State(vm)["net.made.check"] != "ok" || !proxyRuns(s, n.pxc):
			return errors.New("it isn't confined, its self-check passed and its proxy running")
		case direct():
			return errors.New("a direct connection worked")
		case vmOut(s, vm, `curl -s -m 10 -o /dev/null -w %{http_code} --noproxy "" http://127.0.0.1:`+lp+"/") != "403":
			return errors.New("the proxy didn't refuse this machine's loopback")
		}
		return nil
	})
	// Between confined tiers devvm network applies the change at once: it
	// starts the proxy again, the VM running on. internet: every public
	// host on any port, never this machine's addresses.
	s.Check("devvm network internet: at once, a host off the allowlist and another port work through its proxy, this machine's LAN address doesn't, and a direct connection still fails", func() error {
		if _, err := n.dv("", n.home(), "network", "internet"); err != nil {
			return err
		}
		cfg := readText(n.pxc)
		switch {
		case s.Status(vm) != "Running" || n.tier() != "internet":
			return errors.New("it isn't running on the internet tier")
		case !strings.Contains(cfg, "network/internet.policy") || !strings.Contains(cfg, `"deny_policies"`):
			return errors.New("its proxy's config has no internet policy or deny policies")
		case inVM(s, vm, "curl -fsS -m 20 -o /dev/null https://example.com") != nil:
			return errors.New("example.com failed")
		}
		if _, err := vmRun(s, vm, "bash", "-c", "timeout 10 /usr/local/bin/devvm-ssh-proxy one.one.one.one 53 </dev/null; [ $? -ne 1 ]"); err != nil {
			return errors.New("another port failed")
		}
		switch {
		case vmOut(s, vm, "curl -s -m 10 -o /dev/null -w %{http_code} http://"+n.lan+":"+lp+"/") != "403":
			return errors.New("this machine's LAN address wasn't refused")
		case direct():
			return errors.New("a direct connection worked")
		}
		return nil
	})
	// none: its agents' hosts alone, and your denies on top.
	s.Check("devvm network none: github.com is refused, Claude Code's API isn't, and a deny of yours holds", func() error {
		if _, err := n.dv("", n.home(), "network", "none"); err != nil {
			return err
		}
		switch {
		case n.tier() != "none":
			return errors.New("it isn't on the none tier")
		case code("http://github.com/") != "403":
			return errors.New("github.com wasn't refused")
		case code("https://api.anthropic.com/") == "000":
			return errors.New("Claude Code's API was refused")
		}
		if _, err := s.Devvm(n.n, "network", "deny", "api.anthropic.com"); err != nil {
			return err
		}
		if code("https://api.anthropic.com/") != "000" {
			return errors.New("your deny didn't hold")
		}
		_, err := s.Devvm(n.n, "network", "rm", "!api.anthropic.com")
		return err
	})
	_, _ = n.dv("", n.home(), "network", "allowlist")
	if n.lanServer != nil {
		n.lanServer.Stop()
	}
	s.Check("devvm network open while it runs confined, without a terminal: launched, it keeps its proxy until it next starts", func() error {
		r, err := s.Devvm(n.n, "network", "open")
		if err != nil || !grepF(r.Out, FirstLine(s.Msg("next_start", "count=1", "vm="+vm))) {
			return errors.New("devvm network open didn't say it applies at the next start")
		}
		r, err = n.dv("exit\n", n.home())
		switch {
		case err != nil || !grepF(r.Out, FirstLine(s.Msg("next_start_net_off", "vm="+vm))):
			return errors.New("the launch didn't say it keeps its proxy until its next start")
		case !proxyRuns(s, n.pxc) || direct():
			return errors.New("its proxy doesn't run, or a direct connection worked")
		}
		return nil
	})
	s.Check("rm --force deletes it, its proxy and its state", func() error {
		if _, err := n.dv("", n.home(), "rm", "--force", "--no-export"); err != nil {
			return err
		}
		r, _ := s.Limactl("list", "-q")
		if r.Has(vm) || Exists(n.vmd) || proxyRuns(s, n.pxc) {
			return errors.New("the VM, its folder or its proxy is still there")
		}
		return nil
	})
}

// lanServerPy answers every request on its port, every address, with
// host-lan.
const lanServerPy = `import socketserver, sys
class H(socketserver.StreamRequestHandler):
    def handle(self):
        while self.rfile.readline() not in (b"\r\n", b"\n", b""):
            pass
        self.wfile.write(b"HTTP/1.0 200 OK\r\nContent-Length: 9\r\n\r\nhost-lan\n")
socketserver.ThreadingTCPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer(("", int(sys.argv[1])), H).serve_forever()
`

// fakeTokenPy is the fake token endpoints: a company proxy that answers a
// CONNECT to auth.openai.com, platform.claude.com or api.anthropic.com
// itself, with its certificate (port, cert, key and its log are its
// arguments), and refuses any other.
//
//nolint:gosec // G101: its tokens are fakes, which no service takes
const fakeTokenPy = `import base64, json, socket, ssl, sys, threading, urllib.parse
port, cert, key, log = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
ctx.set_alpn_protocols(["http/1.1"])
def b64(d): return base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
def jwt(c): return b64({"alg": "none"}) + "." + b64(c) + ".e2efake-sig"
auth = {"chatgpt_account_id": "acct-e2e", "chatgpt_plan_type": "plus"}
ID = jwt({"email": "e2e@devvm.test", "exp": 4070908800, "https://api.openai.com/auth": auth})
AT = jwt({"exp": 4070908800, "https://api.openai.com/auth": auth, "e2efake": "access"})
def head(f):
    lines = []
    while True:
        l = f.readline()
        if l in (b"\r\n", b"\n", b""):
            return lines
        lines.append(l.decode().strip())
def answer(q):
    g = q.get("grant_type", [""])[0]
    if g == "authorization_code":
        return {"id_token": ID, "access_token": AT, "refresh_token": "rt-e2efake-capture", "expires_in": 3600, "token_type": "Bearer"}
    if g.endswith("token-exchange") and q.get("subject_token", [""])[0] == ID:
        return {"access_token": "sk-e2efake-captured-key"}
    return None
# The device code login's two requests before its code's, as opencode 2's Codex device code login
# sends them (JSON): the code to show, then the code's exchange, answered at once.
def device(path):
    if path == "/api/accounts/deviceauth/usercode":
        return {"device_auth_id": "e2e-device", "user_code": "E2E-0000", "interval": "1"}
    if path == "/api/accounts/deviceauth/token":
        return {"authorization_code": "e2e-code", "code_verifier": "e2e"}
    return None
# Claude's token endpoint, as Claude Code 2.1's bundle has it: JSON, claude setup-token's code with
# expires_in, and a subscription login's code, each answered with tokens holding e2efake.
CLAUDE_ACCOUNT = {"account": {"uuid": "acct-e2e", "email_address": "e2e@devvm.test"}, "organization": {"uuid": "org-e2e"}}
def claude(q):
    g = q.get("grant_type", "")
    if g == "authorization_code" and "expires_in" in q:
        return dict(CLAUDE_ACCOUNT, access_token="sk-ant-oat01-e2efake-setup", expires_in=31536000, scope="user:inference", token_type="Bearer")
    if g == "authorization_code":
        return dict(CLAUDE_ACCOUNT, access_token="sk-ant-oat01-e2efake-oauth", refresh_token="sk-ant-ort01-e2efake-oauth",
                    expires_in=28800, scope="user:inference user:profile", token_type="Bearer")
    if g == "refresh_token" and q.get("refresh_token") == "sk-ant-ort01-e2efake-oauth":
        return dict(access_token="sk-ant-oat01-e2efake-refreshed", refresh_token="sk-ant-ort01-e2efake-refreshed",
                    expires_in=28800, scope="user:inference user:profile", token_type="Bearer")
    return None
# Anthropic's API: the profile Claude Code reads as it logs in, and the Authorization of each request
# logged, and its x-api-key when it has one (an API key's), so a check sees which token the VM's
# proxy put in.
def api(path, h):
    a = [x.split(":", 1)[1].strip() for x in h[1:] if x.lower().startswith("authorization:")]
    k = [x.split(":", 1)[1].strip() for x in h[1:] if x.lower().startswith("x-api-key:")]
    with open(log, "a") as o:
        o.write("api " + path + " " + (a[0] if a else "-") + (" x-api-key " + k[0] if k else "") + "\n")
    if path.startswith("/api/oauth/profile"):
        return 200, {"account": {"uuid": "acct-e2e", "email": "e2e@devvm.test", "display_name": "e2e"},
                     "organization": {"uuid": "org-e2e", "organization_type": "claude_max", "rate_limit_tier": "default_claude_max_20x"}}
    return 404, {"type": "error", "error": {"type": "not_found_error", "message": "e2e"}}
HOSTS = ("auth.openai.com", "platform.claude.com", "api.anthropic.com")
def serve(c):
    try:
        req = head(c.makefile("rb"))
        host = req[0].split(" ")[1].rsplit(":", 1)[0] if req and req[0].startswith("CONNECT ") else ""
        if host not in HOSTS or not req[0].startswith("CONNECT " + host + ":443 "):
            c.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
            return
        c.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
        if host == "platform.claude.com":
            with open(log, "a") as o:
                o.write("CONNECT " + host + "\n")
        t = ctx.wrap_socket(c, server_side=True)
        f = t.makefile("rb")
        while True:
            h = head(f)
            if not h:
                return
            n = sum(int(x.split(":", 1)[1]) for x in h[1:] if x.lower().startswith("content-length:"))
            body = f.read(n).decode()
            code = 200
            if host == "api.anthropic.com":
                code, r = api(h[0].split(" ")[1], h)
            elif host == "platform.claude.com":
                q = json.loads(body) if body.startswith("{") else {}
                with open(log, "a") as o:
                    o.write(h[0] + " " + str(q.get("grant_type", "")) + (" expires_in" if "expires_in" in q else "") + "\n")
                r = claude(q)
            elif h[0].split(" ")[1].startswith("/api/accounts/deviceauth/"):
                with open(log, "a") as o:
                    o.write(h[0] + "\n")
                r = device(h[0].split(" ")[1])
            else:
                with open(log, "a") as o:
                    o.write(h[0] + " " + urllib.parse.parse_qs(body).get("grant_type", [""])[0] + "\n")
                r = answer(urllib.parse.parse_qs(body))
            if r is None:
                code, r = 400, {"error": "invalid_grant"}
            out = json.dumps(r).encode()
            t.sendall(b"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n" %
                      (code, b"OK" if code == 200 else b"Error", len(out)) + out)
    except Exception as e:
        print(e, file=sys.stderr)
    finally:
        c.close()
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen(16)
while True:
    c, _ = s.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
`
