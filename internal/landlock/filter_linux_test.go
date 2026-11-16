package landlock

import (
	"encoding/binary"
	"fmt"
	"testing"
)

// call is a system call as the filter sees it: struct seccomp_data's number, architecture and
// arguments.
type call struct {
	nr, arch uint32
	args     [6]uint64
}

// data is c as struct seccomp_data's bytes, little-endian.
func (c call) data() []byte {
	b := binary.LittleEndian.AppendUint32(nil, c.nr)
	b = binary.LittleEndian.AppendUint32(b, c.arch)
	b = binary.LittleEndian.AppendUint64(b, 0) // instruction_pointer
	for _, a := range c.args {
		b = binary.LittleEndian.AppendUint64(b, a)
	}
	return b
}

// run runs prog, a filter's bytes as the kernel reads them, on c, as the kernel's classic BPF
// would for the instructions the filter uses, and returns what it returned. A program that isn't
// whole instructions, uses another instruction, jumps out of itself, or reads past the data fails
// the test.
func run(t *testing.T, prog []byte, c call) uint32 {
	t.Helper()
	if len(prog)%insnBytes != 0 || len(prog) == 0 || len(prog)/insnBytes > 4096 {
		t.Fatalf("a program of %d bytes", len(prog))
	}
	data := c.data()
	var acc uint32
	n := len(prog) / insnBytes
	for pc := 0; pc < n; pc++ {
		b := prog[pc*insnBytes:]
		code, jt, jf, k := binary.LittleEndian.Uint16(b), int(b[2]), int(b[3]), binary.LittleEndian.Uint32(b[4:])
		jump := func(cond bool) {
			d := jf
			if cond {
				d = jt
			}
			if pc+1+d >= n {
				t.Fatalf("instruction %d jumps out of the program", pc)
			}
			pc += d
		}
		switch code {
		case ldAbs:
			if k%4 != 0 || int(k)+4 > len(data) {
				t.Fatalf("instruction %d loads at %d", pc, k)
			}
			acc = binary.LittleEndian.Uint32(data[k:])
		case jeqK:
			jump(acc == k)
		case jgeK:
			jump(acc >= k)
		case jsetK:
			jump(acc&k != 0)
		case retK:
			return k
		default:
			t.Fatalf("instruction %d's code %#x", pc, code)
		}
	}
	t.Fatal("the program ran off its end")
	return 0
}

// mustFilter is a's program, as the kernel reads it.
func mustFilter(t *testing.T, a arch) []byte {
	t.Helper()
	p, err := filter(a)
	if err != nil {
		t.Fatal(err)
	}
	return encode(p)
}

// fcase is one call and what the filter must return for it.
type fcase struct {
	name string
	c    call
	want uint32
}

// The filter, for each architecture devvm ships for: a call of another architecture, or one of
// amd64's x32 numbers, kills the process; socket and socketpair for AF_UNIX, whatever the high
// bits of the family's argument, and every refused call get EPERM, clone3 ENOSYS; clone for a
// new namespace, or for anything but a thread, gets EPERM, as do utimensat with a path and
// prlimit64 on another pid; socket for another family, a thread's clone, utimensat on a
// descriptor (a NULL path), prlimit64 on the child itself (pid 0) and every other call go on.
func TestFilter(t *testing.T) {
	t.Parallel()
	refuse, nosys, allow, kill := uint32(retErrno|errnoRefused), uint32(retErrno|errnoNoSys), uint32(retAllow), uint32(retKillProcess)
	// Go's runtime clones its threads with CLONE_VM|FS|FILES|SIGHAND|SYSVSEM|THREAD.
	const goThread = 0x100 | 0x200 | 0x400 | 0x800 | 0x40000 | cloneThread
	for _, goarch := range []string{"amd64", "arm64"} {
		a, ok := archOf(goarch)
		if !ok {
			t.Fatal(goarch)
		}
		prog := mustFilter(t, a)
		c := func(nr uint32, args ...uint64) call {
			x := call{nr: nr, arch: a.audit}
			copy(x.args[:], args)
			return x
		}
		socket, socketpair := a.family[0], a.family[1]
		cases := []fcase{
			{"read", c(0), allow},
			{"seccomp", c(a.seccomp), allow},
			{"prctl", c(a.prctl), allow},
			{"socket AF_INET", c(socket, 2, 1), allow},
			{"socket AF_INET6", c(socket, 10, 1), allow},
			{"socket AF_NETLINK", c(socket, 16, 3), allow},
			{"socket AF_UNIX", c(socket, afUnix, 1), refuse},
			{"socket AF_UNIX, high bits set", c(socket, 1<<32|afUnix, 1), refuse},
			{"socketpair AF_UNIX", c(socketpair, afUnix, 1), refuse},
			{"socketpair AF_INET", c(socketpair, 2, 1), allow},
			{"clone3", c(sysClone3), nosys},
			{"clone, a Go thread", c(a.clone, goThread), allow},
			{"clone, a thread in a new user namespace", c(a.clone, goThread|0x10000000), refuse},
			{"clone, a thread in a new network namespace", c(a.clone, goThread|0x40000000), refuse},
			{"clone, a new time namespace", c(a.clone, goThread|0x80), refuse},
			{"clone, a process (fork)", c(a.clone, 17), refuse}, // SIGCHLD alone
			{"clone, a process sharing memory (vfork)", c(a.clone, 0x100|0x4000|17), refuse},
			{"utimensat on a descriptor", c(a.utimensat, 3, 0, 0x1234, 0), allow},
			{"utimensat with a path", c(a.utimensat, 0xffffff9c, 0x7fff0000, 0x1234, 0), refuse},
			{"utimensat with a path above 4 GiB", c(a.utimensat, 3, 0x7f0000000000, 0, 0), refuse},
			{"prlimit64 on the child itself", c(a.prlimit, 0, 7, 0x1234, 0x5678), allow},
			{"prlimit64 on another pid", c(a.prlimit, 4242, 0, 0x1234, 0), refuse},
			{"prlimit64 on a pid in the high half", c(a.prlimit, 1<<32, 0, 0x1234, 0), refuse},
			{"another architecture", call{nr: 1, arch: 0x40000003}, kill},                // i386, whose socketcall is 102
			{"another architecture's socketcall", call{nr: 102, arch: 0x40000003}, kill}, // AUDIT_ARCH_I386
			{"32-bit arm", call{nr: 281, arch: 0x40000028}, kill},                        // AUDIT_ARCH_ARM's socket
			{"no architecture", call{nr: 0}, kill},
		}
		for _, nr := range a.refused {
			cases = append(cases, fcase{fmt.Sprintf("refused %d", nr), c(nr), refuse})
		}
		// The descriptor's calls that the child's logs and the Nix cache use stay.
		// fchmod, fchown, ftruncate, fcntl, flock, and Go runtime's sched_getaffinity.
		kept := map[string][]uint32{"amd64": {91, 93, 77, 72, 73, 204}, "arm64": {52, 55, 46, 25, 32, 123}}
		for _, nr := range kept[goarch] {
			cases = append(cases, fcase{fmt.Sprintf("kept %d", nr), c(nr), allow})
		}
		if a.x32 {
			cases = append(cases,
				fcase{"x32 socket", c(x32Bit|socket, afUnix), kill},
				fcase{"x32 read", c(x32Bit), kill},
			)
		}
		for _, tc := range cases {
			if got := run(t, prog, tc.c); got != tc.want {
				t.Errorf("%s, %s: %#x, want %#x", goarch, tc.name, got, tc.want)
			}
		}
	}
	if _, ok := archOf("386"); ok {
		t.Error("a filter for 386")
	}
}

