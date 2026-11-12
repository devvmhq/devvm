package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The guest is the VM the stubs make believe in, reached through limactl
// shell and through ssh alike: it answers what devvm runs in a VM. Its
// files are $STUB_DIR/vmfs (a --clone VM's repo and worktrees, its git
// run here, on them) and $STUB_DIR/vmhome-<vm> (a VM's home folder, as a
// remake reads it). What it's sent is kept: a script fed to bash -s has
// its first line past devvm's prelude appended to $STUB_DIR/scripts, a
// VM's setup is $STUB_DIR/sync.tar, a base build's packages
// $STUB_DIR/packages-in.tar, what a remake puts in a new disk's home
// $STUB_DIR/home-in.tgz, and an import of agents' history
// $STUB_DIR/history-in.tgz. Its switches, in $STUB_DIR:
//
//	shell-fails         every shell fails
//	shell-fails-on      a shell whose arguments hold its text fails (a home folder's copy too)
//	shell-hangs         every shell hangs (touching hanging first)
//	shell-waits-on      a shell whose arguments hold its text touches waiting, then waits
//	                    until this file is gone and carries on
//	sync-fails-with     the setup fails, and this file is the VM's /run/devvm/tools-unmounted
//	history-unsafe      an import of agents' history finds a path outside them (and fails)
//	history-floods      an export of agents' history writes, stdout and stderr, until they're gone
//	packages-out.tar    the packages a base build gives back, else none
//	agent-save.tar      the stream devvm agent save reads of the VM's agents' settings, else
//	                    an empty one
//	net-direct          a direct connection (curl --noproxy, past the VM's proxy) works,
//	                    which fails as a confined VM's does without it
//	opencode-refuses    devvm's opencode.sh says opencode refused the provider this file names
//	opencode-floods     devvm's opencode.sh prints, and writes its stderr, until they're gone//	hostkeys-fails      the VM doesn't show its ssh host keys, which are otherwise one
//	                    made-up ed25519 key per VM, its name the key
//	guest-ports         what listens in the VM, as /proc/net/tcp has it, for a port
//	                    forwarder's master connection
//	link-taken          ADDRESS:PORT lines: a link's remote forwards the VM refuses
//	vmhome-<vm>         the VM's home folder's files (none when it's missing)
//	tools-glibc-<tool>  the glibc a shared tool's image needs (limactl.go's tools build)
//	mount-unmounted     the project folder isn't a mount point (the remount's check,
//	                    mountOptions, and a running VM's, which answers devvm-not-mounted)
//	remount-fails       the project folder's remount (umount, then mount) fails, as an
//	                    Alpine guest's mount of a share it just unmounted does, and leaves
//	                    the folder unmounted until the VM's next start, which mounts it:
//	                    the stubs mark it with unmounted-<vm>, which start removes
//	paste-count         what paste.sh count and rm print, the number of pasted files (else 0)
//	paste-in            where paste.sh put's stdin goes, with its folder and name in paste-put
type guest struct {
	*call
	vm string // the VM's name, "" over ssh
}

// shellPrefix starts the command line Exec gives limactl shell: the
// folder, then the words, each quoted.
const shellPrefix = "cd '/' && exec "

// shell is limactl shell: a command run in the guest.
func (l *lima) shell() int {
	g := &guest{call: l.call, vm: l.name(0)}
	if g.mentions("shell-waits-on") {
		g.touch("waiting")
		for g.on("shell-waits-on") {
			g.sleep(100 * time.Millisecond)
		}
	}
	_ = g.put("shell-agent", []byte(g.agent()+"\n"))
	if line := g.last(); g.cloneGit(line) {
		if g.on("shell-fails") {
			return 1
		}
		return g.git(shellWords(strings.TrimPrefix(line, shellPrefix)))
	}
	if strings.Join(g.args[max(0, len(g.args)-2):], " ") == "bash -s" || strings.HasSuffix(g.joined(), "exec 'bash' '-s'") {
		g.logScript()
	}
	if code, done := g.files(); done {
		return code
	}
	return g.answer()
}

// cloneGit reports whether line is a --clone VM's git: git -C DIR, the
// script that makes its clone's repos, the one that reads them for the
// clone check, or a workspace clone's copy of the folder's files (a tar
// read from stdin into the folder).
func (g *guest) cloneGit(line string) bool {
	if strings.HasPrefix(line, shellPrefix+"'git' '-C' ") {
		return true
	}
	return strings.HasPrefix(line, shellPrefix+"'sh' '-c' ") &&
		(strings.Contains(line, "receive.denyCurrentBranch") || strings.Contains(line, "status --porcelain") ||
			strings.Contains(line, "--no-same-owner"))
}

// vmfs is the guest's files on the host, under the stubs' folder.
func (g *guest) vmfs(dir string) string { return g.path("vmfs") + dir }

