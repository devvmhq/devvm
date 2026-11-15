//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
)

// WriteProxyConfig writes c as devvm _proxy -config's file f, the run's
// alone (mode 600), as devvm writes a VM's: its password is in the file,
// never in the proxy's arguments. It's deleted at the run's end.
func WriteProxyConfig(s *S, f string, c proxy.Config) error {
	madeFile(s.sb, f)
	if err := os.WriteFile(f, c.Encode(), 0o600); err != nil {
		return err
	}
	return os.Chmod(f, 0o600) // an earlier run's file keeps its mode otherwise
}

// madeFile records f as one of the run's files, deleted at its end.
func madeFile(sb *sandbox, f string) {
	if !slices.Contains(sb.made, f) {
		sb.made = append(sb.made, f)
	}
}

// corpPort is the company proxy's port in slot 0.
const corpPort = 18766

// Corp is a company proxy of the run's: a
// devvm _proxy on this machine's 127.0.0.1, its port slot 0's 18766 as
// this slot has it, with a password of its own, allowing any host, its
// events in Events. The password is in its config, mode 600, and in the
// environment of the commands a check gives Env: never in a process's
// arguments, which other users can see, so a check that looks for it
// there finds only what devvm put there.
type Corp struct {
	Events string // its events, one JSON object a line
	pw     string
	addr   string
	proc   *Proc
}

// StartCorp starts the company proxy and waits for it to listen. Stop
// ends it, and so does the run's end.
func StartCorp(s *S) (*Corp, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	c := &Corp{
		Events: filepath.Join(s.sb.work, "corp.jsonl"), pw: "e2e-corp-pw-" + hex.EncodeToString(b),
		addr: "127.0.0.1:" + strconv.Itoa(s.Port(corpPort)),
	}
	cfg, pol := filepath.Join(s.sb.work, "corp.json"), filepath.Join(s.sb.work, "corp.policy")
	madeFile(s.sb, pol)
	madeFile(s.sb, c.Events)
	if err := os.WriteFile(pol, []byte("**\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.Events, nil, 0o600); err != nil {
		return nil, err
	}
	err := WriteProxyConfig(s, cfg, proxy.Config{VM: "e2e-corp", Listen: c.addr, Password: redact.NewSecret(c.pw), Policies: []string{pol}, Log: c.Events})
	if err != nil {
		return nil, err
	}
	if c.proc, err = s.Start(Cmd{Args: []string{s.sb.devvm, "_proxy", "-config", cfg}}); err != nil {
		return nil, err
	}
	if err := c.proc.listening(s.r.window().ctx, c.addr, 15*time.Second); err != nil {
		c.proc.Stop()
		return nil, fmt.Errorf("the company proxy: %w", err)
	}
	return c, nil
}

// Password is its password, for a check that looks for it: in files
// (NoLeak), a VM (GuestHasNone) or processes (ProcsHave).
func (c *Corp) Password() string { return c.pw }

// Addr is where it listens, 127.0.0.1:PORT, without its password: what a
// log may name.
func (c *Corp) Addr() string { return c.addr }

// URL is its address with its password, as HTTPS_PROXY has it.
func (c *Corp) URL() string { return "http://" + proxy.ProxyUser + ":" + c.pw + "@" + c.addr }

// Env is HTTPS_PROXY and HTTP_PROXY set to it, for a Cmd's Env: a
// command's environment, not its arguments.
func (c *Corp) Env() []string { return []string{"HTTPS_PROXY=" + c.URL(), "HTTP_PROXY=" + c.URL()} }

// Saw is whether one of its events matches the regular expression re.
func (c *Corp) Saw(re string) bool {
	b, err := os.ReadFile(c.Events)
	return err == nil && Matches(string(b), re)
}

// Running is whether it still runs.
func (c *Corp) Running() bool { return c.proc.Running() }

// Log is what the company proxy printed, its errors among it.
func (c *Corp) Log() string { return c.proc.Out() }

// Stop ends it; its events stay until the
// run's end.
func (c *Corp) Stop() { c.proc.Stop() }

// GuestHasNone fails when a file under one of dirs in the running VM
// holds one of secrets, searched as root: the secrets go on grep's input,
// never in a command line, where sudo would log them in the VM's journal.
func GuestHasNone(s *S, vm string, secrets []string, dirs ...string) error {
	if len(secrets) == 0 || slices.Contains(secrets, "") {
		return errors.New("GuestHasNone: no secret, or an empty one")
	}
	args := append([]string{"limactl", "shell", "--workdir", "/", vm, "sudo", "grep", "-rqsF", "-f", "/dev/stdin", "--"}, dirs...)
	_, err := s.Run(Cmd{Args: args, Stdin: strings.Join(secrets, "\n") + "\n"})
	var ee *exec.ExitError
	switch {
	case err == nil:
		return fmt.Errorf("a file under %s in %s holds a secret", strings.Join(dirs, " "), vm)
	case errors.As(err, &ee) && (ee.ExitCode() == 1 || ee.ExitCode() == 2):
		return nil // none found (2: some file grep couldn't read, and none found in the rest)
	}
	return err
}
