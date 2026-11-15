package lima

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/vmname"
)

// remountTimeout bounds the remount and its check: a guest whose mount
// hangs mustn't hold the start.
const remountTimeout = time.Minute

// notMounted is what remountCheck prints for a folder that isn't a mount
// point.
const notMounted = "devvm-not-mounted"

// cacheChanges reports whether a start of in as w wants it changes its
// project folder mount's cache mode (projectMount, mount.same's cache).
func cacheChanges(in instance, w backend.Want) bool {
	return w.Project != "" && w.ProjectCache != "" && in.ninep() && in.backend().ProjectCache != w.ProjectCache
}

// remountProject checks the project folder of a VM just started, w's,
// for w's cache mode, and first mounts it again, as its /etc/fstab now has
// it, when remount is set. Lima's cloud-init rewrites fstab with a mount's
// new options at the boot after an edit, but systemd has mounted the
// folder by the old line by then, and a 9p share's tag stays the same, so
// without this the new mode would wait for a second boot. Nothing in the
// VM uses the folder yet, unless something it starts at boot does (a
// container), which keeps the old mount busy. Its output goes to the run
// log.
//
// A folder that isn't mounted, one mounted with another mode, or a check
// that fails (ssh not answering) gets one restart (restartToMount): the
// VM is stopped, and the start fails with a RemountError, which its
// caller answers with a start again, w.CheckCache set, whose boot mounts
// the folder with the new mode. That start's check is the last: a folder
// that isn't mounted, or that the check can't see, stops the VM and fails
// the start, as what's written there would land on the VM's disk and
// vanish under the share at its next mount; one mounted with another
// mode is warned of. An interrupt meanwhile stops the VM too, whatever
// its tier, and gets no restart: the cleanup that stops it stays until
// any stop here returns.
func remountProject(ctx context.Context, l *machines, vm vmname.VM, w backend.Want, remount bool) error {
	stopIt := func(ctx context.Context) { _ = l.stop(ctx, vm) }
	done := l.AddCleanup(func(ctx context.Context) {
		if l.Interrupted() {
			stopIt(ctx)
		}
	})
	// The stop outlives the remount's timeout.
	parent := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, remountTimeout)
	defer cancel()
	if remount {
		d := shell.Quote(w.Project)
		script := "umount " + d + " || exit 3; mount " + d + " || { mount " + d + "; mountpoint -q " + d + " || exit 4; }"
		_ = l.Runner.Run(ctx, msg.Remounting(vm), bounded(l.limactl(ctx, "shell", "--workdir", "/", vm.String(), "sudo", "sh", "-c", script)))
	}
	has, err := mountedCache(ctx, l, vm, w.Project)
	// The cleanup stays until any stop here returns. An interrupt's is the
	// cleanup's to stop, once the command returns.
	defer func() {
		if !l.Interrupted() {
			done()
		}
	}()
	var fail msg.M
	var why string
	switch {
	case err != nil:
		fail, why = msg.MountCacheUnchecked(vm).Because(err), "the check failed: "+err.Error()
	case has == "":
		fail, why = msg.MountCacheUnmounted(vm), "it isn't mounted"
	case has != w.ProjectCache:
		fail, why = msg.MountCacheRemountFailed(has, vm, w.ProjectCache), "it's mounted with "+has
	default:
		return nil
	}
	mounted := err == nil && has != ""
	switch {
	case !w.CheckCache && !l.Interrupted():
		return restartToMount(parent, l, vm, why, fail, mounted)
	case mounted:
		l.Warn(fail)
		return nil
	case !l.Interrupted():
		// Interrupted, the cleanup stops it: one stop, not two at once.
		stopIt(parent)
	}
	return fail
}

// restartToMount stops the VM vm, whose project folder its start left as
// why says, for a start again that mounts it as it boots: it returns
// fail as a RemountError then, which the run log explains. Where the stop
// fails, a folder mounted with the old mode (mounted) is warned of, fail,
// and the VM goes on as it runs; one that isn't fails the start, as a
// stop that failed, the VM maybe still running.
func restartToMount(ctx context.Context, l *machines, vm vmname.VM, why string, fail msg.M, mounted bool) error {
	_, _ = io.WriteString(l.LogWriter(), "devvm: "+vm.String()+"'s project folder after its remount: "+why+"; restarting the VM\n")
	if err := l.Runner.Run(ctx, msg.RemountRestarting(vm), l.limactl(ctx, "stop", vm.String())); err != nil {
		if mounted {
			l.Warn(fail)
			return nil
		}
		return backend.Logged(msg.StopFailed(vm))
	}
	return &backend.RemountError{Msg: fail}
}

