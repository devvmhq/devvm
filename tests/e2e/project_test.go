//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
)

// projectSection: a base project VM (its launch, mounts, ports, a
// company's CA, ssh by name, stopping and starting it, its project
// folder's cache mode, .git protection,
// a nested project folder replaced by a link, moving its folder), then a devcontainer.json's VM, agents' folders and
// history, SSH agents, and devvm layer add, update and layer rm.
func projectSection(s *S) {
	w := projectBase(s)
	projectCA(s, w)
	projectSSH(s, w)
	projectGit(s, w)
	projectExisting(s, w)
	projectMountCache(s, w)
	projectPaste(s, w)
	projectPorts(s, w)
	projectGitProtect(s, w)
	projectNested(s, w)
	projectNestedRepos(s)
	projectMoved(s, w)
	projectDevcontainer(s)
	projectAgents(s)
	projectSSHAgents(s)
	projectLayerAdd(s)
}

// webVM is the base project's VM, which the section's first parts share.
type webVM struct {
	p, vm  string
	launch string // its launch's output, for the .git protection checks
	sc     string // the ssh checks' folder: their config, agent and home
	kh     string // its pinned host keys
}

func (w *webVM) state(s *S) map[string]string { return s.Paths.State(w.vm) }

// Itoa is strconv.Itoa, for the ports in commands.
func Itoa(n int) string { return strconv.Itoa(n) }

func projectBase(s *S) *webVM {
	s.Section("a base project")
	w := &webVM{}
	w.p, _ = s.NewProject("web")
	_, _ = s.Run(Cmd{Args: []string{"git", "-C", w.p, "init", "-q"}})
	// The shell devvm opens reads its commands from devvm's input: these, then it ends.
	vm, res, _ := s.Launch(w.p, "pwd; echo e2e-shell-ran", "--network", "open", "--cpus", "2", "--memory", "3", "--disk", "120")
	w.vm, w.launch = vm, res.Out
	// A running VM, as the golden fixtures have it (--golden), and its overlay disk on Linux.
	if vm != "" {
		s.Golden("limactl-list-running.json", "limactl", "list", "--json", vm)
		if runtime.GOOS == "linux" {
			s.Golden("qemu-img-info-overlay.json", "qemu-img", "info", "-U", "--output=json", filepath.Join(s.sb.lima, vm, "disk"))
		}
	}
	p := w.p
	s.Check("devvm creates the VM from base, and opens a shell in the folder", func() error {
		st := w.state(s)
		switch {
		case vm == "" || s.Status(vm) != "Running":
			return errors.New("no running VM")
		case !HasLine(res.Out, p) || !HasLine(res.Out, "e2e-shell-ran"):
			return errors.New("the shell didn't run in the folder")
		case st["image"] != "base" || st["net.tier"] != "open" || st["dir"] != p:
			return fmt.Errorf("its state: image=%s net.tier=%s dir=%s", st["image"], st["net.tier"], st["dir"])
		}
		return nil
	})
	// devvm in a subfolder opens the folder's VM there, says so, and makes none.
	_ = os.MkdirAll(filepath.Join(p, "src", "lib"), 0o755) //nolint:gosec // a project's folder, as its user's mkdir makes it
	s.Check("devvm in a subfolder opens the folder's VM in the subfolder", func() error {
		if vm == "" {
			// Without the folder's VM, devvm here would make a VM for the subfolder.
			return errors.New("no VM for the folder: check 1 failed")
		}
		r, err := DevvmIn(s, filepath.Join(p, "src", "lib"), "pwd\n", nil)
		if err != nil {
			return err
		}
		if !HasLine(r.Out, p+"/src/lib") || !grepF(r.Out, s.Msg("using_parent_vm", "dir="+p, "vm="+vm)) {
			return errors.New("it didn't open the folder's VM in the subfolder, saying so")
		}
		for _, st := range allStates(s.Paths) {
			if st["dir"] == p+"/src" || st["dir"] == p+"/src/lib" {
				return errors.New("it made a VM for " + st["dir"])
			}
		}
		return nil
	})
	s.Check("it gets --cpus, --memory and --disk", func() error {
		_, err := s.InVM(vm, `[[ $(nproc) == 2 ]] && (( $(awk "/^MemTotal:/ { print int(\$2 / 1048576 + 0.5) }" /proc/meminfo) == 3 )) && (( $(df -BG --output=size / | tail -1 | tr -dc 0-9) >= 115 ))`)
		return err
	})
	s.Check("devvm size shows it", func() error {
		r, _ := s.Devvm(p, "size")
		if Trim(r.Out) != "cpus=2 memory=3GiB disk=120GiB" {
			return errors.New("devvm size: " + r.Out)
		}
		return nil
	})
	s.Check("ls lists it, and no image", func() error {
		r, err := s.Devvm("", "ls")
		if err != nil {
			return err
		}
		if !Matches(r.Out, `^`+regexp.QuoteMeta(vm)+` +Running +base +- +(on|off) +open +- +`+regexp.QuoteMeta(p)+`$`) || Matches(r.Out, `^devvm-img-`) {
			return errors.New("devvm ls:\n" + r.Out)
		}
		return nil
	})
	s.Check("the folder is mounted read-write", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", "echo from-vm > "+p+"/vm.txt"); err != nil {
			return err
		}
		if !HasLine(readText(filepath.Join(p, "vm.txt")), "from-vm") {
			return errors.New("the VM's file isn't here")
		}
		return nil
	})
	// On Linux hosts the mount is 9p, whose subfolders' events reach no watcher, even the VM's own.
	if runtime.GOOS == "darwin" {
		s.Check("a watcher in the VM sees files changed, made and deleted in the VM", func() error {
			_, err := WatchSees(s, vm, p, "vm", true)
			return err
		})
	} else {
		s.Check("a watcher in the VM sees files changed, made and deleted in the VM, but not in a subfolder (9p)", func() error {
			_, err := WatchSees(s, vm, p, "vm", false)
			return err
		})
	}
	s.Check("... but none changed, made or deleted on this machine (Lima's mounts pass on no file events: polling)", func() error {
		return WatchMisses(s, vm, p)
	})
	projectBaseVM(s, w)
	projectBasePorts(s, w)
	projectGuide(s, w)
	return w
}

// projectGuide: devvm's guide for the agents in the VM, devvm-help, and
// that the VM's user can't change it.
func projectGuide(s *S, w *webVM) {
	guide := readText(filepath.Join(s.sb.root, "assets", "guest", "guide.md"))
	s.Check("the VM has devvm's guide, root's and read-only, and devvm-help prints it", func() error {
		r, err := s.InVM(w.vm, `stat -c '%U %a' /usr/share/doc/devvm/README.md`)
		if err != nil || strings.TrimSpace(r.Stdout) != "root 644" {
			return fmt.Errorf("its owner and mode: %q, %w", r.Stdout, err)
		}
		r, err = s.InVM(w.vm, `devvm-help`)
		if err != nil || r.Stdout != guide {
			return fmt.Errorf("devvm-help printed %d bytes, not the guide's %d: %w", len(r.Stdout), len(guide), err)
		}
		if _, err := s.InVM(w.vm, `echo x >> /usr/share/doc/devvm/README.md`); err == nil {
			return errors.New("the VM's user wrote the guide")
		}
		return nil
	})
}

// projectBaseVM: what the base project's VM has and hasn't from the host.
func projectBaseVM(s *S, w *webVM) {
	p, vm := w.p, w.vm
	// Only the project folder and the shared tools: the VM's own mounts list holds nothing else (in a
	// nested Linux test host, the guest's home has the host's path, so looking for files there proves
	// nothing).
	s.Check("nothing else from the host is mounted", func() error {
		if m := MountLine(s, vm); m != p+" "+s.Paths.Share+" " {
			return errors.New("its mounts: " + m)
		}
		return nil
	})
	s.Check("SSH agent forwarding is off", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "ssh-add", "-l"); err == nil {
			return errors.New("the VM has an agent")
		}
		return nil
	})
	s.Check("without the docker layer, it has no Docker", func() error {
		_, err := s.InVM(vm, `! command -v dockerd && ! command -v docker && ! systemctl cat docker.service >/dev/null 2>&1`)
		return err
	})
	s.Check("hostname is the folder", func() error {
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "hostname")
		if Trim(r.Stdout) != "web" {
			return errors.New("hostname: " + r.Stdout)
		}
		return nil
	})
	// Every port (the default), through devvm's forwarder over Lima's ssh, Lima's own forwards off: a
	// program on this machine on every address of a port (with SO_REUSEADDR, as most servers do) keeps
	// its localhost connections while the VM listens on it too; a VM server on another port reaches this
	// machine (the control).
	kh := filepath.Join(s.Paths.VMDir(vm), "run", "known_hosts")
	s.Check("Lima forwards nothing itself; devvm's forwarder runs, with the host keys devvm pinned for this start", func() error {
		r, _ := s.Limactl("list", "--format", "{{range .Config.PortForwards}}{{.GuestIP}}={{.Ignore}} {{end}}", vm)
		if Trim(r.Stdout) != "127.0.0.1=true 0.0.0.0=true ::=true " {
			return errors.New("Lima's port forwards: " + r.Stdout)
		}
		pinned := false
		for _, l := range Forwarders(s, vm) {
			pinned = pinned || strings.Contains(l, " -known-hosts "+kh+" ")
		}
		if !pinned {
			return errors.New("no forwarder with the pinned host keys")
		}
		if !Matches(readText(kh), `^`+regexp.QuoteMeta(vm)+` ssh-ed25519 `) {
			return errors.New("no ed25519 host key pinned")
		}
		return nil
	})
	// Without network policy too: so no other VM's forwarder takes it before Lima binds it.
	s.Check("its ssh port is the one its state records, on this machine's loopback alone", func() error {
		st := w.state(s)
		sp := st["ports.host.ssh"]
		if sp == "" || st["net.tier"] == "allowlist" {
			return errors.New("no ssh port recorded, or network policy on")
		}
		r, _ := s.Limactl("list", "--format", "{{.SSHLocalPort}}", vm)
		if Trim(r.Stdout) != sp {
			return errors.New("Lima's ssh port is " + r.Stdout + ", its state's " + sp)
		}
		n, _ := strconv.Atoi(sp)
		return loopbackOnly(s, n)
	})
}

