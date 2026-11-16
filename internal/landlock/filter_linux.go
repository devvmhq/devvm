package landlock

// The child's seccomp filter, a classic BPF program written out by hand, as the kernel reads it:
// each instruction a struct sock_filter, run on each system call's struct seccomp_data. Its tests
// run it for amd64 and arm64, whatever the machine's architecture; seccomp_linux.go installs it.

import (
	"encoding/binary"
	"fmt"
)

// The BPF opcodes the filter uses (linux/bpf_common.h).
const (
	bpfLD   = 0x00
	bpfW    = 0x00
	bpfABS  = 0x20
	bpfJMP  = 0x05
	bpfJEQ  = 0x10
	bpfJGE  = 0x30
	bpfJSET = 0x40
	bpfK    = 0x00
	bpfRET  = 0x06

	ldAbs = bpfLD | bpfW | bpfABS
	jeqK  = bpfJMP | bpfJEQ | bpfK
	jgeK  = bpfJMP | bpfJGE | bpfK
	jsetK = bpfJMP | bpfJSET | bpfK
	retK  = bpfRET | bpfK
)

// What the filter returns (linux/seccomp.h): the call goes on, fails with an errno, or the whole
// process is killed.
const (
	retAllow       = 0x7fff0000
	retErrno       = 0x00050000
	retKillProcess = 0x80000000
	errnoRefused   = 1  // EPERM
	errnoNoSys     = 38 // ENOSYS: as a kernel without the call says, so a caller falls back
)

// Where struct seccomp_data keeps a call's number, its architecture and its arguments' 32-bit
// halves, on a little-endian machine, which amd64 and arm64 are.
const (
	dataNr     = 0
	dataArch   = 4
	dataArg0   = 16
	dataArg0Hi = 20
	dataArg1   = 24
	dataArg1Hi = 28
	dataArg2   = 32
	dataArg3   = 40
	x32Bit     = 0x40000000 // __X32_SYSCALL_BIT: amd64's x32 calls
	insnBytes  = 8          // a struct sock_filter
	// afNetlink and netlinkRoute are the one socket the child makes: a netlink route socket,
	// which reads the host's addresses and reaches the kernel alone.
	afNetlink    = 16
	netlinkRoute = 0
	// msgFastOpen is MSG_FASTOPEN, which connects a TCP socket as it sends.
	msgFastOpen = 0x20000000
	// cloneThread is CLONE_THREAD, which every clone of Go's runtime has: a thread, never a process.
	cloneThread = 0x00010000
	// cloneNamespaces are clone's flags for a new namespace: CLONE_NEWTIME, NEWNS, NEWCGROUP,
	// NEWUTS, NEWIPC, NEWUSER, NEWPID and NEWNET.
	cloneNamespaces = 0x00000080 | 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
)

// arch is what the filter needs of an architecture: the audit number its calls carry, whether it
// has x32's numbers, and the numbers of the calls the filter refuses or looks into.
type arch struct {
	audit uint32
	x32   bool
	// socket is refused for every socket but a netlink route socket (afNetlink, netlinkRoute).
	socket uint32
	// flags3 are the calls refused with MSG_FASTOPEN in their fourth argument, sendto and
	// sendmmsg, and flags2 the one with it in its third, sendmsg.
	flags3 []uint32
	flags2 uint32
	// refused are the calls refused with EPERM whatever their arguments (refusedCalls).
	refused []uint32
	// noSys are the calls refused with ENOSYS: clone3, whose flags sit behind a pointer the filter
	// can't read, so a caller falls back to clone, whose flags it can.
	noSys []uint32
	// clone is refused for a new namespace or anything but a thread; utimensat for a path, so a
	// file's times change only through a descriptor the child opened; prlimit64 for any process
	// but the child (a pid of 0), as a limit set on another, such as RLIMIT_CPU, can kill it.
	clone, utimensat, prlimit uint32
	// seccomp and prctl are the calls that install the filter and set no_new_privs.
	seccomp, prctl uint32
}

// The numbers that are the same on amd64 and arm64.
const (
	sysIOURingSetup    = 425
	sysIOURingEnter    = 426
	sysIOURingRegister = 427
	sysClone3          = 435
	sysPidfdGetfd      = 438
	sysFchmodat2       = 452
	sysSetxattrat      = 463
	sysRemovexattrat   = 466
)

