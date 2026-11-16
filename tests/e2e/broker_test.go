//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/proxy"
)

// The proxy's broker and its child: a VM's proxy runs as two processes, devvm _proxy -config, the
// broker, and devvm _proxy-child -for, its child, which reads the VM's network with nothing but
// its descriptors and four variables. They end together, and neither starts the other again.

// childPattern is the pgrep pattern of the child of the broker whose config is pxc.
func childPattern(pxc string) string { return "[_]proxy-child -for " + pxc }

// pair is the pids of the broker and the child of the proxy whose config is pxc, each "" for none.
func brokerPair(s *S, pxc string) (broker, child string) {
	b, c := pids(s, "[_]proxy -config "+pxc), pids(s, childPattern(pxc))
	if len(b) > 0 {
		broker = b[0]
	}
	if len(c) > 0 {
		child = c[0]
	}
	return broker, child
}

// until reports whether ok holds within d, looking every 50 ms.
func until(d time.Duration, ok func() bool) bool {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if ok() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// childEnvOK reports why env, the child's environment as KEY=VALUE, has more than TZ,
// SSL_CERT_FILE, SSL_CERT_DIR and GODEBUG, or a GODEBUG without updatemaxprocs=0: "" when it
// hasn't.
func childEnvOK(env []string) string {
	debug := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR":
		case "GODEBUG":
			debug = slices.Contains(strings.Split(v, ","), "updatemaxprocs=0")
		default:
			return "the child's environment has " + k
		}
	}
	if !debug {
		return "the child's GODEBUG lacks updatemaxprocs=0"
	}
	return ""
}

// childEnv is the child pid's environment: /proc's on Linux, ps -E's words after its arguments on
// macOS, where a value with a space would read as two.
func childEnv(s *S, pid string, args int) ([]string, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
		if err != nil {
			return nil, err
		}
		return strings.FieldsFunc(string(b), func(r rune) bool { return r == 0 }), nil
	}
	r, err := s.Run(Cmd{Args: []string{"ps", "-E", "-ww", "-o", "command=", "-p", pid}})
	if err != nil {
		return nil, err
	}
	f := strings.Fields(r.Stdout)
	if len(f) < args {
		return nil, errors.New("ps -E shows less than the child's arguments")
	}
	return f[args:], nil
}

// childFDs checks the child's descriptors 0 to 4 on Linux: the null device, its log twice, and
// two sockets, the broker's and the listener.
func childFDs(pid, log string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	want := []string{"/dev/null", log, log, "socket:", "socket:"}
	for i, w := range want {
		got, err := os.Readlink(filepath.Join("/proc", pid, "fd", fmt.Sprint(i)))
		if err != nil || !strings.HasPrefix(got, w) {
			return fmt.Errorf("the child's descriptor %d is %q, want %q: %w", i, got, w, err)
		}
	}
	return nil
}

// childConfined checks how the broker whose run folder is run says its child is confined: on
// macOS, by Seatbelt in full, which says the child started under sandbox-exec and its own check
// that the sandbox refuses it the run folder held.
func childConfined(run string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if got := strings.TrimSpace(readText(filepath.Join(run, "proxy.confined"))); got != "full seatbelt" {
		return fmt.Errorf("run/proxy.confined says %q, not full seatbelt", got)
	}
	return nil
}