// projectBasePorts: the base project's forwarded ports.
func projectBasePorts(s *S, w *webVM) {
	vm := w.vm
	p60, p61, p62, p63, p64 := s.Port(47160), s.Port(47161), s.Port(47162), s.Port(47163), s.Port(47164)
	hostprog, _ := HostProgram(s, ":"+Itoa(p61), true)
	LimaServe(s, vm, p60, p61)
	_, _ = s.InVM(vm, fmt.Sprintf(`setsid -f python3 -c 'import socket,time; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",%d)); time.sleep(600)' devvm-e2e-udp </dev/null >/dev/null 2>&1`, p62))
	ps := s.Needing(backend.FeaturePortsAll) // the forward checks; a VM of base forwards every port
	ps.Check("a VM server on a port reaches this machine", func() error {
		if !ReachesWithin(s, 20*time.Second, Local(p60)) {
			return fmt.Errorf("%d isn't forwarded", p60)
		}
		return nil
	})
	ps.Check("through devvm's forwarder alone: Lima's hostagent has none of its ports, TCP or UDP", func() error {
		time.Sleep(Settle)
		switch {
		case !Reaches(s, Local(p60)):
			return fmt.Errorf("%d isn't forwarded", p60)
		case limaListens(s, "tcp", p60) || limaListens(s, "udp", p62):
			return errors.New("Lima's hostagent has the VM's ports")
		case len(Forwarders(s, vm)) == 0:
			return errors.New("no forwarder runs")
		}
		return nil
	})
	ps.Check("the forward listens on this machine's loopback alone", func() error { return loopbackOnly(s, p60) })
	ps.Check("a port a program here has on every address keeps its localhost connections while the VM listens on it", func() error {
		time.Sleep(Settle)
		out := Answer(s, Local(p61))
		if out != "host-program" {
			return fmt.Errorf("%d answered by: %q", p61, out)
		}
		if !strings.Contains(readText(filepath.Join(s.Paths.VMDir(vm), "logs", "ports.log")), fmt.Sprintf("port %d: not forwarded", p61)) {
			return errors.New("ports.log doesn't say it wasn't forwarded")
		}
		return nil
	})
	hostprog.Close()
	// A server on a link's address (127.77.0.0/16) is never forwarded, while the VM's own on 127.0.0.1
	// with the same port is (the control).
	VMSay(s, vm, "127.77.0.1", p63, "link")
	VMSay(s, vm, "127.77.0.1", p64, "link")
	VMSay(s, vm, "127.0.0.1", p64, "own")
	ps.Check("a server on a link's address isn't forwarded, the VM's own on 127.0.0.1 with its port is", func() error {
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "ss", "-Hltn", "sport = :"+Itoa(p63))
		if !strings.Contains(r.Stdout, "127.77.0.1:"+Itoa(p63)) {
			return errors.New("the link's server doesn't listen")
		}
		_ = s.Until(20*time.Second, func() error {
			if Answer(s, Local(p64)) != "own" {
				return errors.New("not the VM's own")
			}
			return nil
		})
		if a := Answer(s, Local(p64)); a != "own" {
			return fmt.Errorf("%d answered by: %q", p64, a)
		}
		time.Sleep(Settle)
		if Reaches(s, Local(p63)) || Reaches(s, Local6(p63)) {
			return fmt.Errorf("%d is forwarded", p63)
		}
		if strings.Contains(readText(filepath.Join(s.Paths.VMDir(vm), "logs", "ports.log")), fmt.Sprintf("port %d", p63)) {
			return errors.New("ports.log names the link's port")
		}
		return nil
	})
	_, _ = s.InVM(vm, fmt.Sprintf("pkill -f '[h]ttp.server -b 0.0.0.0 %s'; pkill -f '[d]evvm-e2e-udp'; pkill -f '[d]evvm-e2e-say'", Itoa(p60)[:len(Itoa(p60))-1]))
	s.Check("Exec's arguments, folder and input reach the guest as given", func() error { return ExecQuoting(s, vm) })
}

// allStates is every VM's state.
func allStates(p Paths) []map[string]string {
	m, _ := filepath.Glob(filepath.Join(p.Devvm, "vms", "*", "vm.json"))
	var out []map[string]string
	for _, f := range m {
		out = append(out, readState(f))
	}
	return out
}

func projectCA(s *S, w *webVM) {
	s.Section("a company's CA")
	p, vm := w.p, w.vm
	p95 := s.Port(47195)
	// The run's CA, in devvm's CA folder: the running VM trusts it from its next launch, in its system's
	// store, so an HTTPS server here it signed is trusted; emptied, the next launch takes it out again.
	ca, caErr := NewCA(s)
	if caErr == nil {
		caErr = errors.Join(ca.Serve(p95), ca.Trust())
	}
	s.Check("a CA in devvm's CA folder is trusted in the VM from its next launch", func() error {
		if caErr != nil {
			return caErr
		}
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		_, err := s.Limactl("shell", "--workdir", "/", vm, "sh", "-c", `openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt /usr/local/share/ca-certificates/devvm-extra-1.crt &&
    grep -qF "$(sed -n 2p /usr/local/share/ca-certificates/devvm-extra-1.crt)" /etc/ssl/devvm.pem && curl -sSf -o /dev/null https://host.lima.internal:`+Itoa(p95)+`/`)
		return err
	})
	s.Check("the VM's environment points tools that don't read the system's store at it", func() error {
		_, err := s.InVM(vm, `[[ $NODE_EXTRA_CA_CERTS == /etc/ssl/devvm.pem && $SSL_CERT_FILE == /etc/ssl/certs/ca-certificates.crt && $REQUESTS_CA_BUNDLE == $SSL_CERT_FILE && $PIP_CERT == $SSL_CERT_FILE ]]`)
		return err
	})
	line2 := ""
	if ca != nil {
		_ = ca.Untrust()
		line2 = ca.Line(2)
	}
	s.Check("emptied, the VM trusts it no more from its next launch", func() error {
		if caErr != nil {
			return caErr
		}
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sh", "-c", `! ls /usr/local/share/ca-certificates/devvm-extra-* >/dev/null 2>&1 &&
    ls /usr/local/share/ca-certificates/devvm-proxy.crt >/dev/null &&
    ! curl -sSf -o /dev/null https://host.lima.internal:`+Itoa(p95)+`/`); err != nil {
			return err
		}
		has, err := vmHas(s, vm, false, "cat /etc/ssl/devvm.pem", line2)
		switch {
		case err != nil:
			return err
		case has:
			return errors.New("/etc/ssl/devvm.pem still has the CA")
		}
		return nil
	})
	if ca != nil {
		ca.StopServing()
	}
}

func projectSSH(s *S, w *webVM) {
	s.Section("ssh by name")
	vm := w.vm
	// An ~/.ssh/config of its own, set up as the docs say, with a Host * that forwards the agent: yours
	// is never read or changed. ssh gets no agent but a throwaway one.
	w.sc = filepath.Join(s.sb.work, "ssh")
	sc := w.sc
	madeFile(s.sb, sc)
	_ = os.RemoveAll(sc)
	_ = os.MkdirAll(sc, 0o700)
	sshConfig := filepath.Join(s.Paths.Devvm, "ssh_config")
	cfg := filepath.Join(sc, "config")
	_ = os.WriteFile(cfg, []byte("Include "+sshConfig+"\nHost *\n  ForwardAgent yes\n"), 0o600)
	w.kh = filepath.Join(s.Paths.VMDir(vm), "run", "known_hosts")
	kh := w.kh
	_, _ = SSHAgent(s, filepath.Join(sc, "agent.sock"))
	ssh := []string{"ssh", "-F", cfg}
	s.Check("the ssh config lists it by the folder's name", func() error {
		if !HasLine(readText(sshConfig), "Host devvm-web "+vm) {
			return errors.New("no Host devvm-web " + vm)
		}
		return nil
	})
	s.Check("its host keys are pinned, as the VM has them", func() error {
		n := 0
		for l := range strings.SplitSeq(readText(kh), "\n") {
			if strings.HasPrefix(l, vm+" ") {
				n++
			}
		}
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "sh", "-c", "cat /etc/ssh/ssh_host_*_key.pub")
		if have := strings.Count(r.Stdout, "\n"); n == 0 || n != have {
			return fmt.Errorf("%d host keys pinned, the VM has %d", n, have)
		}
		return nil
	})
	s.Check("ssh devvm-web reaches it", func() error {
		r, _ := s.Run(Cmd{Args: append(ssh[:3:3], "devvm-web", "hostname")})
		if Trim(r.Stdout) != "web" {
			return errors.New("hostname: " + r.Out)
		}
		return nil
	})
	s.Check("agent forwarding stays off, whatever Host * says", func() error {
		_, err := s.Run(Cmd{Args: append(ssh[:3:3], "devvm-web", `test -z "${SSH_AUTH_SOCK:-}"`), Env: []string{"SSH_AUTH_SOCK=" + filepath.Join(sc, "agent.sock")}})
		return err
	})
	s.Check("a changed host key is refused", func() error {
		saved := readText(kh)
		if _, err := s.Run(Cmd{Args: []string{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(sc, "other")}}); err != nil {
			return err
		}
		pub := strings.Fields(readText(filepath.Join(sc, "other.pub")))
		if len(pub) < 2 {
			return errors.New("no key made")
		}
		var keep []string
		for l := range strings.SplitSeq(Trim(saved), "\n") {
			if !strings.HasPrefix(l, vm+" ") {
				keep = append(keep, l)
			}
		}
		_ = os.WriteFile(kh, []byte(strings.Join(append(keep, vm+" "+pub[0]+" "+pub[1]), "\n")+"\n"), 0o600)
		r, err := s.Run(Cmd{Args: append(ssh[:3:3], "devvm-web", "true")})
		_ = os.WriteFile(kh, []byte(saved), 0o600)
		if ExitStatus(err) != 255 || !strings.Contains(r.Out, "Host key verification failed") {
			return fmt.Errorf("ssh with a changed host key: %w", err)
		}
		return nil
	})
	s.Check("its sshd binds remote forwards only on 127.77.0.0/16, refuses socket forwards and X11; a local forward works (the control)", func() error {
		return limaSSHD(s, vm, "devvm-web", filepath.Join(sc, "agent.sock"), ssh)
	})
	s.Check("the entry turns off X11, tunnels and adding keys to an agent, whatever Host * says", func() error {
		hostile := filepath.Join(sc, "hostile-config")
		_ = os.WriteFile(hostile, []byte("Include "+sshConfig+"\nHost *\n  ForwardX11 yes\n  ForwardX11Trusted yes\n  AddKeysToAgent yes\n  Tunnel yes\n"), 0o600)
		r, err := s.Run(Cmd{Args: []string{"ssh", "-G", "-F", hostile, "devvm-web"}})
		if err != nil {
			return err
		}
		for _, want := range []string{"forwardx11 no", "forwardx11trusted no", "addkeystoagent false", "tunnel false", "permitremoteopen none", "updatehostkeys false"} {
			if !HasLine(r.Stdout, want) {
				return errors.New("ssh -G: not " + want)
			}
		}
		return nil
	})
}

// limaSSHD checks the VM's sshd, reached as host: it binds remote
// forwards only on links' addresses (RfwdLinks) and refuses X11, as sshd
// -T shows; a local forward works (the control).
func limaSSHD(s *S, vm, host, sock string, ssh []string) error {
	if err := RfwdLinks(s, host, sock, ssh...); err != nil {
		return err
	}
	t, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sshd", "-T")
	if err != nil {
		return err
	}
	for _, want := range []string{"x11forwarding no", "allowtcpforwarding yes", "allowstreamlocalforwarding local", "gatewayports clientspecified", "permitlisten 127.77.*:*", "permittunnel no"} {
		if !HasLine(t.Stdout, want) {
			return errors.New("not " + want)
		}
	}
	p, err := s.Start(Cmd{Args: append(ssh[:len(ssh):len(ssh)], "-W", "127.0.0.1:22", host)})
	if err != nil {
		return err
	}
	defer p.Stop()
	return s.Until(30*time.Second, func() error {
		if !strings.HasPrefix(p.Out(), "SSH-") {
			return errors.New("the local forward didn't reach sshd: " + p.Out())
		}
		return nil
	})
}