// archOf is the architecture goarch names, as the filter needs it; false: one the child doesn't
// confine itself on. Its refused calls are, in order:
//
//   - socketpair, connect and listen, as the child makes no connection of its own (its broker
//     opens each), and the three io_uring calls, whose operations wouldn't pass the filter;
//   - those that reach another process's memory or descriptors: ptrace, process_vm_readv,
//     process_vm_writev, pidfd_getfd;
//   - those that change a file by its path, which Landlock doesn't restrict: its mode (chmod,
//     fchmodat, fchmodat2), its owner (chown, lchown, fchownat), its extended attributes (setxattr,
//     lsetxattr, removexattr, lremovexattr, setxattrat, removexattrat), its length (truncate) and
//     its times (utime, utimes, futimesat); fchmod, fchown and ftruncate, on a descriptor the
//     child opened, which Landlock checked, stay;
//   - the kernel's keyrings: add_key, request_key, keyctl;
//   - what changes how another process runs, which Landlock's scope of signals doesn't cover:
//     setpriority, ioprio_set, sched_setaffinity, sched_setscheduler, sched_setparam,
//     sched_setattr (and prlimit64 but on the child itself, below);
//   - what tells of files outside its folders as they change: inotify_init, inotify_init1,
//     inotify_add_watch, fanotify_init, fanotify_mark;
//   - what the child never does: running a program (execve, execveat), a process of its own (fork
//     and vfork on amd64), a namespace (unshare, setns), BPF programs (bpf), userfaultfd and
//     perf_event_open.
//
// socketcall, the 32-bit calls' way to every socket call, is no call of a 64-bit architecture: the
// check of the architecture kills a 32-bit one. arm64 has no chmod, chown, lchown, utime, utimes,
// futimesat, fork or vfork.
func archOf(goarch string) (arch, bool) {
	switch goarch {
	case "amd64":
		return arch{
			audit: 0xc000003e, x32: true, // AUDIT_ARCH_X86_64
			socket: 41, flags3: []uint32{44, 307}, flags2: 46, // socket; sendto, sendmmsg; sendmsg
			refused: []uint32{
				53, 42, 50, sysIOURingSetup, sysIOURingEnter, sysIOURingRegister,
				101, 310, 311, sysPidfdGetfd,
				90, 268, sysFchmodat2, 92, 94, 260, 188, 189, 197, 198, sysSetxattrat, sysRemovexattrat, 76, 132, 235, 261,
				248, 249, 250,
				141, 251, 203, 144, 142, 314,
				253, 294, 254, 300, 301,
				59, 322, 57, 58, 272, 308, 321, 323, 298,
			},
			noSys: []uint32{sysClone3}, clone: 56, utimensat: 280, prlimit: 302,
			seccomp: 317, prctl: 157,
		}, true
	case "arm64":
		return arch{
			audit:  0xc00000b7,                                   // AUDIT_ARCH_AARCH64
			socket: 198, flags3: []uint32{206, 269}, flags2: 211, // socket; sendto, sendmmsg; sendmsg
			refused: []uint32{
				199, 203, 201, sysIOURingSetup, sysIOURingEnter, sysIOURingRegister,
				117, 270, 271, sysPidfdGetfd,
				53, sysFchmodat2, 54, 5, 6, 14, 15, sysSetxattrat, sysRemovexattrat, 45,
				217, 218, 219,
				140, 30, 122, 119, 118, 274,
				26, 27, 262, 263,
				221, 281, 97, 268, 280, 282, 241,
			},
			noSys: []uint32{sysClone3}, clone: 220, utimensat: 88, prlimit: 261,
			seccomp: 277, prctl: 167,
		}, true
	}
	return arch{}, false
}

// insn is one instruction: struct sock_filter.
type insn struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

// asm writes a program whose jumps name labels, which build counts out: a jump goes only forward,
// to an instruction at most 255 past the next.
type asm struct {
	p      []insn
	jumps  map[int][2]string // an instruction's jt and jf labels; "": the next instruction
	labels map[string]int
}

func newAsm() *asm { return &asm{jumps: map[int][2]string{}, labels: map[string]int{}} }

// op is an instruction that doesn't jump.
func (a *asm) op(code uint16, k uint32) { a.p = append(a.p, insn{code: code, k: k}) }

// jump is a conditional jump to jt when it holds, jf when it doesn't.
func (a *asm) jump(code uint16, k uint32, jt, jf string) {
	a.jumps[len(a.p)] = [2]string{jt, jf}
	a.p = append(a.p, insn{code: code, k: k})
}

// label names the next instruction.
func (a *asm) label(name string) { a.labels[name] = len(a.p) }