// The program's bytes: amd64's begins with the check of its architecture, whose miss jumps to
// kill at the end, then of x32's numbers, and ends with the look into utimensat's path, refuse,
// ENOSYS and kill. A jump that goes backward, or past 255, is refused as the program's written.
func TestFilterBytes(t *testing.T) {
	t.Parallel()
	a, _ := archOf("amd64")
	b := mustFilter(t, a)
	n := len(b) / insnBytes
	toKill := byte(n - 3) //nolint:gosec // from instruction 1 to the last, n-1: a few dozen
	head := []byte{
		0x20, 0x00, 0, 0, 0x04, 0x00, 0x00, 0x00, // ld [4]: the architecture
		0x15, 0x00, 0, toKill, 0x3e, 0x00, 0x00, 0xc0, // jeq AUDIT_ARCH_X86_64, else kill
		0x20, 0x00, 0, 0, 0x00, 0x00, 0x00, 0x00, // ld [0]: the number
		0x35, 0x00, toKill - 2, 0, 0x00, 0x00, 0x00, 0x40, // jge x32's bit, kill
	}
	tail := []byte{
		0x20, 0x00, 0, 0, 0x10, 0x00, 0x00, 0x00, // ld [16]: prlimit64's pid, low
		0x15, 0x00, 0, 3, 0x00, 0x00, 0x00, 0x00, // jeq 0, else refuse
		0x20, 0x00, 0, 0, 0x14, 0x00, 0x00, 0x00, // ld [20]: its high half
		0x15, 0x00, 0, 1, 0x00, 0x00, 0x00, 0x00, // jeq 0, else refuse
		0x06, 0x00, 0, 0, 0x00, 0x00, 0xff, 0x7f, // ret ALLOW
		0x06, 0x00, 0, 0, 0x01, 0x00, 0x05, 0x00, // ret ERRNO(EPERM)
		0x06, 0x00, 0, 0, 0x26, 0x00, 0x05, 0x00, // ret ERRNO(ENOSYS)
		0x06, 0x00, 0, 0, 0x00, 0x00, 0x00, 0x80, // ret KILL_PROCESS
	}
	checks := len(a.refused) + len(a.noSys) + len(a.family) + 3
	// The head, the checks, allow, the family's 3, clone's 4, utimensat's 5, prlimit64's 5, and
	// the 3 ends.
	if n != 4+checks+1+3+4+5+5+3 {
		t.Fatalf("%d instructions", n)
	}
	if string(b[:len(head)]) != string(head) || string(b[len(b)-len(tail):]) != string(tail) {
		t.Errorf("% x", b)
	}
	bad := newAsm()
	bad.label("back")
	bad.jump(jeqK, 0, "back", "")
	if _, err := bad.build(); err == nil {
		t.Error("a jump backward: built")
	}
	far := newAsm()
	far.jump(jeqK, 0, "far", "")
	for range 256 {
		far.op(retK, retAllow)
	}
	far.label("far")
	far.op(retK, retAllow)
	if _, err := far.build(); err == nil {
		t.Error("a jump past 255: built")
	}
	unknown := newAsm()
	unknown.jump(jeqK, 0, "nowhere", "")
	if _, err := unknown.build(); err == nil {
		t.Error("a jump to no label: built")
	}
}