func projectGit(s *S, w *webVM) {
	s.Section(".git protection")
	p, vm := w.p, w.vm
	if runtime.GOOS != "darwin" && w.state(s)["git.mode"] == "on" {
		projectGitPins(s, w)
		return
	}
	if runtime.GOOS != "darwin" {
		s.Check("off on a Linux host that can't pin, and says so", func() error {
			if w.state(s)["git.mode"] != "off" || !grepF(w.launch, s.Msg("git_unprotected", "vm="+vm)) {
				return errors.New("git protection isn't off, or the launch didn't say so")
			}
			return nil
		})
		return
	}
	s.Check("hooks and config are locked", func() error {
		if !Locked(s, filepath.Join(p, ".git", "config")) || !Locked(s, filepath.Join(p, ".git", "hooks")) || w.state(s)["git.mode"] != "on" {
			return errors.New("hooks or config aren't locked")
		}
		return nil
	})
	s.Check("root in the VM can't add a hook", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "echo x > "+p+"/.git/hooks/pre-commit"); err == nil || Exists(filepath.Join(p, ".git", "hooks", "pre-commit")) {
			return errors.New("root added a hook")
		}
		return nil
	})
	s.Check("root in the VM can't change the config", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "echo x >> "+p+"/.git/config"); err == nil {
			return errors.New("root changed the config")
		}
		return nil
	})
	s.Check("git in the VM can commit", func() error {
		_, err := s.InVM(vm, "cd "+p+` && for i in 1 2 3 4 5 6 7 8 9 10; do git status >/dev/null 2>&1 && break; sleep 1; done &&
    git add -A && git -c user.email=e@e -c user.name=e commit -qm vm`)
		return err
	})
}

func projectExisting(s *S, w *webVM) {
	s.Section("an existing VM")
	p, vm, sc := w.p, w.vm, w.sc
	sshConfig := filepath.Join(s.Paths.Devvm, "ssh_config")
	ssh := []string{"ssh", "-F", filepath.Join(sc, "config")}
	s.Check("devvm stop stops it and its port forwarder, and drops its ssh config entry", func() error {
		r, err := s.Devvm(p, "stop")
		switch {
		case err != nil:
			return err
		case s.Status(vm) != "Stopped" || !grepF(r.Out, s.Msg("stopped", "vm="+vm)):
			return errors.New("it didn't stop, saying so")
		case len(Forwarders(s, vm)) > 0:
			return errors.New("its forwarder runs")
		case Matches(readText(sshConfig), `^Host devvm-web `):
			return errors.New("its ssh config entry is there")
		}
		return nil
	})
	// A program here that took its ssh port while it was stopped, on 127.0.0.1 alone and accepting
	// nothing (SO_REUSEADDR, as the port's last connections may still be in TIME_WAIT): the VM gets a
	// new one, recorded, and the program keeps its own.
	oldSP := w.state(s)["ports.host.ssh"]
	hostprog, hpErr := HostProgram(s, "127.0.0.1:"+oldSP, false)
	s.Check("stopped, devvm starts it again, on a new ssh port when a program here has its own", func() error {
		if oldSP == "" || s.Status(vm) != "Stopped" || hpErr != nil {
			return fmt.Errorf("not stopped, or no program on its port %q: %w", oldSP, hpErr)
		}
		r, err := DevvmIn(s, p, "echo e2e-again\n", nil)
		if err != nil {
			return err
		}
		if s.Status(vm) != "Running" || !HasLine(r.Out, "e2e-again") {
			return errors.New("it didn't start")
		}
		sp := w.state(s)["ports.host.ssh"]
		if sp == "" || sp == oldSP {
			return errors.New("its ssh port is still " + oldSP)
		}
		lr, _ := s.Limactl("list", "--format", "{{.SSHLocalPort}}", vm)
		if Trim(lr.Stdout) != sp {
			return errors.New("Lima's ssh port is " + lr.Stdout + ", its state's " + sp)
		}
		return nil
	})
	hostprog.Close()
	s.Check("running, devvm opens it as it is", func() error {
		r, _ := DevvmIn(s, p, "echo e2e-running\n", nil)
		if !HasLine(r.Out, "e2e-running") {
			return errors.New("the shell didn't run")
		}
		return nil
	})
	// unsyncedKept: a file written without sync, then devvm stop at once, is there with its checksum
	// after the next start: the guest shut down cleanly. The stop's seconds and any forced stop Lima
	// logged go in the run's output, pass or fail.
	var unsynced []string
	s.Check("a file written without sync just before devvm stop is kept", func() error {
		return unsyncedKept(s, w, &unsynced)
	})
	for _, l := range unsynced {
		s.r.out.line("  %s", l)
	}
	// devvm host with a home of its own, so the Include line is offered for an empty one (and, without a
	// terminal, not added).
	home := filepath.Join(sc, "home")
	_ = os.MkdirAll(home, 0o700)
	s.Check("devvm host starts a stopped VM and names it, without a shell; ssh reaches it", func() error {
		if _, err := DevvmIn(s, p, "", []string{"HOME=" + home}, "stop"); err != nil {
			return err
		}
		if s.Status(vm) != "Stopped" {
			return errors.New("it didn't stop")
		}
		r, err := DevvmIn(s, p, "", []string{"HOME=" + home}, "host")
		if err != nil {
			return err
		}
		info := s.Msg("ssh_host_info", "dir="+p, "host=devvm-web")
		switch {
		case s.Status(vm) != "Running":
			return errors.New("it didn't start")
		case !grepF(r.Out, FirstLine(info)) || !grepF(r.Out, LastLine(info)) || !grepF(r.Out, FirstLine(s.Msg("ssh_include_yourself"))):
			return errors.New("devvm host didn't name it, and offer the Include line:\n" + r.Out)
		case Exists(filepath.Join(home, ".ssh")):
			return errors.New("it wrote ~/.ssh")
		}
		hr, _ := s.Run(Cmd{Args: append(ssh[:3:3], "devvm-web", "hostname")})
		if Trim(hr.Stdout) != "web" {
			return errors.New("ssh devvm-web: " + hr.Out)
		}
		return nil
	})
	s.Check("VS Code's settings in the VM: no port forwarding, no git sign-in relay", func() error {
		_, err := s.InVM(vm, `[[ $(jq -c . ~/.vscode-server/data/Machine/settings.json) == "{\"remote.autoForwardPorts\":false,\"git.terminalAuthentication\":false,\"github.gitAuthentication\":false}" ]]`)
		return err
	})
	_, _ = s.InVM(vm, `echo "{\"editor.fontSize\": 14, \"remote.autoForwardPorts\": true}" > ~/.vscode-server/data/Machine/settings.json`)
	_, _ = DevvmIn(s, p, "", []string{"HOME=" + home}, "host")
	s.Check("changed back in the VM, devvm host turns them off again and keeps the others", func() error {
		_, err := s.InVM(vm, `jq -e ".[\"editor.fontSize\"] == 14 and .[\"remote.autoForwardPorts\"] == false and .[\"github.gitAuthentication\"] == false" ~/.vscode-server/data/Machine/settings.json >/dev/null`)
		return err
	})
	s.Check("restart, and its new host keys are pinned", func() error {
		if _, err := DevvmIn(s, p, "exit\n", nil, "restart"); err != nil {
			return err
		}
		if s.Status(vm) != "Running" {
			return errors.New("it isn't running")
		}
		r, _ := s.Run(Cmd{Args: append(ssh[:3:3], "devvm-web", "hostname")})
		if Trim(r.Stdout) != "web" {
			return errors.New("ssh devvm-web: " + r.Out)
		}
		return nil
	})
}

// unsyncedKept writes a file without sync, stops the VM at once, starts
// it, and checks the file's checksum; what it saw goes in lines.
func unsyncedKept(s *S, w *webVM, lines *[]string) error {
	*lines = nil
	r, _ := s.InVM(w.vm, `head -c 1048576 /dev/urandom > ~/e2e-unsynced && sha256sum ~/e2e-unsynced`)
	sum := ""
	if f := strings.Fields(r.Stdout); len(f) > 0 {
		sum = f[0]
	}
	t0 := time.Now()
	if _, err := s.Devvm(w.p, "stop"); err != nil {
		return errors.New("stop failed")
	}
	*lines = append(*lines, fmt.Sprintf("unsynced: stop took %ds", int(time.Since(t0).Seconds())))
	for l := range strings.SplitSeq(readText(filepath.Join(s.sb.lima, w.vm, "ha.stderr.log")), "\n") {
		if strings.Contains(strings.ToLower(l), "forc") {
			*lines = append(*lines, "unsynced: lima: "+l)
		}
	}
	if _, err := DevvmIn(s, w.p, "exit\n", nil); err != nil {
		return errors.New("start failed")
	}
	r, _ = s.InVM(w.vm, `sha256sum ~/e2e-unsynced 2>&1; rm -f ~/e2e-unsynced`)
	after := ""
	if f := strings.Fields(r.Stdout); len(f) > 0 {
		after = f[0]
	}
	if sum == "" || sum != after {
		return fmt.Errorf("unsynced: before %s after %s", sum, after)
	}
	return nil
}

func projectPorts(s *S, w *webVM) {
	p, vm := w.p, w.vm
	p70, p73 := s.Port(47170), s.Port(47173)
	rng := Itoa(p70) + "-" + Itoa(p73)
	ps := s.Needing(backend.FeaturePorts) // the checks of listed ports
	ps.Check("a running VM's new ports apply at once", func() error {
		r, _ := s.Devvm(p, "ports", rng)
		if !grepF(r.Out, s.Msg("ports_forwarding", "ports="+rng, "vm="+vm)) {
			return errors.New("devvm ports didn't say it forwards them:\n" + r.Out)
		}
		if now, _ := s.Devvm(p, "ports"); Trim(now.Stdout) != rng {
			return errors.New("devvm ports: " + now.Stdout)
		}
		for _, l := range Forwarders(s, vm) {
			if strings.HasSuffix(l, " -only "+rng) {
				return nil
			}
		}
		return errors.New("its forwarder doesn't forward them alone")
	})
	s.Check("a bad port list is a usage error, exit 2, naming devvm ports --help, and the VM's ports stay", func() error {
		r, err := s.Devvm(p, "ports", Itoa(p70)+"-x")
		if ExitStatus(err) != 2 || !SaidLine(r.Out, s.Msg("usage_help", "command=devvm ports")) {
			return fmt.Errorf("devvm ports %d-x: %w\n%s", p70, err, r.Out)
		}
		if now, _ := s.Devvm(p, "ports"); Trim(now.Stdout) != rng {
			return errors.New("devvm ports: " + now.Stdout)
		}
		return nil
	})
	s.Check("a word that's neither a command nor an image is a usage error suggesting the command, and the VM runs on", func() error {
		r, err := s.Devvm(p, "stoop")
		if ExitStatus(err) != 2 || !grepF(r.Out, s.Msg("no_command_like", "command=stop", "word=stoop")) || s.Status(vm) != "Running" {
			return fmt.Errorf("devvm stoop: %w\n%s", err, r.Out)
		}
		return nil
	})
	p78 := s.Port(47178)
	ps.Check("a port added to a running VM reaches this machine at once, and once removed doesn't, the VM never restarted", func() error {
		return PortsLive(s, p, vm, p78, LimaServe, LimaBoot)
	})
	_, _ = s.InVM(vm, fmt.Sprintf("pkill -f '[h]ttp.server -b 0.0.0.0 %d'", p78))
	s.Check("its new size waits for its next start", func() error {
		r, _ := s.Devvm(p, "size", "--cpus", "3")
		if !grepF(r.Out, FirstLine(s.Msg("next_start", "count=1", "vm="+vm))) {
			return errors.New("devvm size --cpus 3:\n" + r.Out)
		}
		if r, _ = s.Devvm(p, "size"); !grepF(r.Out, s.Msg("size_pending", "size=cpus=3", "vm="+vm)) {
			return errors.New("devvm size:\n" + r.Out)
		}
		return nil
	})
	s.Check("and applies on restart, its ports kept", func() error {
		if _, err := DevvmIn(s, p, "exit\n", nil, "restart"); err != nil {
			return err
		}
		ports, _ := s.Devvm(p, "ports")
		n, _ := s.Limactl("shell", "--workdir", "/", vm, "nproc")
		size, _ := s.Devvm(p, "size")
		if Trim(ports.Out) != rng || Trim(n.Stdout) != "3" || Trim(size.Out) != "cpus=3 memory=3GiB disk=120GiB" {
			return fmt.Errorf("ports %q, nproc %q, size %q", ports.Out, n.Stdout, size.Out)
		}
		return nil
	})
	s.Check("a smaller disk is refused", func() error {
		r, err := s.Devvm(p, "size", "--disk", "100")
		if err == nil || !grepF(r.Out, s.Msg("disk_shrink", "have=120", "vm="+vm, "want=100")) {
			return errors.New("devvm size --disk 100:\n" + r.Out)
		}
		return nil
	})
	// Listeners on each kind of address Lima matches differently (loopback, every address; IPv4 and 6), on
	// listed ports (47170-47173 in slot 0) and unlisted ones (47174-47177). Their output goes nowhere, or
	// the SSH session waits for them.
	_, _ = s.InVM(vm, "p="+Itoa(p70)+`; for r in 1 2; do for a in 127.0.0.1 ::1 0.0.0.0 ::; do
  setsid -f python3 -m http.server -b $a $p </dev/null >/dev/null 2>&1; p=$((p + 1)); done; done`)
	ps.Check("only the listed ports reach this machine, whatever the address", func() error {
		ReachesWithin(s, 20*time.Second, Local(p73))
		time.Sleep(Settle)
		for i := range 8 {
			port := p70 + i
			if Reaches(s, Local(port)) != (i < 4) {
				return fmt.Errorf("port %d: reached %v", port, Reaches(s, Local(port)))
			}
		}
		return nil
	})
	ps.Check("a listed port a program on this machine has, on every address, stays its own until it's freed", func() error {
		return HostPortKept(s, p, vm, p70, s.Port(47171), LimaServe)
	})
}