// build is the program, its jumps counted out.
func (a *asm) build() ([]insn, error) {
	for i, j := range a.jumps {
		for side, name := range j {
			if name == "" {
				continue
			}
			to, ok := a.labels[name]
			if !ok || to <= i || to-i-1 > 255 {
				return nil, fmt.Errorf("instruction %d's jump to %q", i, name)
			}
			if side == 0 {
				a.p[i].jt = uint8(to - i - 1) //nolint:gosec // checked: 0 to 255
			} else {
				a.p[i].jf = uint8(to - i - 1) //nolint:gosec // checked: 0 to 255
			}
		}
	}
	return a.p, nil
}

// filter is a's program:
//
//	load the architecture; another than a's: kill
//	load the call's number; on amd64, one of x32's: kill
//	one of a's refused calls: EPERM; clone3: ENOSYS
//	socket: load the family and the protocol; anything but a netlink route socket: EPERM
//	sendto, sendmmsg, sendmsg: load the flags; MSG_FASTOPEN: EPERM
//	clone: load the flags; a new namespace, or not a thread: EPERM
//	utimensat: load the path; not NULL: EPERM
//	prlimit64: load the pid; not 0: EPERM
//	anything else: allow
func filter(a arch) ([]insn, error) {
	p := newAsm()
	p.op(ldAbs, dataArch)
	p.jump(jeqK, a.audit, "", "kill")
	p.op(ldAbs, dataNr)
	if a.x32 {
		p.jump(jgeK, x32Bit, "kill", "")
	}
	for _, nr := range a.refused {
		p.jump(jeqK, nr, "refuse", "")
	}
	for _, nr := range a.noSys {
		p.jump(jeqK, nr, "nosys", "")
	}
	p.jump(jeqK, a.socket, "socket", "")
	for _, nr := range a.flags3 {
		p.jump(jeqK, nr, "flags3", "")
	}
	p.jump(jeqK, a.flags2, "flags2", "")
	p.jump(jeqK, a.clone, "clone", "")
	p.jump(jeqK, a.utimensat, "utimensat", "")
	p.jump(jeqK, a.prlimit, "prlimit", "")
	p.op(retK, retAllow)
	// Each look into a call's arguments ends in an allow of its own, as a jump only goes forward.
	// The kernel reads socket's family and protocol and the send calls' flags as ints, their
	// arguments' low halves.
	p.label("socket")
	p.op(ldAbs, dataArg0)
	p.jump(jeqK, afNetlink, "", "refuse")
	p.op(ldAbs, dataArg2)
	p.jump(jeqK, netlinkRoute, "", "refuse")
	p.op(retK, retAllow)
	p.label("flags3")
	p.op(ldAbs, dataArg3)
	p.jump(jsetK, msgFastOpen, "refuse", "")
	p.op(retK, retAllow)
	p.label("flags2")
	p.op(ldAbs, dataArg2)
	p.jump(jsetK, msgFastOpen, "refuse", "")
	p.op(retK, retAllow)
	p.label("clone")
	p.op(ldAbs, dataArg0)
	p.jump(jsetK, cloneNamespaces, "refuse", "")
	p.jump(jsetK, cloneThread, "", "refuse")
	p.op(retK, retAllow)
	p.label("utimensat")
	p.op(ldAbs, dataArg1)
	p.jump(jeqK, 0, "", "refuse")
	p.op(ldAbs, dataArg1Hi)
	p.jump(jeqK, 0, "", "refuse")
	p.op(retK, retAllow)
	p.label("prlimit")
	p.op(ldAbs, dataArg0)
	p.jump(jeqK, 0, "", "refuse")
	p.op(ldAbs, dataArg0Hi)
	p.jump(jeqK, 0, "", "refuse")
	p.op(retK, retAllow)
	p.label("refuse")
	p.op(retK, retErrno|errnoRefused)
	p.label("nosys")
	p.op(retK, retErrno|errnoNoSys)
	p.label("kill")
	p.op(retK, retKillProcess)
	return p.build()
}

// encode is p as the kernel reads it: each struct sock_filter, in this machine's byte order,
// which on amd64 and arm64 is little-endian.
func encode(p []insn) []byte {
	b := make([]byte, 0, len(p)*insnBytes)
	for _, in := range p {
		b = binary.LittleEndian.AppendUint16(b, in.code)
		b = append(b, in.jt, in.jf)
		b = binary.LittleEndian.AppendUint32(b, in.k)
	}
	return b
}
