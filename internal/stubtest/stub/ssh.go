package main

import (
	"strings"
	"syscall"
	"time"
)

// masterLife is how long a master connection waits to be stopped.
const masterLife = 300 * time.Second

// listenOnce listens on the unix socket path with a backlog of one and
// never accepts: the socket takes the first connection or two, which
// wait, and refuses the rest, as no master answers on it. devvm's check
// for a killed session's leftover socket (vm.LeftoverSockets) then finds
// it stale once something has tried it.
func listenOnce(path string) (int, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	if err := syscall.Listen(fd, 1); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

// ssh stands in for OpenSSH's ssh, reaching the guest (guest.go). It logs
// every call to $STUB_LOG. A master connection (-M, a VM's port
// forwarder's) reads the script it's fed, answers it with
// $STUB_DIR/guest-ports and ".", again whenever that file changes, until
// it's stopped. A link
// session's master (-M -N -S CONTROL, devvm _links's) listens on its
// control socket until it's stopped, and its -O forward and -O cancel
// work, but for a forward whose ADDRESS:PORT is a line of
// $STUB_DIR/link-taken, which the VM refuses. Either master outlives the
// process that started it, as ssh's does, for 5 minutes (TestScript
// stops its sandbox's). Host git fetching from a --clone VM, or a test's
// own git push to it, runs its git-upload-pack or git-receive-pack on the
// guest's files.
// With $STUB_DIR/ssh-fails, or for anything else, it fails as ssh does
// (255).
func ssh(c *call) int {
	c.log()
	if c.on("ssh-fails") {
		return 255
	}
	g := &guest{call: c}
	switch {
	case c.has("-M") && c.has("-N"):
		fd, err := listenOnce(c.after("-S"))
		if err != nil {
			c.say("ssh stub: " + err.Error())
			return 255
		}
		defer func() { _ = syscall.Close(fd) }()
		c.sleep(masterLife)
		return 0
	case c.has("-O"):
		addr, rest, _ := strings.Cut(c.after("-R"), ":")
		port, _, _ := strings.Cut(rest, ":")
		taken, _ := c.text("link-taken")
		for line := range strings.Lines(taken) {
			if strings.TrimSuffix(line, "\n") == addr+":"+port {
				c.say("Error: remote port forwarding failed for listen port " + port)
				return 255
			}
		}
		return 0
	case c.has("-M"):
		return g.ports()
	}
	if cmd := c.last(); strings.HasPrefix(cmd, "git-receive-pack ") || strings.HasPrefix(cmd, "git-upload-pack ") ||
		strings.HasPrefix(cmd, "git -c ") && (strings.Contains(cmd, " upload-pack ") || strings.Contains(cmd, " receive-pack ")) {
		return g.pack(cmd)
	}
	return 255
}
