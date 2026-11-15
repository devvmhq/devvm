package platform

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A Linux host pins paths for a VM in QEMU's own user and mount
// namespace: each is bound over itself there, read-only or not, so the
// guest, which reaches them through QEMU, can't change it, or rename or
// remove it (a mount point is busy), and nothing of it persists once QEMU
// is gone. Each path is resolved once, by the kernel, beneath the folder
// it's in, through no symlink, and the bind acts on what that found, never
// on a path again: mount(2) would follow a symlink the guest planted.

// HasPins reports whether this system can pin paths (PinBeneath): Linux
// can, in a user namespace, where it allows one.
const HasPins = true

// InUserNamespace makes cmd start in a user and mount namespace of its
// own, as the same user and group (so files keep their owner), with
// CAP_SYS_ADMIN there to pin, and killed if devvm's thread that starts it
// ends: the caller keeps it (runtime.LockOSThread). Where AppArmor
// restricts user namespaces (Ubuntu), the namespace comes without the
// capability, and the pins fail.
func InUserNamespace(cmd *exec.Cmd) error {
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                []uintptr{unix.CAP_SYS_ADMIN},
		Pdeathsig:                  syscall.SIGKILL,
	}
	return nil
}

// PrivateMounts makes every mount in this mount namespace private, so a
// pin made here never reaches the host's.
func PrivateMounts() error {
	return unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}

// OpenRoot opens the folder that pins are beneath, by its path, which has
// no symlink in it (a physical path).
func OpenRoot(path string) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openBeneath opens rel beneath root as a path (O_PATH): never through a
// symlink, rel itself included, nor out of root.
func openBeneath(root *os.File, rel string) (*os.File, error) {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return nil, &os.PathError{Op: "open", Path: rel, Err: unix.EINVAL}
	}
	fd, err := unix.Openat2(int(root.Fd()), rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// linked reports whether the file f opens, a regular one, has another
// name, or a folder has a regular file that has (a hooks folder's
// hooks).
func linked(f *os.File, st *unix.Stat_t) bool {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return st.Nlink > 1
	case unix.S_IFDIR:
		d, err := unix.Openat(int(f.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return false
		}
		dir := os.NewFile(uintptr(d), f.Name())
		defer func() { _ = dir.Close() }()
		names, _ := dir.Readdirnames(-1)
		for _, n := range names {
			var es unix.Stat_t
			if unix.Fstatat(d, n, &es, unix.AT_SYMLINK_NOFOLLOW) == nil && es.Mode&unix.S_IFMT == unix.S_IFREG && es.Nlink > 1 {
				return true
			}
		}
	}
	return false
}

// PinBeneath binds rel, beneath root, over itself, read-only when
// readOnly: a folder or a file, never a symlink or through one, nor a
// file with another name (ErrLinked), or a folder holding one. It needs
// CAP_SYS_ADMIN in the mount namespace's user namespace.
func PinBeneath(root *os.File, rel string, readOnly bool) error {
	target, err := openBeneath(root, rel)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(target.Fd()), &st); err != nil {
		return &os.PathError{Op: "stat", Path: rel, Err: err}
	}
	if m := st.Mode & unix.S_IFMT; m != unix.S_IFDIR && m != unix.S_IFREG {
		return &os.PathError{Op: "pin", Path: rel, Err: unix.EINVAL}
	}
	if readOnly && linked(target, &st) {
		return &os.PathError{Op: "pin", Path: rel, Err: ErrLinked}
	}
	tree, err := unix.OpenTree(int(target.Fd()), "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return &os.PathError{Op: "open_tree", Path: rel, Err: err}
	}
	defer func() { _ = unix.Close(tree) }()
	if readOnly {
		if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
			return &os.PathError{Op: "mount_setattr", Path: rel, Err: err}
		}
	}
	if err := unix.MoveMount(tree, "", int(target.Fd()), "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return &os.PathError{Op: "move_mount", Path: rel, Err: err}
	}
	return nil
}

