package lima

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// The oldest versions the lima backend runs with, as messages name them;
// only their first number is compared (major).
const (
	minMacOS = "13"
	minLima  = "2.2"
)

// Problems lists what Lima needs that this host lacks: macOS minMacOS or
// Linux on arm64 or x86_64, Lima minLima or newer, on Linux QEMU and
// /dev/kvm, and none of Lima's global configs in devvm's home. A fix is
// Homebrew's for Lima, when it's installed, and the package manager's for
// QEMU.
func (l *host) Problems(ctx context.Context) []backend.Problem {
	if l.OS != "darwin" && l.OS != "linux" {
		return []backend.Problem{{Key: "os", Msg: msg.LimaHostUnsupported(l.OS)}}
	}
	if l.Arch != "arm64" && l.Arch != "amd64" {
		return []backend.Problem{{Key: "arch", Msg: msg.LimaArchUnsupported(l.Arch)}}
	}
	var ps []backend.Problem
	if l.OS == "darwin" {
		if v := l.MacOS(); v != "" && major(v) < major(minMacOS) {
			ps = append(ps, backend.Problem{Key: "macos", Msg: msg.ProgramOld(minMacOS, "macOS", v)})
		}
	}
	if p, ok := l.limaProblem(ctx); ok {
		ps = append(ps, p)
	}
	if l.OS == "linux" {
		if p, ok := l.qemuProblem(); ok {
			ps = append(ps, p)
		}
		if p, ok := l.kvmProblem(); ok {
			ps = append(ps, p)
		}
	}
	if m, ok := l.globalConfigs(); ok {
		ps = append(ps, backend.Problem{Key: "lima-config", Msg: m})
	}
	return ps
}

// limaProblem is a missing Lima, one that doesn't run (built for another
// processor, say) or one older than minLima.
func (l *host) limaProblem(ctx context.Context) (backend.Problem, bool) {
	brew := has("brew")
	problem := func(fix string, plain msg.M, fixed func(fix string) msg.M) (backend.Problem, bool) {
		if !brew {
			return backend.Problem{Key: "lima", Msg: plain}, true
		}
		return backend.Problem{Key: "lima", Fix: fix, Msg: fixed(fix)}, true
	}
	if !has("limactl") {
		return problem("brew install lima", limaMissing(), func(fix string) msg.M { return msg.ProgramMissingFix(fix, "Lima", "limactl") })
	}
	ver := l.limaVersion(ctx)
	const command = "limactl --version"
	switch v := strings.TrimPrefix(ver, "v"); {
	case major(v) < 0:
		return problem("brew reinstall lima", msg.ProgramBroken(command, "Lima"), func(fix string) msg.M { return msg.ProgramBrokenFix(command, fix, "Lima") })
	case older(v, minLima):
		return problem("brew upgrade lima", msg.ProgramOld(minLima, "Lima", ver), func(fix string) msg.M { return msg.ProgramOldFix(fix, "Lima", ver) })
	}
	return backend.Problem{}, false
}

// limaMissing is the message for limactl not on PATH.
func limaMissing() msg.M { return msg.ProgramMissing("Lima", "limactl") }

// limaVersion is the version limactl --version prints last, or "" when
// it fails, read under a status line: every command's backend check
// runs it.
func (l *host) limaVersion(ctx context.Context) string {
	stop := l.Status(msg.CheckingBackend("Lima"))
	out, err := l.Runner.Output(l.limactl(ctx, "--version"))
	stop()
	if fields := strings.Fields(string(out)); err == nil && len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

// qemuProblem is QEMU's emulator for the host's processor, or qemu-img,
// missing; the fix is the first package manager's found.
func (l *host) qemuProblem() (backend.Problem, bool) {
	for _, prog := range []string{qemuProgram(l.Arch), "qemu-img"} {
		if has(prog) {
			continue
		}
		if fix := qemuInstall(l.Arch); fix != "" {
			return backend.Problem{Key: "qemu", Fix: fix, Msg: msg.ProgramMissingFix(fix, "QEMU", prog)}, true
		}
		return backend.Problem{Key: "qemu", Msg: qemuMissing(prog)}, true
	}
	return backend.Problem{}, false
}

// qemuMissing is the message for QEMU's program prog not on PATH.
func qemuMissing(prog string) msg.M { return msg.ProgramMissing("QEMU", prog) }

// qemuInstall is the command that installs QEMU with the first package
// manager found, or "". apt's leaves out QEMU's recommended GUI and audio
// libraries but not ipxe-qemu, whose efi-virtio.rom Lima's network card
// boots with, and adds the firmware.
func qemuInstall(arch string) string {
	switch {
	case has("apt-get"):
		apt := "sudo apt-get install -y --no-install-recommends qemu-utils ipxe-qemu"
		if arch == "amd64" {
			return apt + " qemu-system-x86 ovmf"
		}
		return apt + " qemu-system-arm qemu-efi-aarch64"
	case has("dnf"):
		return "sudo dnf install -y qemu-kvm qemu-img"
	case has("pacman"):
		return "sudo pacman -S --noconfirm qemu-base"
	case has("zypper"):
		return "sudo zypper --non-interactive install qemu-kvm"
	}
	return ""
}

// kvmProblem is a missing /dev/kvm (virtualization is off), or one the
// user can't open: the fix joins the kvm group.
func (l *host) kvmProblem() (backend.Problem, bool) {
	if _, err := os.Stat(l.KVM); err != nil {
		return backend.Problem{Key: "kvm", Msg: msg.NoKVM()}, true
	}
	f, err := os.OpenFile(l.KVM, os.O_RDWR, 0)
	if err == nil {
		_ = f.Close()
		return backend.Problem{}, false
	}
	name := l.Getenv("USER")
	if u, uerr := user.Current(); name == "" && uerr == nil {
		name = u.Username
	}
	fix := "sudo usermod -aG kvm " + cmp.Or(name, "$USER")
	return backend.Problem{Key: "kvm-access", Fix: fix, Msg: msg.KVMAccess(fix)}, true
}

// has reports whether name is a program on PATH.
func has(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// major is a version's first number (13 for 13.4), or -1.
func major(v string) int {
	s, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// older reports whether version v's major and minor numbers come before
// minimum's (2.1.9 before 2.2); a missing minor is 0.
func older(v, minimum string) bool {
	if a, b := major(v), major(minimum); a != b {
		return a < b
	}
	return minor(v) < minor(minimum)
}

// minor is a version's second number (4 for 13.4.1), or 0.
func minor(v string) int {
	_, rest, _ := strings.Cut(v, ".")
	s, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
