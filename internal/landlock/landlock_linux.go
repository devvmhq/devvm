package landlock

// Landlock (linux/landlock.h): the files the child may read and write, the TCP and UDP ports it
// may bind and connect to, which are none, and the scope of its signals and abstract unix sockets.

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Landlock's system calls, the same numbers on amd64 and arm64.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446
)

// Landlock's flags and rule types.
const (
	createRulesetVersion = 1 << 0
	createRulesetErrata  = 1 << 1
	ruleTypePathBeneath  = 1
	restrictSelfTSYNC    = 1 << 3 // ABI 8: every thread at once, in one domain
	// erratumSignals is the fix that lets a process's threads signal each other whatever their
	// domain (erratum 2), which a domain made on each thread in turn needs to scope signals.
	erratumSignals = 1 << 1
	abiTSYNC       = 8
	abiUDP         = 10
)

// Landlock's access rights.
const (
	fsWriteFile   = 1 << 1
	fsReadFile    = 1 << 2
	fsReadDir     = 1 << 3
	fsRemoveFile  = 1 << 5
	fsMakeReg     = 1 << 8
	fsAllABI1     = 1<<13 - 1 // EXECUTE through MAKE_SYM
	fsRefer       = 1 << 13   // ABI 2
	fsTruncate    = 1 << 14   // ABI 3
	fsIoctlDev    = 1 << 15   // ABI 5
	fsResolveUnix = 1 << 16   // ABI 9

	netBindTCP        = 1 << 0 // ABI 4
	netConnectTCP     = 1 << 1 // ABI 4
	netBindUDP        = 1 << 2 // ABI 10
	netConnectSendUDP = 1 << 3 // ABI 10

	scopeAbstractUnix = 1 << 0 // ABI 6
	scopeSignal       = 1 << 1 // ABI 6
)

// oPath is open's O_PATH, on amd64 and arm64, which the syscall package doesn't have: a folder
// opened for a rule alone.
const oPath = 0x200000

// landlockABI is the kernel's Landlock ABI: 0 when it has none, or has it turned off.
func landlockABI() int {
	r, _, e := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if e != 0 {
		return 0
	}
	return int(r)
}

// signalsScoped reports whether the child scopes its signals at abi: from ABI 6, when the kernel
// confines every thread at once (TSYNC) or lets a process's threads signal each other across the
// domains each made in turn. Without that, the Go runtime's own signals between its threads
// would be refused, the one that makes each thread restrict itself among them, and the start
// would hang.
func signalsScoped(abi int) bool {
	if abi < ABISignals {
		return false
	}
	if abi >= abiTSYNC {
		return true
	}
	r, _, e := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetErrata)
	return e == 0 && r&erratumSignals != 0
}

// handledFS are the file rights Landlock at abi knows, all of which the child's ruleset handles,
// so it has none of them but where a rule gives them.
func handledFS(abi int) uint64 {
	r := uint64(fsAllABI1)
	if abi >= 2 {
		r |= fsRefer
	}
	if abi >= 3 {
		r |= fsTruncate
	}
	if abi >= 5 {
		r |= fsIoctlDev
	}
	if abi >= ABIUnix {
		r |= fsResolveUnix
	}
	return r
}

// folderRights are what the child may do in its folders: read, write, list, make and delete
// regular files, rename them between its folders (REFER) and empty them (TRUNCATE), as far as abi
// handles them. No folders, links, FIFOs, sockets or devices, and nothing runs.
func folderRights(abi int) uint64 {
	return (fsReadFile | fsWriteFile | fsReadDir | fsRemoveFile | fsMakeReg | fsRefer | fsTruncate) & handledFS(abi)
}

// rulesetAttr is struct landlock_ruleset_attr's first three fields: a kernel that knows fewer
// takes the rest as zero.
type rulesetAttr struct {
	handledFS, handledNet, scoped uint64
}

// landlock confines every thread of this process with Landlock at abi to folders, binding and
// connecting no TCP port (ABI 4), binding and sending no UDP (ABI 10), as the broker opens its
// connections, and, as scope says, its signals and abstract unix sockets to itself (ABI 6). Every
// thread already has no_new_privs.
func landlock(abi int, folders []string, scope bool) error {
	attr := rulesetAttr{handledFS: handledFS(abi)}
	if abi >= ABIPorts {
		attr.handledNet = netBindTCP | netConnectTCP
	}
	if abi >= abiUDP {
		attr.handledNet |= netBindUDP | netConnectSendUDP
	}
	if abi >= ABISignals {
		attr.scoped = scopeAbstractUnix
		if scope {
			attr.scoped |= scopeSignal
		}
	}
	r, _, e := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) //nolint:gosec // the kernel reads the struct it's given, its size alongside
	if e != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	ruleset := int(r)
	defer func() { _ = syscall.Close(ruleset) }()
	for _, dir := range folders {
		if err := allowBeneath(ruleset, dir, folderRights(abi)); err != nil {
			return err
		}
	}
	if abi >= abiTSYNC {
		_, _, e = syscall.Syscall(sysLandlockRestrictSelf, uintptr(ruleset), restrictSelfTSYNC, 0)
	} else {
		_, _, e = syscall.AllThreadsSyscall(sysLandlockRestrictSelf, uintptr(ruleset), 0, 0)
	}
	if e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}

// allowBeneath gives the ruleset rights on dir and everything below it.
func allowBeneath(ruleset int, dir string, rights uint64) error {
	fd, err := syscall.Open(dir, oPath|syscall.O_CLOEXEC|syscall.O_DIRECTORY, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer func() { _ = syscall.Close(fd) }()
	// struct landlock_path_beneath_attr, packed: allowed_access, then parent_fd.
	var attr [12]byte
	binary.NativeEndian.PutUint64(attr[0:], rights)
	binary.NativeEndian.PutUint32(attr[8:], uint32(fd))                                                                                          //nolint:gosec // a descriptor, which is never negative once open
	if _, _, e := syscall.Syscall6(sysLandlockAddRule, uintptr(ruleset), ruleTypePathBeneath, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); e != 0 { //nolint:gosec // the kernel reads the packed struct's 12 bytes
		return fmt.Errorf("landlock_add_rule %s: %w", dir, e)
	}
	return nil
}
