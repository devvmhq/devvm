package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
)

// Each sandbox has ports of its own: a block of sandboxBlock ports from
// sandboxPortsLo up to sandboxPortsHi, below both the range devvm
// chooses from (platform.ChoosablePorts, from 20000) and the system's
// ephemeral range, so neither a devvm the tests run nor an outgoing
// connection takes one. A sandbox holds its block's lock file in
// sandboxPortsDir for as long as it lasts, so no other sandbox, in this
// test process or another running at once, has the same block; while
// every block is held, it waits for one, up to sandboxPortsWait. Its VMs'
// ssh ports and every port a script asks for (free_port, the fixtures'
// ports) come from the block's first sandboxTaken, in turn
// (SANDBOX_PORT_NEXT up to SANDBOX_PORT_END), each one free on every
// address as it's taken; its devvm chooses the VMs' other ports from the
// rest (DEVVM_PORT_RANGE), in place of the range every devvm chooses
// from, so two sandboxes' devvms never choose the same one.
const (
	sandboxPortsLo   = 10000
	sandboxPortsHi   = 19999
	sandboxBlock     = 160
	sandboxTaken     = 60
	sandboxPortsWait = 5 * time.Minute
)

// sandboxPortsDir is the folder of the blocks' lock files: this user's
// own, in /tmp, as every test process of the user's must find it.
func sandboxPortsDir() string {
	return "/tmp/devvm-test-ports-" + strconv.Itoa(os.Getuid())
}

// portsDirOwned makes dir, or checks the one there: a folder, not a
// symlink, of this user's alone, so no other user's process can hold
// its blocks' locks or swap the folder.
func portsDirOwned(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if uid, _, ok := platform.Owner(fi); !fi.IsDir() || !ok || uid != uint32(os.Getuid()) || fi.Mode().Perm()&0o077 != 0 { //nolint:gosec // a uid fits
		return fmt.Errorf("%s isn't a folder of this user's alone", dir)
	}
	return nil
}