// git runs a clone's command on the guest's files: git -C DIR …, or sh -c
// SCRIPT NAME DIR …, which runs with sudo as a plain call. git worktree
// list gives its paths as the guest's, without the stubs' folder.
func (g *guest) git(words []string) int {
	if len(words) < 3 {
		return 1
	}
	if words[0] != "git" {
		sudo := "sudo() { \"$@\"; }; export -f sudo\n"
		argv := []string{"bash", "-c", sudo + words[2]}
		if len(words) > 3 {
			argv = append(argv, words[3])
		}
		if len(words) > 4 {
			argv = append(append(argv, g.vmfs(words[4])), words[5:]...)
		}
		return g.replace(argv, nil)
	}
	argv := append([]string{"git", "-C", g.vmfs(words[2])}, words[3:]...)
	if len(words) < 5 || words[3] != "worktree" || words[4] != "list" {
		return g.replace(argv, nil)
	}
	out, code := g.output(argv...)
	vmfs := g.vmfs("")
	if p, err := filepath.EvalSymlinks(g.dir()); err == nil {
		vmfs = filepath.Join(p, "vmfs")
	}
	recs := bytes.Split(out, []byte{0})
	for _, r := range recs[:len(recs)-1] {
		r = bytes.Replace(r, []byte("worktree "+vmfs), []byte("worktree "), 1)
		g.print(string(r) + "\x00")
	}
	return code
}

// preludeRE starts devvm's prelude, the first lines of a build script.
var preludeRE = regexp.MustCompile(`^# devvm.s prelude`)

// logScript appends a bash -s script's first line, past devvm's prelude
// (assets/prelude.sh, which ends with its function's closing brace and a
// blank line) when it starts with it, to $STUB_DIR/scripts. It reads the
// whole script.
func (g *guest) logScript() {
	b, _ := io.ReadAll(g.stdin)
	g.stdin = bytes.NewReader(nil)
	lines := strings.Split(string(b), "\n")
	at := 1
	if len(lines) > 1 && preludeRE.MatchString(lines[1]) {
		at = len(lines)
		for i := 1; i < len(lines); i++ {
			if lines[i] == "}" {
				at = i + 2
				break
			}
		}
	}
	line := ""
	if at < len(lines) {
		line = lines[at]
	}
	g.logLine(g.path("scripts"), line)
}

// files answers what moves files in and out of the guest: a home folder's
// copy, a base build's packages and a VM's setup.
func (g *guest) files() (code int, done bool) {
	all := g.joined()
	out, in := strings.Contains(all, "tar -czf - --one-file-system"), strings.Contains(all, "tar -xzf - --skip-old-files")
	switch {
	case (out || in) && g.mentions("shell-fails-on"):
		return 1, true
	case out:
		home := g.path("vmhome-" + g.vm)
		if err := mkdir(home); err != nil {
			return g.status(err), true
		}
		// macOS's tar adds AppleDouble ._ files, which a VM's GNU tar never has.
		return g.replace([]string{"tar", "-czf", "-", "-C", home, "."}, []string{"COPYFILE_DISABLE=1"}), true
	case in:
		return g.status(g.copyIn("home-in.tgz")), true
	case strings.Contains(all, "ls -d .claude ") && g.on("history-floods"):
		return g.flood("history flood\n"), true
	case strings.Contains(all, "--quoting-style=escape"):
		if err := g.copyIn("history-in.tgz"); err != nil {
			return g.status(err), true
		}
		if g.on("history-unsafe") {
			g.print("devvm-history-unsafe\n")
			return 1, true
		}
		return 0, false
	case strings.Contains(all, ".devvm-save"):
		if g.on("shell-fails") || g.mentions("shell-fails-on") {
			return 1, true
		}
		g.print(string(g.raw("agent-save.tar", string(make([]byte, 1024)))))
		return 0, true
	case strings.Contains(all, "'devvm-paste' 'put' "):
		if g.on("shell-fails") || g.mentions("shell-fails-on") {
			return 1, true
		}
		if g.on("paste-hangs") {
			g.hang() // then goes on, as a write that's only slow
		}
		words := shellWords(strings.TrimPrefix(g.last(), shellPrefix))
		if len(words) < 7 {
			return 2, true
		}
		if err := g.put("paste-put", []byte(words[5]+" "+words[6]+"\n")); err != nil {
			return g.status(err), true
		}
		return g.status(g.copyIn("paste-in")), true
	case strings.Contains(all, "'devvm-paste' 'count' "), strings.Contains(all, "'devvm-paste' 'rm' "):
		if g.on("shell-fails") || g.mentions("shell-fails-on") {
			return 1, true
		}
		if g.on("paste-floods") {
			return g.flood("1\n"), true
		}
		if g.on("paste-hangs") {
			g.hang() // then goes on, as a count that's only slow
		}
		g.print(g.textOr("paste-count", "0") + "\n")
		return 0, true
	case strings.Contains(all, "tar -x -C "):
		return g.status(g.copyIn("packages-in.tar")), true
	case strings.Contains(all, "tar -c -C "):
		g.print(string(g.raw("packages-out.tar", string(make([]byte, 1024)))))
		return 0, true
	case strings.Contains(all, "tar -xf - "):
		if err := g.copyIn("sync.tar"); err != nil {
			return g.status(err), true
		}
		if g.on("sync-fails-with") {
			return 1, true
		}
	}
	return 0, false
}