// netBroker checks the network VM's proxy runs as a broker and a child, and that they end
// together: a stop leaves neither, a killed broker takes its child, and a killed child its
// broker; things planted in the VM's logs folder lead nowhere. It launches the VM again after.
func netBroker(n *netRun) {
	s := n.s
	s.Check("the VM's proxy runs as a broker and a child, the child with its arguments, descriptors and environment alone", func() error {
		b, c := brokerPair(s, n.pxc)
		if b == "" || c == "" {
			return errors.New("no broker, or no child")
		}
		if pp := out1(s.Run(Cmd{Args: []string{"ps", "-o", "ppid=", "-p", c}})); pp != b {
			return fmt.Errorf("the child's parent is %s, not the broker %s", pp, b)
		}
		r, _ := s.Run(Cmd{Args: []string{"ps", "-ww", "-o", "command=", "-p", c}})
		if f := strings.Fields(r.Stdout); len(f) != 4 || strings.Join(f[1:], " ") != "_proxy-child -for "+n.pxc {
			return fmt.Errorf("the child's arguments: %q", r.Stdout)
		}
		run := filepath.Dir(n.pxc)
		switch {
		case strings.TrimSpace(readText(filepath.Join(run, "proxy-child.pid"))) != c:
			return errors.New("run/proxy-child.pid doesn't name the child")
		case strings.TrimSpace(readText(filepath.Join(run, "proxy.confined"))) == "":
			return errors.New("run/proxy.confined says nothing")
		}
		if err := childConfined(run); err != nil {
			return err
		}
		env, err := childEnv(s, c, 4)
		if err != nil {
			return err
		}
		if why := childEnvOK(env); why != "" {
			return errors.New(why)
		}
		return childFDs(c, filepath.Join(n.vmd, "logs", "proxy.log"))
	})
	if runtime.GOOS == "linux" {
		s.Check("on Linux the child is confined: Landlock and the seccomp filter in run/proxy.confined, no_new_privs and a filter in its /proc entry", func() error {
			_, c := brokerPair(s, n.pxc)
			said := strings.TrimSpace(readText(filepath.Join(filepath.Dir(n.pxc), "proxy.confined")))
			if !strings.HasPrefix(said, "full landlock") && !strings.HasPrefix(said, "partial landlock landlock-") {
				return fmt.Errorf("run/proxy.confined says %q", said)
			}
			status := readText(filepath.Join("/proc", c, "status"))
			if !strings.Contains(status, "\nNoNewPrivs:\t1\n") || !strings.Contains(status, "\nSeccomp:\t2\n") {
				return fmt.Errorf("the child's status: %q", status)
			}
			return nil
		})
	}
	s.Check("devvm stop leaves neither process, and the next launch starts both on the same port at once", func() error {
		if _, err := s.Devvm(n.n, "stop"); err != nil {
			return err
		}
		if b, c := brokerPair(s, n.pxc); b != "" || c != "" {
			return fmt.Errorf("after the stop: broker %q, child %q", b, c)
		}
		if _, err := n.dv("exit\n", n.tokenEnv()); err != nil {
			return err
		}
		b, c := brokerPair(s, n.pxc)
		switch {
		case b == "" || c == "":
			return errors.New("the launch started no broker, or no child")
		case !strings.Contains(readText(n.pxc), `"listen": "127.0.0.1:`+n.pp+`"`):
			return errors.New("the proxy moved off its port")
		}
		return nil
	})
	s.Check("a broker killed with SIGKILL takes its child with it within a second, and the next launch starts both", func() error {
		b, c := brokerPair(s, n.pxc)
		if b == "" || c == "" {
			return errNo
		}
		if err := kill(b); err != nil {
			return err
		}
		if !until(1500*time.Millisecond, func() bool { return len(pids(s, childPattern(n.pxc))) == 0 }) {
			return errors.New("the child outlived its killed broker")
		}
		return relaunch(n)
	})
	s.Check("a child killed takes its broker and the agent's relay with it, and nothing starts them again until the next launch", func() error {
		b, c := brokerPair(s, n.pxc)
		if b == "" || c == "" {
			return errNo
		}
		cfg, err := proxy.ReadConfig(n.pxc)
		if err != nil {
			return err
		}
		if err := kill(c); err != nil {
			return err
		}
		if !until(3*time.Second, func() bool { return !proxyLeft(s, n.pxc) }) {
			return errors.New("the broker outlived its killed child")
		}
		time.Sleep(2 * time.Second)
		if b2, c2 := brokerPair(s, n.pxc); b2 != "" || c2 != "" {
			return errors.New("something started the proxy again")
		}
		if cfg.AgentSocket != "" && Exists(cfg.AgentSocket) {
			return errors.New("the agent's relay outlived its broker: " + cfg.AgentSocket)
		}
		return relaunch(n)
	})
	s.Check("a link, a FIFO and a hard link planted in the VM's logs folder lead the next start and devvm network log nowhere, and hang neither", func() error {
		return plantedLogs(n)
	})
}

// plantedLogs plants what a VM's proxy's child could leave in its logs folder, a link to a file of
// the run's, a FIFO and a hard link of that file, and checks the next start makes logs of its own
// in their place, leaving the file as it was, and devvm network log refuses a link planted while
// the proxy runs, at once.
func plantedLogs(n *netRun) error {
	s := n.s
	mine := filepath.Join(s.sb.work, "broker-mine")
	if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
		return err
	}
	if _, err := s.Devvm(n.n, "stop"); err != nil {
		return err
	}
	logs := filepath.Join(n.vmd, "logs")
	defer func() { _ = os.Remove(filepath.Join(logs, "network.jsonl.1")) }()
	for _, f := range []string{"proxy.log", "network.jsonl", "network.jsonl.1"} {
		_ = os.Remove(filepath.Join(logs, f))
	}
	if err := errors.Join(os.Symlink(mine, filepath.Join(logs, "proxy.log")),
		syscall.Mkfifo(filepath.Join(logs, "network.jsonl"), 0o600),
		os.Link(mine, filepath.Join(logs, "network.jsonl.1"))); err != nil {
		return err
	}
	if err := relaunch(n); err != nil {
		return err
	}
	if b := readText(mine); b != "mine\n" {
		return fmt.Errorf("the planted link's target changed: %q", b)
	}
	for _, f := range []string{"proxy.log", "network.jsonl"} {
		fi, err := os.Lstat(filepath.Join(logs, f))
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s isn't a log of its own again: %w", f, err)
		}
	}
	// What the VM's child can plant while it runs: devvm network log refuses it, at once.
	_ = os.Remove(filepath.Join(logs, "network.jsonl"))
	if err := os.Symlink(mine, filepath.Join(logs, "network.jsonl")); err != nil {
		return err
	}
	start := time.Now()
	r, _ := s.Devvm(n.n, "network", "log")
	if strings.Contains(r.Stdout, "mine") || time.Since(start) > 10*time.Second {
		return errors.New("devvm network log read the planted link's target, or hung")
	}
	_ = os.Remove(filepath.Join(logs, "network.jsonl"))
	return relaunch(n)
}

// kill sends pid SIGKILL.
func kill(pid string) error {
	var p int
	if _, err := fmt.Sscan(pid, &p); err != nil {
		return err
	}
	return syscall.Kill(p, syscall.SIGKILL)
}

// relaunch launches the network VM again, as the token checks do, and checks both its proxy's
// processes run.
func relaunch(n *netRun) error {
	if _, err := n.dv("exit\n", n.tokenEnv()); err != nil {
		return err
	}
	if b, c := brokerPair(n.s, n.pxc); b == "" || c == "" {
		return errors.New("the launch started no broker, or no child")
	}
	return nil
}