// maxCheckOutput caps what's read of the guest's answer to a mount check:
// one line of /proc/self/mountinfo, or notMounted.
const maxCheckOutput = 4096

// lastMount is the awk program that prints the last line of a mountinfo
// file whose mount point, its fifth field, is $P exactly, as mountinfo
// escapes it (mountinfoPath): where mounts are stacked, the last is the
// one visible there. P comes in through the environment, which awk reads
// as it is, unlike -v, which would undo the escapes. It's POSIX, so any
// guest's awk runs it, busybox's too, and the check depends on no
// util-linux tool but mountpoint, which busybox has as well.
const lastMount = `awk '$5 == ENVIRON["P"] { l = $0 } END { print l }'`

// mountedCache asks the guest how its folder at dir is mounted: the cache
// mode of the mount it sees there, cacheOfOptions's, "" when it isn't a
// mount point, or an error when the guest doesn't answer or its answer
// isn't a mountinfo line. Only the last line counts, and only so much of
// it is read (maxCheckOutput).
func mountedCache(ctx context.Context, l *machines, vm vmname.VM, dir string) (string, error) {
	d := shell.Quote(dir)
	check := "if mountpoint -q " + d + "; then P=" + shell.Quote(mountinfoPath(dir)) + " " + lastMount +
		" /proc/self/mountinfo; else echo " + notMounted + "; fi"
	cmd := bounded(l.limactl(ctx, "shell", "--workdir", "/", vm.String(), "sh", "-c", check))
	out := &capped{max: maxCheckOutput}
	cmd.Stdout = out
	if err := l.Runner.Attach(cmd); err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	switch last {
	case notMounted:
		return "", nil
	case "":
		return "", errors.New("the guest gave no answer")
	}
	opts, ok := optionsOfMountinfo(last)
	if !ok {
		return "", errors.New("the guest's answer isn't a line of mountinfo")
	}
	return cacheOfOptions(opts), nil
}

// mountinfoPath is path as /proc/self/mountinfo writes a mount point: a
// space, a tab, a newline and a backslash as octal escapes (\040, \011,
// \012, \134), every other byte as it is.
func mountinfoPath(path string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(path)
}

// optionsOfMountinfo is a mountinfo line's options, the mount's and then
// its file system's, joined by a comma, as findmnt's OPTIONS shows them;
// false for a line that isn't one. The optional fields between the mount
// options and the file system's type vary in number, so the file
// system's options are the third field after the "-" that ends them.
func optionsOfMountinfo(line string) (string, bool) {
	f := strings.Fields(line)
	if len(f) < 10 {
		return "", false
	}
	sep := slices.Index(f[6:], "-")
	if sep < 0 || 6+sep+3 >= len(f) {
		return "", false
	}
	return f[5] + "," + f[6+sep+3], true
}

// bounded is cmd, its output's pipes closed a second after it ends or
// its context does: limactl shell's ssh can leave a child behind that
// holds them open, which would hold the caller past its timeout.
func bounded(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = time.Second
	return cmd
}

// capped keeps the first max bytes written to it, and takes the rest
// without keeping it. Its buffer isn't embedded: io.Copy would use the
// buffer's own ReadFrom, which keeps everything.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// cacheOption finds a 9p mount's cache option in its mount options.
var cacheOption = regexp.MustCompile(`(?:^|,)cache=([^,]*)`)

// cacheOfOptions is the cache mode a 9p mount's options name
// (optionsOfMountinfo's):
// the kernel shows cache=0x5 for mmap, and no cache option at all for
// none. Any other is "unknown": the guest's text never reaches a message.
func cacheOfOptions(opts string) string {
	m := cacheOption.FindStringSubmatch(opts)
	if m == nil {
		return backend.CacheNone
	}
	switch m[1] {
	case "0x0", "0", backend.CacheNone:
		return backend.CacheNone
	case "0x5", "5", backend.CacheMmap:
		return backend.CacheMmap
	}
	return "unknown"
}