// flood prints line on stdout, and eight times as much on stderr, until
// a write fails, as a VM that writes without end, and fails.
func (g *guest) flood(line string) int {
	text := strings.Repeat(line, 1<<10)
	for {
		if _, err := io.WriteString(g.stdout, text); err != nil {
			return 1
		}
		for range 8 {
			if _, err := io.WriteString(g.stderr, text); err != nil {
				return 1
			}
		}
	}
}

// answer is the rest the guest answers: the setup's unmounted tools, a
// direct connection, the VM's host keys, then the shell switches.
func (g *guest) answer() int {
	all := g.joined()
	switch {
	case strings.Contains(all, "/run/devvm/tools-unmounted"):
		if !g.on("sync-fails-with") {
			return 1
		}
		g.print(string(g.raw("sync-fails-with", "")))
		return 0
	case strings.Contains(all, "'curl' '--noproxy'") && !g.on("net-direct"):
		return 7
	case strings.HasSuffix(all, "'opencode.sh'") && g.on("opencode-floods"):
		return g.flood("devvm-opencode-failed flood\n")
	case strings.HasSuffix(all, "'opencode.sh'") && g.on("opencode-refuses"):
		g.print("devvm-opencode-failed " + strings.TrimSpace(string(g.raw("opencode-refuses", ""))) + "\n")
		return 1
	case strings.Contains(all, "/etc/ssh/ssh_host_"):
		if g.on("hostkeys-fails") {
			return 1
		}
		g.print(hostKey(g.vm) + "\nnot a key\n")
		return 0
	case strings.Contains(all, "umount ") && g.on("remount-fails"):
		g.touch("unmounted-" + g.vm)
		return 4
	case strings.Contains(all, "/proc/self/mountinfo"):
		g.print(g.mountinfo(all) + "\n")
		return 0
	case strings.Contains(all, "echo devvm-mounted"):
		if g.unmounted() {
			g.print("devvm-not-mounted\n")
		} else {
			g.print("devvm-mounted\n")
		}
		return 0
	case g.on("shell-fails"), g.mentions("shell-fails-on"):
		return 1
	case g.on("shell-hangs"):
		return g.hang()
	}
	return 0
}

// mountinfo is the project folder's line of /proc/self/mountinfo, which
// devvm's check (all) asks for by the folder that mountpoint -q names: a
// 9p share with mountOptions's options; $STUB_DIR/mount-unmounted, or a
// remount that failed (unmounted), makes it not a mount point (devvm's
// check prints its own word then).
func (g *guest) mountinfo(all string) string {
	if g.unmounted() {
		return "devvm-not-mounted"
	}
	dir := "/"
	if m := mountpointArg.FindStringSubmatch(all); m != nil {
		dir = m[1]
	}
	dir = strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(dir)
	return "1109 29 0:61 / " + dir + " rw,relatime shared:604 - 9p mount0 " + g.mountOptions()
}

// unmounted reports whether the project folder isn't a mount point:
// mount-unmounted, or the VM's remount failed since its start
// (remount-fails).
func (g *guest) unmounted() bool { return g.on("mount-unmounted") || g.on("unmounted-"+g.vm) }

// mountpointArg finds the folder devvm's check gives mountpoint -q.
var mountpointArg = regexp.MustCompile(`mountpoint -q '([^']*)'`)

// mountOptions is the project folder's 9p file system options, as the
// kernel shows them in the VM: cache=0x5 for its writable mount's mmap,
// Lima's default, and no cache option for none.
func (g *guest) mountOptions() string {
	opts := "rw,access=client,msize=131072,trans=virtio"
	st, err := g.state()
	if err != nil {
		return opts
	}
	var ms []struct {
		Writable bool `json:"writable"`
		NineP    struct {
			Cache string `json:"cache"`
		} `json:"9p"`
	}
	if in, ok := st.Instances[g.vm]; ok && json.Unmarshal([]byte(in.Config["mounts"]), &ms) == nil {
		for _, m := range ms {
			if m.Writable && m.NineP.Cache != "none" {
				return "rw,cache=0x5,access=client,msize=131072,trans=virtio"
			}
		}
	}
	return opts
}

// hostKey is a VM's made-up ssh host key: an ed25519 key in ssh's wire
// format, its type, then the VM's name cut or padded to 32 bytes.
func hostKey(vm string) string {
	k := (vm + strings.Repeat(".", 32))[:32]
	blob := "\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20" + k
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte(blob)) + " root@" + vm
}

// ports answers a port forwarder's master connection: it reads the
// script it's fed, answers with what listens in the VM and ".", then
// waits to be stopped.
func (g *guest) ports() int {
	g.drain()
	g.print(string(g.raw("guest-ports", "")) + ".\n")
	g.sleep(masterLife)
	return 0
}

// pack runs git's receive-pack or upload-pack on the guest's files, for
// host git's push to and fetch from a --clone VM over ssh.
func (g *guest) pack(command string) int {
	words := shellWords(command)
	if len(words) < 2 {
		return 255
	}
	return g.replace([]string{"git", strings.TrimPrefix(words[0], "git-"), g.vmfs(words[1])}, nil)
}