// Pinned reports whether rel, beneath root, is pinned as PinBeneath pins
// it: a mount's root, read-only when readOnly, and then with no other name
// (ErrLinked). A path that's gone, or that a symlink now stands for, isn't.
func Pinned(root *os.File, rel string, readOnly bool) bool {
	f, err := openBeneath(root, rel)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var sx unix.Statx_t
	if unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS, &sx) != nil ||
		sx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT == 0 || sx.Attributes&unix.STATX_ATTR_MOUNT_ROOT == 0 {
		return false
	}
	if !readOnly {
		return true
	}
	var fs unix.Statfs_t
	var st unix.Stat_t
	return unix.Fstatfs(int(f.Fd()), &fs) == nil && fs.Flags&unix.ST_RDONLY != 0 &&
		unix.Fstat(int(f.Fd()), &st) == nil && !linked(f, &st)
}

// ExecUnprivileged replaces devvm with the program at path, as Exec does,
// with no capability it could keep: devvm's ambient, permitted, effective
// and inheritable ones cleared, and no new privileges from a set-id file
// or file capabilities. The process's uid isn't root, so execve leaves it
// none. The bounding set stays whole, which takes CAP_SETPCAP to change,
// and gives nothing with no new privileges.
func ExecUnprivileged(path string, argv, env []string) error {
	// The calling thread is the one that execs: the capabilities are its.
	runtime.LockOSThread()
	if os.Getuid() == 0 {
		return errors.New("devvm runs QEMU only as a user that isn't root")
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	none := [2]unix.CapUserData{}
	if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &none[0]); err != nil {
		return err
	}
	return unix.Exec(path, argv, env)
}

// WaitExit returns a channel that's closed once the process pid ends,
// and a stop for the wait, which closes it too; a pid that can't be
// watched is taken as ended. It waits in Go's poller, which the stop
// wakes: a blocking poll(2) wouldn't wake as its fd closed, so its
// goroutine, and its thread, would stay.
func WaitExit(pid int) (<-chan struct{}, func()) {
	done := make(chan struct{})
	f, err := pollable(unix.PidfdOpen(pid, 0))
	if err != nil {
		close(done)
		return done, func() {}
	}
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		close(done)
		return done, func() {}
	}
	go func() {
		defer close(done)
		// Readable once the process ends; until then the poller waits.
		_ = rc.Read(func(fd uintptr) bool {
			n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 0) //nolint:gosec // an fd fits
			return err != nil || n > 0
		})
	}()
	return done, func() { _ = f.Close() }
}

// pollable is fd, which open returned, as a file Go's poller waits on:
// its reads wait there, and its close ends them.
func pollable(fd int, err error) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "pollable"), nil
}

// WatchFolders returns a channel that gets a value (dropped when one is
// waiting) whenever an entry of one of dirs is made, removed or renamed
// over, or a folder itself moves or goes, and a stop for the watch. A
// folder that can't be watched (it's gone, or the user's inotify watches
// ran out) fails it, naming the folder.
func WatchFolders(dirs []string) (<-chan struct{}, func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, nil, err
	}
	const mask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
		unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW
	for _, d := range dirs {
		if _, err := unix.InotifyAddWatch(fd, d, mask); err != nil {
			_ = unix.Close(fd)
			return nil, nil, &os.PathError{Op: "watch", Path: d, Err: err}
		}
	}
	// Read in Go's poller, which the stop wakes: a blocking read(2)
	// wouldn't wake as its fd closed, so its goroutine, and its thread,
	// would stay, and its next read could take another file's fd.
	f, err := pollable(fd, nil)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch, func() { _ = f.Close() }, nil
}

// UsernsLimit is why this host refuses an unprivileged user namespace
// the capabilities pins need, as its settings say: "apparmor" (Ubuntu's
// kernel.apparmor_restrict_unprivileged_userns), "sysctl"
// (kernel.unprivileged_userns_clone off, as linux-hardened has it), "max"
// (user.max_user_namespaces 0), or "" for none of those.
func UsernsLimit() string {
	return usernsLimit(func(path string) string {
		b, err := os.ReadFile(path) //nolint:gosec // the kernel's own settings
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	})
}

// usernsLimit is UsernsLimit, its settings read with read.
func usernsLimit(read func(path string) string) string {
	switch {
	case read("/proc/sys/kernel/apparmor_restrict_unprivileged_userns") == "1":
		return "apparmor"
	case read("/proc/sys/kernel/unprivileged_userns_clone") == "0":
		return "sysctl"
	case read("/proc/sys/user/max_user_namespaces") == "0":
		return "max"
	}
	return ""
}
