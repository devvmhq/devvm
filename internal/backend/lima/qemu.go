package lima

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
)

// On Linux hosts Lima runs QEMU, with a user-mode network. Every VM starts
// with QEMU_SYSTEM_<ARCH> pointing at a wrapper devvm writes, which runs
// devvm _qemu exec. For a VM that must reach nothing, or nothing but
// devvm's proxy, it adds restrict=on (no traffic in or out but Lima's own
// forwards) to Lima's -netdev and, for the proxy, a guestfwd: one address
// in the VM that QEMU connects to the proxy, through devvm _qemu connect,
// a process for each connection, connectMax at once.
// When it can't, it refuses to start QEMU: never unrestricted. Only a VM
// on Lima's own network says so (NetEnv). A start with pins runs QEMU in a
// namespace that pins them (pins.go).

// guestFwdHost and guestFwdPort are the proxy's address as every guest
// sees it.
const (
	guestFwdHost = "192.168.5.4"
	guestFwdPort = 3128
)

// GuestFwdEnv names the variable holding the command QEMU runs for each
// connection to the proxy's address in the guest. Unset, nothing is
// forwarded.
const GuestFwdEnv = "DEVVM_GUESTFWD"

// qemuArch is QEMU's name for a Go architecture.
func qemuArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return goarch
}

// qemuProgram is the QEMU Lima runs on this architecture.
func qemuProgram(goarch string) string { return "qemu-system-" + qemuArch(goarch) }

// qemuEnvVar is the variable Lima takes that program's path from.
func qemuEnvVar(goarch string) string {
	return "QEMU_SYSTEM_" + strings.ToUpper(qemuArch(goarch))
}

var wrapperTmpl = template.Must(template.New("qemu").Funcs(template.FuncMap{"shq": shell.Quote}).Parse(`#!/bin/sh
# Written by devvm: QEMU with devvm's network restrictions and pins.
exec {{shq .}} _qemu exec "$0" "$@"
`))

// writeWrapper writes the QEMU wrapper into dir and returns its path.
func writeWrapper(dir, self, goarch string) (string, error) {
	var b bytes.Buffer
	if err := wrapperTmpl.Execute(&b, self); err != nil {
		return "", err
	}
	w := filepath.Join(dir, qemuProgram(goarch))
	return w, state.WriteFileMode(w, b.Bytes(), 0o700)
}

// guestFwdCmd is the command QEMU runs for each connection to the proxy's
// address: devvm joining it to the proxy's port, holding one of the slots
// in slots (Connect). QEMU runs it with /bin/sh -c, so the paths are
// quoted.
func guestFwdCmd(self, proxyAddr, slots string) string {
	return shell.Quote(self) + " _qemu connect " + proxyAddr + " " + shell.Quote(slots)
}

// RestrictNetdev returns QEMU's arguments with restrict=on, and a guestfwd
// running guestfwd unless it's "", added to Lima's one -netdev
// user,id=net0,…. Arguments that define no network (Lima probing QEMU:
// -accel help, -netdev help) come back as they are. Any other shape is
// refused: another network (-netdev, -nic, -net, -readconfig), a -netdev
// that already says something about restrict, or an option as -netdev's
// value.
func RestrictNetdev(args []string, guestfwd string) ([]string, error) {
	out := make([]string, len(args))
	copy(out, args)
	netdevs, done := 0, false
	for i := 0; i < len(out); i++ {
		opt, ok := option(out[i])
		if !ok {
			continue
		}
		switch opt {
		case "netdev":
			if i+1 >= len(out) {
				continue // QEMU refuses it itself
			}
			i++
			v := out[i]
			if strings.HasPrefix(v, "-") {
				// An option where its value goes: QEMU may read the
				// arguments differently.
				return nil, msg.QEMURefused()
			}
			if strings.Contains(v, "id=") { // not a network otherwise: -netdev help
				netdevs++
			}
			if !done && strings.HasPrefix(v, "user,id=net0,") && !strings.Contains(v, "restrict=") {
				v += ",restrict=on"
				if guestfwd != "" {
					v += ",guestfwd=tcp:" + net.JoinHostPort(guestFwdHost, strconv.Itoa(guestFwdPort)) + "-cmd:" + strings.ReplaceAll(guestfwd, ",", ",,")
				}
				out[i] = v
				done = true
			}
		case "nic", "net":
			if i+1 >= len(out) || out[i+1] != "none" {
				return nil, msg.QEMURefused()
			}
			i++
		case "readconfig":
			return nil, msg.QEMURefused()
		}
	}
	if netdevs > 0 && (!done || netdevs > 1) {
		return nil, msg.QEMURefused()
	}
	return out, nil
}

// option returns the name of a QEMU option, which takes one dash or two.
func option(a string) (string, bool) {
	if !strings.HasPrefix(a, "-") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), true
}