func projectGitProtect(s *S, w *webVM) {
	p, vm := w.p, w.vm
	config := filepath.Join(p, ".git", "config")
	if runtime.GOOS != "darwin" && w.state(s)["git.mode"] == "on" {
		projectGitProtectPins(s, w)
		return
	}
	if runtime.GOOS != "darwin" {
		s.Check("git-protect on is refused on a Linux host that can't pin", func() error {
			r, _ := s.Devvm(p, "git-protect", "on")
			if !grepF(r.Out, FirstLine(s.Msg("git_protect_on_needs_locks"))) {
				return errors.New("devvm git-protect on:\n" + r.Out)
			}
			return nil
		})
		projectGitWatch(s, w)
		return
	}
	s.Check("a swapped .git is reported", func() error {
		_, _ = s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", "cd "+p+" && mv .git .git-old && git init -q")
		r, _ := DevvmIn(s, p, "exit\n", nil)
		if !grepF(r.Out, FirstLine(s.Msg("git_replaced", "git="+p+"/.git"))) {
			return errors.New("the launch didn't report it:\n" + r.Out)
		}
		return nil
	})
	_ = os.RemoveAll(filepath.Join(p, ".git"))
	_ = os.Rename(filepath.Join(p, ".git-old"), filepath.Join(p, ".git"))
	// (virtiofs shows the VM the restored .git after a moment.) Locks are set while it's stopped:
	// devvm stop sets them.
	// A submodule whose .git file names the project's own src, which isn't
	// a git folder, gets nothing in src locked.
	s.Check("a submodule git folder the VM adds is reported, and locked by devvm stop", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", "cd "+p+` &&
      for i in 1 2 3 4 5 6 7 8 9 10; do git rev-parse >/dev/null 2>&1 && break; sleep 1; done && git init -q vmsub &&
      git -C vmsub -c user.email=e@e -c user.name=e commit -q --allow-empty -m s &&
      git update-index --add --cacheinfo 160000,$(git -C vmsub rev-parse HEAD),vmsub &&
      mkdir -p src/hooks evil && echo 'export {}' > src/hooks/useThing.ts && echo 'gitdir: ../src' > evil/.git &&
      git update-index --add --cacheinfo 160000,$(git -C vmsub rev-parse HEAD),evil`); err != nil {
			return err
		}
		r, err := s.Devvm(p, "stop")
		if err != nil {
			return err
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_embedded_new", "git="+p+"/vmsub/.git"))) || s.Status(vm) != "Stopped" || !Locked(s, filepath.Join(p, "vmsub", ".git", "config")) {
			return errors.New("devvm stop didn't report it and lock it:\n" + r.Out)
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_gitdir_not_git", "git="+p+"/src", "sub="+p+"/evil"))) || Locked(s, filepath.Join(p, "src", "hooks")) {
			return errors.New("devvm stop didn't report evil, or locked src/hooks:\n" + r.Out)
		}
		return nil
	})
	s.Check("git-protect on a running VM waits for it to stop, and devvm stop applies it", func() error {
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil || s.Status(vm) != "Running" {
			return fmt.Errorf("it didn't start: %w", err)
		}
		r, _ := s.Devvm(p, "git-protect", "off")
		if !grepF(r.Out, FirstLine(s.Msg("git_protect_next_stop", "mode=off", "vm="+vm))) || !Locked(s, config) {
			return errors.New("devvm git-protect off:\n" + r.Out)
		}
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		if Locked(s, config) {
			return errors.New("devvm stop didn't unlock it")
		}
		if m, _ := s.Devvm(p, "git-protect"); Trim(m.Stdout) != "off" {
			return errors.New("devvm git-protect: " + m.Stdout)
		}
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		if s.Status(vm) != "Running" {
			return errors.New("it didn't start again")
		}
		return nil
	})
	s.Check("stopped outside devvm, its port forwarder ends with it (its lifeline)", func() error {
		if _, err := s.Limactl("stop", vm); err != nil {
			return err
		}
		return s.Until(10*time.Second, func() error {
			if len(Forwarders(s, vm)) > 0 {
				return errors.New("its forwarder runs")
			}
			return nil
		})
	})
	s.Check("git-protect strict is refused, and a VM an earlier devvm left strict is on at its next launch", func() error {
		_, _ = s.Limactl("stop", vm)
		if r, err := s.Devvm(p, "git-protect", "strict"); err == nil || !strings.Contains(err.Error(), "exit status 2") ||
			!grepF(r.Out, FirstLine(s.Msg("git_protect_strict_refused"))) {
			return fmt.Errorf("devvm git-protect strict: %w\n%s", err, r.Out)
		}
		// As strict left it: diff.ignoreSubmodules = all in the locked
		// config, and its mode.
		for _, c := range [][]string{
			{"chflags", "nouchg", config},
			{"git", "config", "--file", config, "diff.ignoreSubmodules", "all"},
			{"chflags", "uchg", config},
		} {
			if _, err := s.Run(Cmd{Args: c}); err != nil {
				return err
			}
		}
		if err := SetState(s.Paths, vm, "git.mode", `"strict"`); err != nil {
			return err
		}
		r, err := DevvmIn(s, p, "exit\n", nil)
		if err != nil {
			return err
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_strict_converted", "vm="+vm))) {
			return errors.New("the launch didn't say it's on:\n" + r.Out)
		}
		g, _ := s.Run(Cmd{Args: []string{"git", "config", "--file", config, "diff.ignoreSubmodules"}})
		m, _ := s.Devvm(p, "git-protect")
		if Trim(g.Stdout) != "" || Trim(m.Stdout) != "on" || w.state(s)["git.mode"] != "on" || !Locked(s, config) {
			return fmt.Errorf("diff.ignoreSubmodules %q, mode %q, locked %v", g.Stdout, m.Stdout, Locked(s, config))
		}
		return nil
	})
	s.Check("git-protect off unlocks, and on locks again", func() error {
		_, _ = s.Limactl("stop", vm)
		if _, err := s.Devvm(p, "git-protect", "off"); err != nil {
			return err
		}
		if Locked(s, config) {
			return errors.New("still locked")
		}
		if _, err := s.Devvm(p, "git-protect", "on"); err != nil {
			return err
		}
		if m, _ := s.Devvm(p, "git-protect"); !Locked(s, config) || Trim(m.Stdout) != "on" {
			return errors.New("not locked again: " + m.Stdout)
		}
		return nil
	})
}

// projectGitWatch: on Linux, .git/config that the VM changes is named
// when devvm stop finds it, after the fact; then the VM is as it was.
func projectGitWatch(s *S, w *webVM) {
	p, vm := w.p, w.vm
	s.Check("on Linux, devvm stop names the .git/config the VM changed", func() error {
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		if _, err := s.InVM(vm, "printf '[e2e]\\n\\twatched = vm\\n' >> "+p+"/.git/config"); err != nil {
			return err
		}
		r, err := s.Devvm(p, "stop")
		if err != nil {
			return err
		}
		if !grepF(r.Out, s.Msg("git_changed", "paths=.git/config", "vm="+vm)) {
			return errors.New("devvm stop didn't name .git/config:\n" + r.Out)
		}
		if again, _ := s.Devvm(p, "stop"); grepF(again.Out, s.Msg("git_changed", "paths=.git/config", "vm="+vm)) {
			return errors.New("named twice:\n" + again.Out)
		}
		return nil
	})
	// Put back, and recorded as that by another devvm stop, then started again.
	_, _ = s.Run(Cmd{Args: []string{"git", "config", "--unset", "e2e.watched"}, Dir: p})
	_, _ = s.Devvm(p, "stop")
	_, _ = DevvmIn(s, p, "exit\n", nil)
}

// claudeSlug is the name Claude Code gives a folder's sessions' folder.
func claudeSlug(dir string) string {
	return regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(dir, "-")
}

// A folder moved keeps its VM once you say so: without a terminal devvm refuses, naming devvm
// adopt; in one, a yes moves it, its home folder as it was, and its next start mounts the folder at
// its new path, never the old one, which isn't made again. Claude Code's sessions and its
// .claude.json entry, kept by the folder's path, follow it (made here as Claude Code would make them).
// Moved back, devvm adopt --force moves it.
func projectMoved(s *S, w *webVM) {
	p, vm := w.p, w.vm
	pm := p + "-moved"
	s.sb.projects = append(s.sb.projects, pm)
	_, _ = DevvmIn(s, p, "exit\n", nil)
	// The file is written, read back and synced, so a failed write or a stop that loses it says so.
	wrote, _ := s.InVM(vm, `echo e2e-home-kept > ~/e2e-moved && sync && cat ~/e2e-moved`)
	_, _ = s.InVM(vm, fmt.Sprintf(`mkdir -p ~/.claude/projects/%[1]s && echo e2e > ~/.claude/projects/%[1]s/e2e.jsonl &&
  f=~/.claude.json && { [ -s $f ] || echo '{}' > $f; } && jq --arg p '%[2]s' '.projects[$p] = {e2e: true}' $f > $f.tmp && mv $f.tmp $f`, claudeSlug(p), p))
	_, _ = s.Devvm(p, "stop")
	_ = os.Rename(p, pm)
	dirOf := func() string { return s.Paths.State(vm)["dir"] }
	s.Check("a moved folder's VM is refused without a terminal, naming devvm adopt", func() error {
		r, err := s.Devvm(pm, "stop")
		if err == nil || !grepF(r.Out, s.Msg("vm_moved_here", "vm="+vm, "was="+p)) || dirOf() != p {
			return errors.New("refused:\n" + r.Out)
		}
		return nil
	})
	s.Check("in a terminal, a yes moves the VM to the folder's new path", func() error {
		r, err := TTYRun(s, pm, "y", "env", "NO_COLOR=1", s.sb.devvm, "stop")
		if err != nil || !grepF(r.Out, s.Msg("vm_adopted", "dir="+pm, "vm="+vm)) || dirOf() != pm {
			return fmt.Errorf("moved: %w\n%s", err, r.Out)
		}
		return nil
	})
	s.Check("opened there, its home is as it was, it mounts the new path alone, and Claude Code's history followed it", func() error {
		r, err := DevvmIn(s, pm, "cat ~/e2e-moved; pwd\n", nil)
		if err != nil || !HasLine(r.Out, "e2e-home-kept") || !HasLine(r.Out, pm) {
			d, _ := s.InVM(vm, `echo "home: $HOME"; ls -la ~; findmnt -rn -o TARGET,SOURCE,FSTYPE; uptime`)
			return fmt.Errorf("opened: %w\n%s\nbefore the move, writing it gave: %s\n%s", err, r.Out, wrote.Out, d.Out)
		}
		ms := MountsOf(s, vm)
		if !HasLine(ms, pm) || HasLine(ms, p) {
			return errors.New("mounts: " + ms)
		}
		if Exists(p) {
			return errors.New(p + " made again")
		}
		// In the VM, the new path is mounted and the old one isn't (an empty folder may stay there).
		if _, err := s.InVM(vm, "mountpoint -q '"+pm+"' && ! mountpoint -q '"+p+"'"); err != nil {
			d, _ := s.InVM(vm, "grep devvm-go-e2e /proc/mounts")
			return errors.New("the VM's mounts:\n" + d.Out)
		}
		return movedHistory(s, vm, p, pm)
	})
	s.Check("moved back, devvm adopt --force moves it back, Claude Code's history too", func() error {
		if _, err := s.Devvm(pm, "stop"); err != nil {
			return err
		}
		if err := os.Rename(pm, p); err != nil {
			return err
		}
		r, err := s.Devvm(p, "adopt", "--force")
		if err != nil || !grepF(r.Out, s.Msg("vm_adopted", "dir="+p, "vm="+vm)) {
			return fmt.Errorf("devvm adopt --force: %w\n%s", err, r.Out)
		}
		r, err = DevvmIn(s, p, "pwd\n", nil)
		if err != nil || !HasLine(r.Out, p) || !HasLine(MountsOf(s, vm), p) {
			return fmt.Errorf("opened: %w\n%s", err, r.Out)
		}
		return movedHistory(s, vm, pm, p)
	})
	if !Exists(p) {
		_ = os.Rename(pm, p)
	}
	projectRm(s, w)
}

// movedHistory checks that Claude Code's session and .claude.json entry
// are to's, and from has neither.
func movedHistory(s *S, vm, from, to string) error {
	if _, err := s.InVM(vm, fmt.Sprintf(`[ -f ~/.claude/projects/%s/e2e.jsonl ] && [ ! -e ~/.claude/projects/%s ] &&
    jq -e --arg o '%s' --arg n '%s' '.projects[$n].e2e == true and (.projects | has($o) | not)' ~/.claude.json >/dev/null`,
		claudeSlug(to), claudeSlug(from), from, to)); err != nil {
		d, _ := s.InVM(vm, `ls ~/.claude/projects; jq -c .projects ~/.claude.json`)
		return errors.New("Claude Code's history isn't " + to + "'s:\n" + d.Out)
	}
	return nil
}

func projectRm(s *S, w *webVM) {
	p, vm := w.p, w.vm
	s.Check("rm deletes only this VM, with its state and ssh config entry, and unlocks", func() error {
		if _, err := s.Devvm(p, "rm", "--force"); err != nil {
			return err
		}
		exports, _ := os.ReadDir(filepath.Join(s.Paths.Devvm, "exports"))
		switch {
		case Listed(s, vm) || Exists(s.Paths.VMDir(vm)):
			return errors.New("the VM or its state is still there")
		case Matches(readText(filepath.Join(s.Paths.Devvm, "ssh_config")), `^Host devvm-web `):
			return errors.New("its ssh config entry is still there")
		case Matches(readText(w.kh), `^`+regexp.QuoteMeta(vm)+` `):
			return errors.New("its host keys are still pinned")
		case Locked(s, filepath.Join(p, ".git", "config")):
			return errors.New(".git is still locked")
		case len(Forwarders(s, vm)) > 0:
			return errors.New("its forwarder runs")
		case len(exports) > 0:
			return errors.New("it exported something")
		}
		return nil
	})
}

// devcontainerJSON is the devcontainer.json's first version: its
// forwardPorts, variables and commands, with a comment and a trailing
// comma, as people write them.
func devcontainerJSON(port int) string {
	return strings.Join([]string{
		"{", "  // e2e", `  "name": "dc",`, fmt.Sprintf(`  "forwardPorts": [%d],`, port),
		`  "containerEnv": {"E2E_DC": "dc-${containerWorkspaceFolderBasename}"},`,
		`  "postCreateCommand": "echo \"$E2E_DC $(pwd)\" >> ~/dc-created",`,
		`  "postStartCommand": ["sh", "-c", "echo started >> ~/dc-started"],`, "}", "",
	}, "\n")
}

// Its forwardPorts added to the user's ports (none for this VM) and reaching this machine, its
// variables in the shell and its commands, its postCreateCommand once, its postStartCommand at each
// start; nothing written to the folder, a changed file only warned about, then devvm update makes
// the VM again as it says, its home kept and the file's new port forwarded in place of its old one.
func projectDevcontainer(s *S) {
	s.Section("a devcontainer.json")
	p80, p81 := s.Port(47180), s.Port(47181)
	dc, _ := s.NewProject("dc")
	file := filepath.Join(dc, ".devcontainer", "devcontainer.json")
	_ = os.MkdirAll(filepath.Dir(file), 0o755) //nolint:gosec // a project's folder, as its user's mkdir makes it
	orig := devcontainerJSON(p80)
	_ = os.WriteFile(file, []byte(orig), 0o644) //nolint:gosec // a project's file, as its user writes it
	_, _ = s.Devvm("", "ports", "-g", "none")
	vm, launch, _ := s.Launch(dc, `echo "dc-env=$E2E_DC"`, "--network", "open")
	_, _ = s.Devvm("", "ports", "-g", "all")
	st := func() map[string]string { return s.Paths.State(vm) }
	const rel = "file=.devcontainer/devcontainer.json"
	s.Check("devvm makes the VM as its devcontainer.json says, and says so", func() error {
		switch {
		case vm == "" || s.Status(vm) != "Running":
			return errors.New("no running VM")
		case !grepF(launch.Out, s.Msg("new_vm_summary", "vm="+vm)) || !grepF(launch.Out, s.Msg("new_vm_devcontainer", rel)):
			return errors.New("the launch didn't say so:\n" + launch.Out)
		case !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(st()["devcontainer.sum"]) ||
			st()["devcontainer.file"] != ".devcontainer/devcontainer.json" || st()["ports.forward"] != Itoa(p80):
			return fmt.Errorf("its state: %v", st())
		}
		return nil
	})
	s.Check("its forwardPorts join your ports, as the file's", func() error {
		if st()["devcontainer.ports_gave"] != Itoa(p80) {
			return errors.New("devcontainer.ports_gave=" + st()["devcontainer.ports_gave"])
		}
		return nil
	})
	s.Check("its shell has the file's variables", func() error {
		if !HasLine(launch.Out, "dc-env=dc-dc") {
			return errors.New("no dc-env=dc-dc")
		}
		return nil
	})
	s.Check("postCreateCommand ran once, in the VM, in the folder, with them", func() error {
		_, err := s.InVM(vm, "[[ $(cat ~/dc-created) == 'dc-dc "+dc+"' ]]")
		return err
	})
	s.Check("postStartCommand ran at its start", func() error {
		_, err := s.InVM(vm, `[[ $(cat ~/dc-started) == started ]]`)
		return err
	})
	s.Check("nothing was written to the project folder", func() error {
		top, _ := os.ReadDir(dc)
		sub, _ := os.ReadDir(filepath.Dir(file))
		if len(top) != 1 || top[0].Name() != ".devcontainer" || len(sub) != 1 || sub[0].Name() != "devcontainer.json" || readText(file) != orig {
			return errors.New("the folder changed")
		}
		return nil
	})
	serve := func(ports ...int) {
		var cmds []string
		for _, p := range ports {
			cmds = append(cmds, fmt.Sprintf("setsid -f python3 -m http.server -b 0.0.0.0 %d </dev/null >/dev/null 2>&1", p))
		}
		_, _ = s.InVM(vm, strings.Join(cmds, "; "))
	}
	serve(p80, p81)
	s.Check("... its forwardPorts reach this machine's localhost, and other ports don't", func() error {
		if !ReachesWithin(s, 20*time.Second, Local(p80)) || Reaches(s, Local(p81)) {
			return fmt.Errorf("%d reached %v, %d reached %v", p80, Reaches(s, Local(p80)), p81, Reaches(s, Local(p81)))
		}
		return nil
	})
	counts := func(created, started int) error {
		_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", fmt.Sprintf(`[[ $(wc -l < ~/dc-created) == %d && $(wc -l < ~/dc-started) == %d ]]`, created, started))
		return err
	}
	s.Check("launched again, running: nothing runs again", func() error {
		if _, err := DevvmIn(s, dc, "exit\n", nil); err != nil {
			return err
		}
		return counts(1, 1)
	})
	s.Check("restarted: postStartCommand runs again, postCreateCommand doesn't", func() error {
		if _, err := DevvmIn(s, dc, "exit\n", nil, "restart"); err != nil {
			return err
		}
		return counts(1, 2)
	})
	projectDevcontainerChanged(s, dc, vm, file)
}

func projectDevcontainerChanged(s *S, dc, vm, file string) {
	p80, p82 := s.Port(47180), s.Port(47182)
	const rel = "file=.devcontainer/devcontainer.json"
	st := func() map[string]string { return s.Paths.State(vm) }
	_ = os.WriteFile(file, []byte(fmt.Sprintf(`{"forwardPorts": [%d], "postCreateCommand": "touch ~/dc-changed"}`+"\n", p82)), 0o644) //nolint:gosec // a project's file, as its user writes it
	changed := FirstLine(s.Msg("devcontainer_changed", rel, "vm="+vm))
	s.Check("a changed file is warned about, and changes nothing", func() error {
		r, err := DevvmIn(s, dc, "exit\n", nil)
		if err != nil {
			return err
		}
		if !grepF(r.Out, changed) || st()["ports.forward"] != Itoa(p80) {
			return errors.New("no warning, or its ports changed:\n" + r.Out)
		}
		_, err = s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `[[ ! -e ~/dc-changed ]]`)
		return err
	})
	s.Check("devvm update makes it again as the changed file says, keeping its home", func() error {
		r, err := DevvmIn(s, dc, "echo \"dc-env=$E2E_DC.\"; exit\n", nil, "update", "--force")
		if err != nil {
			return err
		}
		if !grepF(r.Out, s.Msg("devcontainer_reread", rel, "vm="+vm)) || !HasLine(r.Out, "dc-env=.") || st()["ports.forward"] != Itoa(p82) {
			return errors.New("it wasn't made again as the file says:\n" + r.Out)
		}
		_, err = s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `[[ -e ~/dc-changed && $(cat ~/dc-created) == "dc-dc `+dc+`" ]]`)
		return err
	})
	_, _ = s.InVM(vm, fmt.Sprintf("setsid -f python3 -m http.server -b 0.0.0.0 %d </dev/null >/dev/null 2>&1; setsid -f python3 -m http.server -b 0.0.0.0 %d </dev/null >/dev/null 2>&1", p80, p82))
	s.Check("... the file's new port reaches this machine, and its old one no longer does", func() error {
		if !ReachesWithin(s, 20*time.Second, Local(p82)) || Reaches(s, Local(p80)) {
			return fmt.Errorf("%d reached %v, %d reached %v", p82, Reaches(s, Local(p82)), p80, Reaches(s, Local(p80)))
		}
		return nil
	})
	s.Check("... and a launch no longer warns about the file", func() error {
		r, err := DevvmIn(s, dc, "exit\n", nil)
		if err != nil {
			return err
		}
		if grepF(r.Out, changed) {
			return errors.New("it warns:\n" + r.Out)
		}
		return nil
	})
	s.Check("rm deletes it", func() error {
		if _, err := s.Devvm(dc, "rm", "--force", "--no-export"); err != nil {
			return err
		}
		if Listed(s, vm) {
			return errors.New(vm + " is still there")
		}
		return nil
	})
	_ = os.WriteFile(file, []byte(`{"image": "localhost:5000/app:1"}`+"\n"), 0o644) //nolint:gosec // a project's file, as its user writes it
	s.Check("an image on this machine's registry is refused, nothing made", func() error {
		r, err := s.Devvm(dc, "--network", "open")
		if err == nil || !grepF(r.Out, s.Msg("devcontainer_image_not_public", rel, "from=localhost:5000/app:1")) {
			return errors.New("not refused:\n" + r.Out)
		}
		if Listed(s, vm) || Exists(s.Paths.VMDir(vm)) {
			return errors.New("a VM was made")
		}
		return nil
	})
}

// agentHome is a home of its own for the agents' checks: a fake agent
// config, history and git identity, and a fake file outside the agent
// folders with links to it from inside one, relative and absolute;
// nothing of yours is read.
func agentHome(s *S) string {
	ah := filepath.Join(s.sb.work, "agent-home")
	madeFile(s.sb, ah)
	_ = os.RemoveAll(ah)
	for _, d := range []string{".claude/skills/e2e", ".claude/projects/p", "outside"} {
		_ = os.MkdirAll(filepath.Join(ah, d), 0o755) //nolint:gosec // a home's folders, as its user's mkdir makes them
	}
	if r, err := filepath.EvalSymlinks(ah); err == nil {
		ah = r
	}
	for f, body := range map[string]string{
		".claude/skills/e2e/SKILL.md":  "# e2e skill\n",
		".claude/settings.json":        `{"theme":"e2e-settings"}` + "\n",
		".claude/projects/p/h.jsonl":   `{"prompt":"e2e-history"}` + "\n",
		".gitconfig":                   "[user]\n\tname = E2E User\n\temail = e2e@example.com\n",
		"outside/e2e-not-a-secret.txt": "e2e-outside-marker\n",
	} {
		_ = os.WriteFile(filepath.Join(ah, f), []byte(body), 0o644) //nolint:gosec // a home's files, as its user writes them
	}
	_ = os.Symlink("../../outside/e2e-not-a-secret.txt", filepath.Join(ah, ".claude", "skills", "escape"))
	_ = os.Symlink(filepath.Join(ah, "outside", "e2e-not-a-secret.txt"), filepath.Join(ah, ".claude", "skills", "escape-abs"))
	return ah
}

func projectAgents(s *S) {
	s.Section("agent folders and the VM's setup")
	// Claude Code's logins in devvm's store, which an earlier run's network section leaves, are set
	// aside for these VMs, which have none, and put back after.
	logins := filepath.Join(s.Paths.Devvm, "logins", "claude")
	aside := logins + ".e2e-aside"
	if os.Rename(logins, aside) == nil {
		putBack := func() {
			if Exists(aside) {
				_ = os.RemoveAll(logins)
				_ = os.Rename(aside, logins)
			}
		}
		s.Defer(func(context.Context) { putBack() })
		defer putBack()
	}
	ah := agentHome(s)
	home := []string{"HOME=" + ah}
	// devvm agent import copies the agent's settings into the sandbox's agent store, which the VM gets
	// them from, and leaves the links out.
	store := filepath.Join(s.sb.config(), "devvm", "agents", "claude")
	_ = os.RemoveAll(store)
	skills := filepath.Join(store, ".claude", "skills")
	s.Check("devvm agent import copies the settings into the store, without the history or the links out of the folder", func() error {
		r, err := DevvmIn(s, "", "", home, "agent", "import", "--force", "claude")
		if err != nil {
			return err
		}
		abs, _ := os.Lstat(filepath.Join(skills, "escape-abs"))
		switch {
		case !strings.Contains(readText(filepath.Join(skills, "e2e", "SKILL.md")), "e2e") ||
			!strings.Contains(readText(filepath.Join(store, ".claude", "settings.json")), "e2e-settings"):
			return errors.New("the settings weren't copied")
		case Exists(filepath.Join(store, ".claude", "projects")) || Exists(filepath.Join(skills, "escape")) || (abs != nil && abs.Mode()&os.ModeSymlink != 0):
			return errors.New("the history or a link was copied")
		case !grepF(r.Out, s.Msg("agent_import_link_left_out", "path="+ah+"/.claude/skills/escape")):
			return errors.New("it didn't say it left the link out:\n" + r.Out)
		}
		return nil
	})
	a, _ := s.NewProject("agents")
	_, _ = DevvmIn(s, a, "", append(home, "TZ=Europe/Paris", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-e2efake-plain"), "--network", "open", "--no-git-protect", "--agents", "claude")
	vm := s.Paths.VMOf(a)
	OwnVM(s, vm)
	mounts := func(format string) string {
		r, _ := s.Limactl("list", "--format", format, vm)
		return r.Stdout
	}
	s.Check("an agent folder of the store is mounted read-only at its path, and linked into the VM's home", func() error {
		if !strings.Contains(mounts("{{range .Config.Mounts}}{{.Location}}|{{.Writable}} {{end}}"), skills+"|false ") ||
			strings.Contains(MountLine(s, vm), ah+"/") {
			return errors.New("the store's folder isn't mounted read-only, or the home is")
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "touch", skills+"/x"); err == nil {
			return errors.New("root wrote into it")
		}
		_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", "grep -q e2e ~/.claude/skills/e2e/SKILL.md")
		return err
	})
	// Links in the store's folder that lead out of it, relative and absolute, as you might make there.
	_ = os.Symlink("../../../../../../agent-home/outside/e2e-not-a-secret.txt", filepath.Join(skills, "escape"))
	_ = os.Symlink(filepath.Join(ah, "outside", "e2e-not-a-secret.txt"), filepath.Join(skills, "escape-abs"))
	s.Check("a symlink in an agent folder, relative or absolute, can't reach the host's files outside it", func() error {
		l, err := s.Limactl("shell", "--workdir", "/", vm, "readlink", skills+"/escape", skills+"/escape-abs")
		if err != nil {
			return err
		}
		o, _ := s.Limactl("shell", "--workdir", "/", vm, "sudo", "cat", skills+"/escape", skills+"/escape-abs")
		if !regexp.MustCompile(`(?s)outside/e2e-not-a-secret\.txt.*outside/e2e-not-a-secret\.txt$`).MatchString(Trim(l.Stdout)) || strings.Contains(o.Out, "e2e-outside-marker") {
			return fmt.Errorf("links: %s\nread: %s", l.Stdout, o.Out)
		}
		return nil
	})
	_ = os.Remove(filepath.Join(skills, "escape"))
	_ = os.Remove(filepath.Join(skills, "escape-abs"))
	s.Check("its settings are copied in, its history never", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `grep -q e2e-settings ~/.claude/settings.json && [[ ! -e ~/.claude/projects/p ]]`); err != nil {
			return err
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "grep", "-rqs", "e2e-history", "/home", "/root", "/etc"); err == nil {
			return errors.New("the history is in the VM")
		}
		return nil
	})
	s.Check("the VM gets the host's git identity, timezone and its hostname", func() error {
		_, err := s.InVM(vm, `[[ $(git config --global user.name) == 'E2E User' && $(git config --global user.email) == e2e@example.com &&
     $(timedatectl show -p Timezone --value) == Europe/Paris && $(hostname) == agents ]]`)
		return err
	})
	s.Check("an open VM gets the Claude Code token's placeholder, never the token", func() error {
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "bash", "-lc", "printenv CLAUDE_CODE_OAUTH_TOKEN")
		if Trim(r.Stdout) != "sk-ant-oat01-devvm-proxy-managed-"+s.Paths.State(vm)["net.made.proxy_salt"] {
			return errors.New("the VM's token: " + r.Stdout)
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "grep", "-rqs", "-e", "e2efake-plain", "/etc", "/home", "/root"); err == nil {
			return errors.New("the token is in the VM")
		}
		return nil
	})
	projectAgentSave(s, a, vm, ah, store)
	projectHistory(s, a, vm, ah)
}

// devvm agent save: the VM's changed settings file and a folder of its own, with a link out of it and a
// fake login beside them; refused without a terminal, saved after a yes, never the login or the link.
func projectAgentSave(s *S, a, vm, ah, store string) {
	home := []string{"HOME=" + ah}
	_, _ = s.InVM(vm, `echo "{\"theme\":\"e2e-settings\",\"e2eSaved\":true}" > ~/.claude/settings.json && mkdir -p ~/.claude/commands &&
  echo "# e2e command" > ~/.claude/commands/e2e.md && ln -s /etc/hostname ~/.claude/commands/escape &&
  echo e2e-credentials > ~/.claude/.credentials.json`)
	settings := filepath.Join(store, ".claude", "settings.json")
	commands := filepath.Join(store, ".claude", "commands")
	s.Check("devvm agent save without a terminal shows what changed, then saves nothing", func() error {
		r, err := DevvmIn(s, a, "", home, "agent", "save")
		switch {
		case err == nil:
			return errors.New("it saved without a terminal")
		case !Matches(r.Out, `^\+  "e2eSaved": true`) || !HasLine(r.Out, "  A e2e.md") || !grepF(r.Out, s.Msg("agent_save_refused")):
			return errors.New("it didn't show what changed and refuse:\n" + r.Out)
		case strings.Contains(readText(settings), "e2eSaved") || Exists(commands):
			return errors.New("it saved something")
		}
		return nil
	})
	s.Check("after a yes it saves them into the store, never the VM's login or a link out of the folder", func() error {
		r, err := DevvmIn(s, a, "y\n", append(home, "DEVVM_PIPED_ANSWERS=1"), "agent", "save")
		if err != nil {
			return err
		}
		login := NoLeak([]string{"e2e-credentials"}, store, filepath.Join(s.Paths.Devvm, "logins"))
		switch {
		case !grepF(r.Out, s.Msg("agent_settings_saved", "agent=claude", "dir="+store, "vm="+vm)) ||
			!grepF(r.Out, s.Msg("agent_save_link_left_out", "path=.claude/commands/escape", "vm="+vm)):
			return errors.New("it didn't say what it saved:\n" + r.Out)
		case !strings.Contains(readText(settings), "e2eSaved") || !strings.Contains(readText(filepath.Join(commands, "e2e.md")), "e2e command"):
			return errors.New("it didn't save them")
		case Exists(filepath.Join(commands, "escape")):
			return errors.New("it saved the link")
		case login != nil:
			return fmt.Errorf("it saved the login: %w", login)
		}
		return nil
	})
	_ = os.MkdirAll(filepath.Join(store, ".claude", "agents"), 0o755) //nolint:gosec // the store's folder, as its user's mkdir makes it
	locations := func() string { return MountLine(s, vm) }
	s.Check("a new agent folder in the store is noticed on launch, and waits for a restart", func() error {
		r, _ := DevvmIn(s, a, "exit\n", home)
		if !grepF(r.Out, FirstLine(s.Msg("next_start_mounts", "vm="+vm))) {
			return errors.New("the launch didn't notice it:\n" + r.Out)
		}
		return nil
	})
	s.Check("restart mounts it", func() error {
		if _, err := DevvmIn(s, a, "exit\n", home, "restart"); err != nil {
			return err
		}
		if !strings.Contains(locations(), filepath.Join(store, ".claude", "agents")+" ") {
			return errors.New("not mounted: " + locations())
		}
		_, err := s.Limactl("shell", "--workdir", "/", vm, "ls", filepath.Join(store, ".claude", "agents"))
		return err
	})
	_ = os.RemoveAll(filepath.Join(store, ".claude", "skills"))
	s.Check("a removed one is unmounted, and the VM starts", func() error {
		if _, err := DevvmIn(s, a, "exit\n", home, "restart"); err != nil {
			return err
		}
		if strings.Contains(locations(), "skills") || s.Status(vm) != "Running" {
			return errors.New("still mounted, or not running: " + locations())
		}
		return nil
	})
}

// tarList is the regular files' paths in an export, one a line, sorted.
func tarList(f string) (string, error) {
	files, err := exportFiles(f)
	return strings.Join(slices.Sorted(maps.Keys(files)), "\n"), err
}

// historyOK checks an export's list: the transcript, and no login.
func historyOK(list string) error {
	if !HasLine(list, ".claude/projects/e2e/t.jsonl") || strings.Contains(list, "credentials") {
		return errors.New("the export:\n" + list)
	}
	return nil
}

func projectHistory(s *S, a, vm, ah string) {
	home := []string{"HOME=" + ah}
	export := filepath.Join(s.sb.work, "export")
	ex := filepath.Join(s.Paths.Devvm, "exports")
	madeFile(s.sb, export)
	madeFile(s.sb, ex)
	_, _ = s.InVM(vm, `mkdir -p ~/.claude/projects/e2e && echo e2e-transcript > ~/.claude/projects/e2e/t.jsonl && echo secret > ~/.claude/.credentials.json`)
	_, _ = s.Limactl("stop", vm)
	s.Check("export starts the stopped VM and saves its agents' history to a file, without their logins", func() error {
		if _, err := DevvmIn(s, a, "", home, "history", "export", filepath.Join(export, "h.tar.gz")); err != nil {
			return err
		}
		if s.Status(vm) != "Running" {
			return errors.New("it isn't running")
		}
		list, err := tarList(filepath.Join(export, "h.tar.gz"))
		if err != nil {
			return err
		}
		return historyOK(list)
	})
	exported := func() (string, error) {
		m, _ := filepath.Glob(filepath.Join(ex, vm+"-*.tar.gz"))
		if len(m) != 1 {
			return "", fmt.Errorf("exports: %v", m)
		}
		return m[0], nil
	}
	s.Check("rm exports its agents' history, without their logins, only yours to read, then deletes it", func() error {
		_ = os.MkdirAll(ex, 0o700)
		_ = os.Chmod(ex, 0o755) //nolint:gosec // a folder open to others, which devvm makes yours alone
		if _, err := DevvmIn(s, a, "", home, "rm", "--force"); err != nil {
			return err
		}
		if Listed(s, vm) {
			return errors.New(vm + " is still there")
		}
		f, err := exported()
		if err != nil {
			return err
		}
		list, err := tarList(f)
		if err != nil {
			return err
		}
		if err := historyOK(list); err != nil {
			return err
		}
		de, _ := os.Stat(ex)
		fe, _ := os.Stat(f)
		if de == nil || fe == nil || de.Mode().Perm() != 0o700 || fe.Mode().Perm() != 0o600 {
			return errors.New("the export or its folder isn't yours alone")
		}
		return nil
	})
	// The same folder's new VM: without a terminal it names the export rm saved, and devvm history
	// import puts it in, without the logins; an export with a path outside the history imports nothing.
	s.Check("a new VM in the same folder names the export, without a terminal", func() error {
		f, err := exported()
		if err != nil {
			return err
		}
		r, err := DevvmIn(s, a, "", home, "--network", "open", "--no-git-protect", "--agents", "claude")
		if err != nil {
			return err
		}
		if !grepF(r.Out, s.Msg("history_import_later", "file="+f, "vm="+vm)) {
			return errors.New("it didn't name the export:\n" + r.Out)
		}
		return nil
	})
	s.Check("history import puts the agents' history into it, without their logins", func() error {
		if _, err := DevvmIn(s, a, "", home, "history", "import"); err != nil {
			return err
		}
		_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `[[ $(cat ~/.claude/projects/e2e/t.jsonl) == e2e-transcript && ! -e ~/.claude/.credentials.json ]] &&
    grep -q e2e-settings ~/.claude/settings.json && [[ -z $(ls -A ~/.cache | grep devvm-import) ]]`)
		if err != nil {
			d, _ := s.InVM(vm, `cat ~/.claude/projects/e2e/t.jsonl; ls -la ~/.claude ~/.cache; head -c 300 ~/.claude/settings.json`)
			return fmt.Errorf("%w\nthe VM has:\n%s", err, d.Out)
		}
		return nil
	})
	s.Check("an export with a path outside the agents' history imports nothing", func() error {
		ev := filepath.Join(export, "evil")
		_ = os.RemoveAll(ev)
		if err := os.MkdirAll(filepath.Join(ev, "d"), 0o700); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(ev, "e2e-escape"), []byte("e2e-escape\n"), 0o600)
		if _, err := s.Run(Cmd{Args: []string{"tar", "-czPf", "../evil.tar.gz", "../e2e-escape"}, Dir: filepath.Join(ev, "d")}); err != nil {
			return err
		}
		r, err := DevvmIn(s, a, "", home, "history", "import", filepath.Join(ev, "evil.tar.gz"))
		if err == nil || !grepF(r.Out, s.Msg("history_import_unsafe", "file="+filepath.Join(ev, "evil.tar.gz"))) {
			return errors.New("not refused:\n" + r.Out)
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "grep", "-rqs", "e2e-escape", "/home", "/root", "/tmp", "/etc"); err == nil {
			return errors.New("the file got into the VM")
		}
		return nil
	})
	_, _ = DevvmIn(s, a, "", home, "rm", "--force", "--no-export")
}

// A home of its own, without keys. A throwaway agent with a throwaway key stands for yours; devvm's
// own agent (its socket in the sandbox's data folder) gets another.
func projectSSHAgents(s *S) {
	s.Section("SSH agents")
	sa := filepath.Join(s.sb.work, "agent-ssh")
	madeFile(s.sb, sa)
	madeFile(s.sb, filepath.Join(s.Paths.Devvm, "agent-keys.json"))
	_ = os.RemoveAll(sa)
	_ = os.MkdirAll(filepath.Join(sa, "home"), 0o700)
	if r, err := filepath.EvalSymlinks(sa); err == nil {
		sa = r
	}
	for _, k := range [][2]string{{"devvm-e2e-user-key", "user-key"}, {"devvm-e2e-key", "devvm-key"}} {
		_, _ = s.Run(Cmd{Args: []string{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", k[0], "-f", filepath.Join(sa, k[1])}})
	}
	userAgent, _ := SSHAgent(s, filepath.Join(sa, "user.sock"))
	// asUser: a command with that home and agent.
	asUser := []string{"HOME=" + filepath.Join(sa, "home"), "SSH_AUTH_SOCK=" + filepath.Join(sa, "user.sock")}
	_, _ = s.Run(Cmd{Args: []string{"ssh-add", "-q", filepath.Join(sa, "user-key")}, Env: asUser})
	g, _ := s.NewProject("agentvm")
	_, _ = DevvmIn(s, g, "", asUser, "--network", "open", "--no-git-protect", "--ssh-agent")
	vm := s.Paths.VMOf(g)
	OwnVM(s, vm)
	sshConfig := filepath.Join(s.Paths.Devvm, "ssh_config")
	// keys is the comments of the keys the command's ssh-add -l lists.
	keys := func(args ...string) string {
		r, _ := s.Run(Cmd{Args: append(args, "ssh-add", "-l"), Env: asUser})
		return KeyComments(r.Stdout)
	}
	inVM := []string{"limactl", "shell", "--workdir", "/", vm}
	byName := []string{"ssh", "-F", sshConfig, vm}
	s.Check("--ssh-agent: the VM uses your agent's keys", func() error {
		if k := keys(inVM...); k != "devvm-e2e-user-key" {
			return errors.New("the VM's keys: " + k)
		}
		return nil
	})
	s.Check("ssh-agent add loads a key into devvm's own agent", func() error {
		r, _ := DevvmIn(s, "", "", asUser, "ssh-agent", "add", filepath.Join(sa, "devvm-key"))
		if !grepF(r.Out, s.Msg("agent_holds", "keys=devvm-e2e-key")) {
			return errors.New("devvm ssh-agent add:\n" + r.Out)
		}
		return nil
	})
	s.Check("ssh-agent devvm while it runs: after a launch, devvm's shell and ssh by name still use yours, until a restart", func() error {
		if _, err := DevvmIn(s, g, "", asUser, "ssh-agent", "devvm"); err != nil {
			return err
		}
		if _, err := DevvmIn(s, g, "exit\n", asUser); err != nil {
			return err
		}
		if k := keys(byName...); k != "devvm-e2e-user-key" {
			return errors.New("ssh by name's keys: " + k)
		}
		r, _ := DevvmIn(s, g, "ssh-add -l\nexit\n", asUser)
		var shell []string
		for l := range strings.SplitSeq(r.Stdout, "\n") {
			if f := strings.Fields(l); strings.Contains(l, "SHA256:") && len(f) >= 3 {
				shell = append(shell, f[2])
			}
		}
		if strings.Join(shell, "\n") != "devvm-e2e-user-key" {
			return fmt.Errorf("devvm's shell's keys: %v", shell)
		}
		return nil
	})
	s.Check("ssh-agent devvm, then restart: the VM uses only that key, not yours", func() error {
		if _, err := DevvmIn(s, g, "", asUser, "ssh-agent", "devvm"); err != nil {
			return err
		}
		if _, err := DevvmIn(s, g, "exit\n", asUser, "restart"); err != nil {
			return err
		}
		if k := keys(inVM...); k != "devvm-e2e-key" {
			return errors.New("the VM's keys: " + k)
		}
		return nil
	})
	s.Check("so does an editor connecting by name, whatever agent it has", func() error {
		if k := keys(byName...); k != "devvm-e2e-key" {
			return errors.New("ssh by name's keys: " + k)
		}
		return nil
	})
	projectSSHAgentOff(s, g, vm, sa, asUser)
	StopPid(userAgent)
	_, _ = s.Run(Cmd{Args: []string{"pkill", "-f", "[s]sh-agent -a " + filepath.Join(s.Paths.Devvm, "agent.sock")}})
}

func projectSSHAgentOff(s *S, g, vm, sa string, asUser []string) {
	sshConfig := filepath.Join(s.Paths.Devvm, "ssh_config")
	s.Check("ssh-agent off, then restart: no agent in the VM", func() error {
		if _, err := DevvmIn(s, g, "", asUser, "ssh-agent", "off"); err != nil {
			return err
		}
		if _, err := DevvmIn(s, g, "exit\n", asUser, "restart"); err != nil {
			return err
		}
		if _, err := s.Run(Cmd{Args: []string{"limactl", "shell", "--workdir", "/", vm, "ssh-add", "-l"}, Env: asUser}); err == nil {
			return errors.New("the VM has an agent")
		}
		ls := strings.Split(readText(sshConfig), "\n")
		for i, l := range ls {
			if strings.HasPrefix(l, "Host ") && strings.HasSuffix(l, " "+vm) {
				for _, n := range ls[i:min(len(ls), i+9)] {
					if n == "  ForwardAgent no" {
						return nil
					}
				}
			}
		}
		return errors.New("its ssh config entry doesn't turn agent forwarding off")
	})
	s.Check("ssh-agent rm takes the key out of devvm's own agent and its record, and leaves the key file", func() error {
		if r, _ := DevvmIn(s, g, "", asUser, "ssh-agent"); !strings.Contains(r.Stdout, "devvm-e2e-key") {
			return errors.New("devvm ssh-agent: " + r.Stdout)
		}
		if _, err := DevvmIn(s, g, "", asUser, "ssh-agent", "rm", filepath.Join(sa, "devvm-key")); err != nil {
			return err
		}
		own, _ := s.Run(Cmd{Args: []string{"ssh-add", "-l"}, Env: []string{"SSH_AUTH_SOCK=" + filepath.Join(s.Paths.Devvm, "agent.sock")}})
		switch {
		case strings.Contains(own.Stdout, "devvm-e2e-key"):
			return errors.New("devvm's own agent still holds it")
		case strings.Contains(readText(filepath.Join(s.Paths.Devvm, "agent-keys.json")), "devvm-key"):
			return errors.New("its record is still there")
		case !Exists(filepath.Join(sa, "devvm-key")):
			return errors.New("the key file is gone")
		}
		if r, _ := DevvmIn(s, g, "", asUser, "ssh-agent"); Trim(r.Stdout) != "off" {
			return errors.New("devvm ssh-agent: " + r.Stdout)
		}
		return nil
	})
	s.Check("rm deletes the SSH agent VM", func() error {
		if _, err := DevvmIn(s, g, "", asUser, "rm", "--force", "--no-export"); err != nil {
			return err
		}
		if Listed(s, vm) {
			return errors.New(vm + " is still there")
		}
		return nil
	})
}

// remade is devvm layer add's VM, and what it keeps.
type remade struct {
	r, vm, aside, id string
}

// Two small layers of the sandbox's own, each leaving a file in the system.
func projectLayerAdd(s *S) {
	s.Section("devvm layer add")
	var scripts []string
	for _, l := range []string{"e2eadd", "e2eadd2"} {
		f, _ := s.Layer(l+".sh", fmt.Sprintf("#!/bin/bash\nset -euo pipefail\necho %s | sudo tee /etc/devvm-%s >/dev/null\n", l, l))
		pol, _ := s.Layer(l+".policy", "")
		scripts = append(scripts, f, pol)
	}
	s.Defer(func(ctx context.Context) {
		if runtime.GOOS == "darwin" {
			_ = s.sb.command(ctx, "security", "delete-generic-password", "-s", s.sb.service, "-a", "github").Run()
		}
		_ = os.Remove(filepath.Join(s.Paths.Devvm, "secrets", "github"))
	})
	m := &remade{}
	m.r, _ = s.NewProject("remade")
	m.vm, _, _ = s.Launch(m.r, "echo in-home > ~/e2e-home.txt; echo in-project > e2e-project.txt", "--network", "open", "--no-git-protect", "--ports", "3000")
	if len(m.vm) >= 14 {
		m.aside = "devvm-" + m.vm[len(m.vm)-14:] + "-old"
		OwnVM(s, m.aside)
	}
	m.id = s.Paths.State(m.vm)["id"]
	r, vm, aside := m.r, m.vm, m.aside
	st := func() map[string]string { return s.Paths.State(vm) }
	s.Check("a VM with ports, a rule and a secret binding", func() error {
		for _, c := range []Cmd{
			{Args: []string{s.sb.devvm, "ports", "3000,5173"}},
			{Args: []string{s.sb.devvm, "network", "allow", "example.com"}},
			{Args: []string{s.sb.devvm, "secret", "set", "github", "--stdin"}, Stdin: "ghp_E2EFAKE\n"},
			{Args: []string{s.sb.devvm, "secret", "use", "github"}},
		} {
			c.Dir = r
			if _, err := s.Run(c); err != nil {
				return err
			}
		}
		_, err := s.InVM(vm, "grep -qx in-home ~/e2e-home.txt")
		return err
	})
	s.Check("layer add without a terminal or --force refuses, and changes nothing", func() error {
		out, err := s.Devvm(r, "layer", "add", "e2eadd")
		if err == nil || !grepF(out.Out, s.Msg("no_input", "answer=--force")) || st()["image"] != "base" {
			return errors.New("not refused:\n" + out.Out)
		}
		return nil
	})
	s.Check("layer add --force makes it again with the layer, in a shell", func() error {
		out, err := DevvmIn(s, r, "echo e2e-remade-shell\n", nil, "layer", "add", "e2eadd", "--force")
		if err != nil {
			return err
		}
		if !HasLine(out.Out, "e2e-remade-shell") || st()["image"] != "e2eadd" || Listed(s, aside) {
			return errors.New("not made again in a shell:\n" + out.Out)
		}
		_, err = s.InVM(vm, "grep -qx e2eadd /etc/devvm-e2eadd")
		return err
	})
	s.Own("e2eadd")
	s.Check("it keeps its ID, ports, rule, secret binding and network choice", func() error {
		if err := m.kept(s); err != nil {
			return err
		}
		if st()["net.tier"] != "open" || Exists(filepath.Join(s.Paths.VMDir(vm), "home.tar.gz")) {
			return errors.New("its network choice changed, or its home's copy is left")
		}
		return nil
	})
	s.Check("it keeps the project folder and its home folder", func() error {
		if !HasLine(readText(filepath.Join(r, "e2e-project.txt")), "in-project") {
			return errors.New("the project's file is gone")
		}
		_, err := s.InVM(vm, "grep -qx in-home ~/e2e-home.txt")
		return err
	})
	projectLayerClean(s, m)
	projectLayerUpdate(s, m)
	_, _ = s.Devvm("", "secret", "rm", "github", "--force")
	for _, f := range scripts {
		_ = os.Remove(f)
	}
}

// kept checks what a VM made again keeps: its ID, ports, secret binding
// and rule.
func (m *remade) kept(s *S) error {
	st := s.Paths.State(m.vm)
	if st["id"] != m.id || st["ports.forward"] != "3000,5173" || st["secrets.names"] != "github" {
		return fmt.Errorf("id %s (was %s), ports %s, secrets %s", st["id"], m.id, st["ports.forward"], st["secrets.names"])
	}
	if !HasLine(readText(filepath.Join(s.Paths.VMDir(m.vm), "policy")), "example.com") {
		return errors.New("its rule is gone")
	}
	return nil
}

func projectLayerClean(s *S, m *remade) {
	r, vm := m.r, m.vm
	st := func() map[string]string { return s.Paths.State(vm) }
	s.Check("layer add --clean-home adds the next layer on top, with the image's home folder", func() error {
		if _, err := DevvmIn(s, r, "exit\n", nil, "layer", "add", "e2eadd2", "--force", "--clean-home"); err != nil {
			return err
		}
		if st()["image"] != "e2eadd+e2eadd2" || st()["id"] != m.id || !HasLine(readText(filepath.Join(r, "e2e-project.txt")), "in-project") {
			return errors.New("its image, ID or project changed: " + st()["image"])
		}
		if _, err := s.InVM(vm, "grep -qx e2eadd /etc/devvm-e2eadd && grep -qx e2eadd2 /etc/devvm-e2eadd2"); err != nil {
			return err
		}
		_, err := s.InVM(vm, "[[ ! -e ~/e2e-home.txt ]]")
		return err
	})
	s.Own("e2eadd+e2eadd2")
	s.Check("layer add of a layer it has changes nothing", func() error {
		out, err := s.Devvm(r, "layer", "add", "e2eadd", "--force")
		if err != nil {
			return err
		}
		if !grepF(out.Out, s.Msg("has_layers", "layers=e2eadd", "vm="+vm)) || st()["image"] != "e2eadd+e2eadd2" {
			return errors.New("devvm layer add e2eadd:\n" + out.Out)
		}
		return nil
	})
	s.Check("update on a current VM says so and changes nothing", func() error {
		out, err := s.Devvm(r, "update", "--force")
		if err != nil {
			return err
		}
		if !grepF(out.Out, s.Msg("vm_current", "image=e2eadd+e2eadd2", "vm="+vm)) || Listed(s, m.aside) {
			return errors.New("devvm update:\n" + out.Out)
		}
		return nil
	})
}

func projectLayerUpdate(s *S, m *remade) {
	r, vm := m.r, m.vm
	st := func() map[string]string { return s.Paths.State(vm) }
	_ = AppendFile(filepath.Join(s.Paths.Layers, "e2eadd2.sh"), "echo e2eupd | sudo tee /etc/devvm-e2eupd >/dev/null\n")
	chain := st()["chain"]
	outdated := s.Msg("vm_outdated", "changed=e2eadd+e2eadd2")
	s.Check("an edited layer's script makes it outdated, and the warning names devvm update", func() error {
		if _, err := s.InVM(vm, "echo in-home > ~/e2e-home.txt"); err != nil {
			return err
		}
		out, err := DevvmIn(s, r, "exit\n", nil)
		if err != nil {
			return err
		}
		if !grepF(out.Out, outdated) {
			return errors.New("no warning:\n" + out.Out)
		}
		return nil
	})
	// keptAll is what update and layer rm keep: home, settings, the project, and no VM set aside.
	keptAll := func(image, inVM string) error {
		if st()["image"] != image {
			return errors.New("its image is " + st()["image"])
		}
		if _, err := s.InVM(vm, inVM); err != nil {
			return err
		}
		if err := m.kept(s); err != nil {
			return err
		}
		if !HasLine(readText(filepath.Join(r, "e2e-project.txt")), "in-project") || Listed(s, m.aside) {
			return errors.New("its project's file is gone, or a VM is set aside")
		}
		return nil
	}
	s.Check("update --force makes it again from the image's new build, keeping home and settings", func() error {
		if _, err := DevvmIn(s, r, "exit\n", nil, "update", "--force"); err != nil {
			return err
		}
		if st()["chain"] == chain {
			return errors.New("its chain didn't change")
		}
		return keptAll("e2eadd+e2eadd2", "grep -qx e2eupd /etc/devvm-e2eupd && grep -qx in-home ~/e2e-home.txt")
	})
	s.Own("e2eadd+e2eadd2")
	s.Check("once updated, a launch doesn't warn", func() error {
		out, err := DevvmIn(s, r, "exit\n", nil)
		if err != nil {
			return err
		}
		if grepF(out.Out, outdated) {
			return errors.New("it warns:\n" + out.Out)
		}
		return nil
	})
	s.Check("layer rm --force makes it again without the layer, keeping home and settings", func() error {
		if _, err := DevvmIn(s, r, "exit\n", nil, "layer", "rm", "e2eadd", "--force"); err != nil {
			return err
		}
		return keptAll("e2eadd2", "[[ ! -e /etc/devvm-e2eadd ]] && grep -qx e2eadd2 /etc/devvm-e2eadd2 && grep -qx in-home ~/e2e-home.txt")
	})
	s.Own("e2eadd2")
	s.Check("rm deletes the VM made again", func() error {
		if _, err := s.Devvm(r, "rm", "--force", "--no-export"); err != nil {
			return err
		}
		if Listed(s, vm) || Listed(s, m.aside) {
			return errors.New("the VM or the one set aside is still there")
		}
		return nil
	})
}