// claimPortBlock claims a block of ports in dir no other sandbox holds,
// returning its first port and the release of its lock. While every
// block is held, it tries again until wait has passed.
func claimPortBlock(dir string, wait time.Duration) (first int, release func(), err error) {
	if err := portsDirOwned(dir); err != nil {
		return 0, nil, err
	}
	n := (sandboxPortsHi - sandboxPortsLo + 1) / sandboxBlock
	for deadline := time.Now().Add(wait); ; {
		start := rand.IntN(n) //nolint:gosec // a block to start from, not a secret
		for i := range n {
			k := (start + i) % n
			unlock, ok, err := platform.TryLockFile(filepath.Join(dir, strconv.Itoa(k)+".lock"))
			if err != nil {
				return 0, nil, err
			}
			if ok {
				return sandboxPortsLo + k*sandboxBlock, unlock, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, nil, errors.New("every sandbox's block of ports is held")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// takePorts returns n ports from next up to end (not included), each
// free now on every address, in a row when inARow, and the port to take
// from after them.
func takePorts(next, end, n int, inARow bool) (ports []int, after int, err error) {
	for p := next; p < end; p++ {
		if !freeHere(p) {
			if inARow {
				ports = ports[:0]
			}
			continue
		}
		if ports = append(ports, p); len(ports) == n {
			return ports, p + 1, nil
		}
	}
	return nil, end, errors.New("the sandbox's block of ports ran out")
}

// freeHere reports whether nothing listens on port, on any address.
// On macOS a listen on every address succeeds while a program has the
// port on 127.0.0.1 alone, so devvm's own check (ports.Listening) has the
// last word.
func freeHere(port int) bool {
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port)) //nolint:noctx // every address, so the port is free on each; closed at once
	if err != nil {
		return false
	}
	_ = l.Close()
	return !ports.Listening(context.Background(), port) //nolint:forbidigo // a sandbox's setup, before any script's context
}

// Sandboxes' ports: each sandbox's block is its own while it holds it,
// another process's too, and lies below both the range devvm chooses
// from and the system's ephemeral range; ports are taken from it in turn,
// skipping one a program here listens on, and in a row only when asked.
func TestSandboxPorts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ports")
	a, releaseA, err := claimPortBlock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the blocks' folder: %v, %v", fi, err)
	}
	b, releaseB, err := claimPortBlock(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	if a == b || a < sandboxPortsLo || b+sandboxBlock-1 > sandboxPortsHi || (a-sandboxPortsLo)%sandboxBlock != 0 {
		t.Errorf("blocks %d and %d", a, b)
	}
	if sandboxPortsHi >= platform.ChoosablePorts().Lo {
		t.Errorf("sandboxes' ports reach devvm's, from %d", platform.ChoosablePorts().Lo)
	}
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil { //nolint:noctx // the system's choice, from its ephemeral range
		if l.Addr().(*net.TCPAddr).Port <= sandboxPortsHi {
			t.Errorf("an ephemeral port in the sandboxes' range: %s", l.Addr())
		}
		_ = l.Close()
	}
	// Every block held, by this process or another: none left.
	var rest []func()
	for {
		_, r, err := claimPortBlock(dir, 0)
		if err != nil {
			break
		}
		rest = append(rest, r)
	}
	if len(rest) != (sandboxPortsHi-sandboxPortsLo+1)/sandboxBlock-2 {
		t.Errorf("%d more blocks", len(rest))
	}
	// A claim waits for a block given up meanwhile.
	go func() { time.Sleep(300 * time.Millisecond); releaseA() }()
	if again, r, err := claimPortBlock(dir, 10*time.Second); err != nil || again != a {
		t.Errorf("the block given up: %d, %v", again, err)
	} else {
		r()
	}
	// A folder that's a symlink, or anyone's to write, isn't the blocks'.
	link, open := filepath.Join(t.TempDir(), "link"), filepath.Join(t.TempDir(), "open")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimPortBlock(link, 0); err == nil {
		t.Error("claimed in a symlink")
	}
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil { //nolint:gosec // the test's own folder, made open to be refused
		t.Fatal(err)
	}
	if _, _, err := claimPortBlock(open, 0); err == nil {
		t.Error("claimed in a folder anyone can write")
	}
	for _, r := range rest {
		r()
	}
	// A port taken here is skipped, and breaks a row.
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", ":"+strconv.Itoa(b+1))
	if err != nil {
		t.Skipf("port %d: %v", b+1, err)
	}
	defer func() { _ = held.Close() }()
	if got, after, err := takePorts(b, b+sandboxBlock, 2, false); err != nil || !slices.Equal(got, []int{b, b + 2}) || after != b+3 {
		t.Errorf("two ports: %v, after %d, %v", got, after, err)
	}
	if got, after, err := takePorts(b, b+sandboxBlock, 2, true); err != nil || !slices.Equal(got, []int{b + 2, b + 3}) || after != b+4 {
		t.Errorf("two in a row: %v, after %d, %v", got, after, err)
	}
	if _, after, err := takePorts(b, b+2, 2, false); err == nil || after != b+2 {
		t.Errorf("a block run out: %v", err)
	}
}

// sandboxTake takes n of the script's sandbox's ports, in a row when
// inARow (takePorts), failing the script when its block runs out.
func sandboxTake(ts *testscript.TestScript, n int, inARow bool) []int {
	next, err1 := strconv.Atoi(ts.Getenv("SANDBOX_PORT_NEXT"))
	end, err2 := strconv.Atoi(ts.Getenv("SANDBOX_PORT_END"))
	if err1 != nil || err2 != nil {
		ts.Fatalf("no sandbox ports: SANDBOX_PORT_NEXT %q, SANDBOX_PORT_END %q", ts.Getenv("SANDBOX_PORT_NEXT"), ts.Getenv("SANDBOX_PORT_END"))
	}
	ports, after, err := takePorts(next, end, n, inARow)
	if err != nil {
		ts.Fatalf("%v", err)
	}
	ts.Setenv("SANDBOX_PORT_NEXT", strconv.Itoa(after))
	return ports
}