// RunQEMU is devvm _qemu exec, devvm being self: it runs the real QEMU
// for the wrapper at wrapper (its name, on PATH, skipping the wrapper's
// own folder and relative entries, which name whatever folder Lima runs
// in), with args restricted unless NetEnv says open, in devvm's place, or,
// with pins (PinsEnv), in a namespace that pins them (runPinned).
func RunQEMU(self, wrapper string, args []string) error {
	out := args
	if os.Getenv(NetEnv) != netOpen {
		var err error
		if out, err = RestrictNetdev(args, os.Getenv(GuestFwdEnv)); err != nil {
			return err
		}
	}
	name := filepath.Base(wrapper)
	qemu, err := findQEMU(name, filepath.Dir(wrapper), os.Getenv("PATH"))
	if err != nil {
		return err
	}
	if os.Getenv(PinsEnv) != "" {
		return runPinned(self, qemu, out)
	}
	return platform.Exec(qemu, append([]string{qemu}, out...), os.Environ())
}

// findQEMU finds name on path, outside skip.
func findQEMU(name, skip, path string) (string, error) {
	skipReal := realDir(skip)
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) || realDir(d) == skipReal {
			continue
		}
		p := filepath.Join(d, name)
		fi, err := os.Stat(p) //nolint:gosec // searching PATH is the point
		if err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", qemuMissing(name)
}

// realDir is a folder on PATH as an absolute physical path
// (paths.Physical), so two names for one folder compare equal.
func realDir(d string) string {
	if a, err := filepath.Abs(d); err == nil {
		d = a
	}
	return paths.Physical(d)
}

// connectMax is how many of a VM's connections to its proxy devvm _qemu
// connect carries at once, a process each, which measured about 7.7 MB of
// memory: about 2 GB for a VM that holds every slot. As many as the port
// forwarder relays, and many times a package manager's (npm's 15 sockets,
// uv's 50 downloads), so that builds and installs fit while a VM that
// opens connections without end can't fill the host with processes. The
// proxy bounds what each carries.
const connectMax = 256

// connectsFullReply is what a connection past connectMax gets before it
// closes, as the proxy answers a request past one of its limits, so a
// client shows why rather than a closed connection.
func connectsFullReply() string {
	body := "devvm proxy: " + strconv.Itoa(connectMax) + " connections to the proxy at once already; try again\n"
	return "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// connectFullNote is how long a refusal past connectMax keeps the next
// ones quiet, so a VM that keeps trying can't fill QEMU's log.
const connectFullNote = time.Minute

// Connect is devvm _qemu connect: it joins in and out to a TCP connection
// to addr. Each direction ends on its own (a client may finish sending
// before the reply), and it returns once the connection has nothing more
// to say. With slots, a folder (paths.VM.Proxy's .slots), it holds one of its
// connectMax slots while it runs, and refuses the connection when none is
// free: it writes connectsFullReply to out and returns QEMUConnectsFull,
// or nil when that was said within connectFullNote (connectsFull). A VM
// started before the slots has none, and no limit until its next start.
func Connect(addr, slots string, in io.Reader, out io.Writer) error {
	if slots != "" {
		release, ok, err := takeSlot(slots)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = io.WriteString(out, connectsFullReply())
			return connectsFull(slots, time.Now())
		}
		defer release()
	}
	c, err := net.Dial("tcp", addr) //nolint:noctx // runs for the whole connection
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	go func() {
		_, _ = io.Copy(c, in)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	_, err = io.Copy(out, c)
	return err
}

// takeSlot holds one of dir's connectMax slots, a lock file each, until
// release or the process's end: ok is false while every one is held. It
// starts at a slot of its own (its pid's), so that many at once don't all
// try the same ones first.
func takeSlot(dir string) (release func(), ok bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	start := os.Getpid() % connectMax
	for i := range connectMax {
		unlock, got, err := platform.TryLockFile(filepath.Join(dir, strconv.Itoa((start+i)%connectMax)))
		if err != nil {
			return nil, false, err
		}
		if got {
			return unlock, true, nil
		}
	}
	return nil, false, nil
}

// connectsFull is a refusal past connectMax, at now: QEMUConnectsFull,
// unless one was said within connectFullNote (dir's full file, touched
// each time), then nil. One refusal at a time decides, holding dir's
// full.lock, and one that finds it held is quiet, so refusals at once
// say it once.
func connectsFull(dir string, now time.Time) error {
	unlock, got, err := platform.TryLockFile(filepath.Join(dir, "full.lock"))
	if err != nil || !got {
		return nil
	}
	defer unlock()
	marker := filepath.Join(dir, "full")
	if fi, err := os.Stat(marker); err == nil && now.Sub(fi.ModTime()) < connectFullNote {
		return nil
	}
	if err := state.WriteFileMode(marker, nil, 0o600); err == nil {
		_ = os.Chtimes(marker, now, now)
	}
	return msg.QEMUConnectsFull(strconv.Itoa(connectMax))
}
