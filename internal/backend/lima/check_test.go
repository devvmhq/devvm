package lima

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// onlyOnPath puts a new folder alone on PATH, with programs that only
// exit.
func onlyOnPath(t *testing.T, progs ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range progs {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a test's own programs
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// checkedUnderStatus fails t when limactl --version runs with anything
// but its status line showing.
func checkedUnderStatus(t *testing.T, l *Lima, f *fakeRunner) {
	t.Helper()
	fake, ok := l.Reporter.(*msg.Fake)
	if !ok {
		t.Fatal("the backend's Reporter isn't a msg.Fake")
	}
	f.do = func(c call) {
		if m, _ := fake.Showing(); c.line() == "limactl --version" && m.Names() != "checking_backend name=Lima" {
			t.Errorf("limactl --version under %q", m.Names())
		}
	}
}

// Problems names what Lima needs that this machine lacks, with its fix,
// limactl --version under its status line.
func TestProblems(t *testing.T) {
	const (
		v2      = "limactl version 2.2.1\n"
		apt     = "sudo apt-get install -y --no-install-recommends qemu-utils ipxe-qemu"
		aptArm  = apt + " qemu-system-arm qemu-efi-aarch64"
		aptX86  = apt + " qemu-system-x86 ovmf"
		usermod = "sudo usermod -aG kvm alice"
		noLima  = "program_missing name=Lima program=limactl"
		broken  = "program_broken command=limactl --version name=Lima"
	)
	for _, tc := range []struct {
		name, os, arch string
		path           []string // the programs on PATH
		version        string   // limactl --version's output; fail for a failure
		macos          string
		kvm            string // the device: rw (the default), ro or missing
		globalConfig   bool
		want           []string // key|fix|message names
	}{
		{name: "macOS, all there", path: []string{"limactl"}, version: v2},
		{name: "Windows", os: "windows", want: []string{"os||lima_host_unsupported os=windows"}},
		{name: "riscv64", os: "linux", arch: "riscv64", want: []string{"arch||lima_arch_unsupported arch=riscv64"}},
		{name: "macOS 12", path: []string{"limactl"}, version: v2, macos: "12.7.6", want: []string{"macos||program_old min=13 name=macOS version=12.7.6"}},
		{name: "macOS unknown", path: []string{"limactl"}, version: v2, macos: "-"},
		{name: "no Lima", want: []string{"lima||" + noLima}},
		{
			name: "no Lima, Homebrew", path: []string{"brew"},
			want: []string{"lima|brew install lima|program_missing_fix fix=brew install lima name=Lima program=limactl"},
		},
		{
			name: "Lima 1", path: []string{"limactl"}, version: "limactl version 1.2.0\n",
			want: []string{"lima||program_old min=2.2 name=Lima version=1.2.0"},
		},
		{
			name: "Lima 2.1", path: []string{"limactl"}, version: "limactl version 2.1.9\n",
			want: []string{"lima||program_old min=2.2 name=Lima version=2.1.9"},
		},
		{name: "Lima 2.2.0", path: []string{"limactl"}, version: "limactl version 2.2.0\n"},
		{name: "Lima 3.0", path: []string{"limactl"}, version: "limactl version 3.0.0\n"},
		{
			name: "Lima 1, Homebrew", path: []string{"limactl", "brew"}, version: "limactl version 1.2.0\n",
			want: []string{"lima|brew upgrade lima|program_old_fix fix=brew upgrade lima name=Lima version=1.2.0"},
		},
		{name: "limactl doesn't run", path: []string{"limactl"}, version: "fail", want: []string{"lima||" + broken}},
		{
			name: "limactl doesn't run, Homebrew", path: []string{"limactl", "brew"}, version: "fail",
			want: []string{"lima|brew reinstall lima|program_broken_fix command=limactl --version fix=brew reinstall lima name=Lima"},
		},
		{name: "limactl prints nothing", path: []string{"limactl"}, want: []string{"lima||" + broken}},
		{
			name: "a Lima global config", path: []string{"limactl"}, version: v2, globalConfig: true,
			want: []string{"lima-config||lima_global_config files=<config>"},
		},
		{name: "Linux, all there", os: "linux", path: []string{"limactl", "qemu-system-aarch64", "qemu-img"}, version: v2},
		{
			name: "Linux arm64, apt", os: "linux", path: []string{"limactl", "apt-get"}, version: v2,
			want: []string{"qemu|" + aptArm + "|program_missing_fix fix=" + aptArm + " name=QEMU program=qemu-system-aarch64"},
		},
		{
			name: "Linux x86_64 with the arm64 emulator", os: "linux", arch: "amd64",
			path: []string{"limactl", "apt-get", "qemu-system-aarch64", "qemu-img"}, version: v2,
			want: []string{"qemu|" + aptX86 + "|program_missing_fix fix=" + aptX86 + " name=QEMU program=qemu-system-x86_64"},
		},
		{name: "Linux x86_64, all there", os: "linux", arch: "amd64", path: []string{"limactl", "qemu-system-x86_64", "qemu-img"}, version: v2},
		{
			name: "no qemu-img, dnf", os: "linux", path: []string{"limactl", "dnf", "qemu-system-aarch64"}, version: v2,
			want: []string{"qemu|sudo dnf install -y qemu-kvm qemu-img|program_missing_fix fix=sudo dnf install -y qemu-kvm qemu-img name=QEMU program=qemu-img"},
		},
		{
			name: "pacman", os: "linux", path: []string{"limactl", "pacman"}, version: v2,
			want: []string{"qemu|sudo pacman -S --noconfirm qemu-base|program_missing_fix fix=sudo pacman -S --noconfirm qemu-base name=QEMU program=qemu-system-aarch64"},
		},
		{
			name: "zypper", os: "linux", path: []string{"limactl", "zypper"}, version: v2,
			want: []string{"qemu|sudo zypper --non-interactive install qemu-kvm|program_missing_fix fix=sudo zypper --non-interactive install qemu-kvm name=QEMU program=qemu-system-aarch64"},
		},
		{
			name: "no /dev/kvm", os: "linux", path: []string{"limactl", "qemu-system-aarch64", "qemu-img"}, version: v2, kvm: "missing",
			want: []string{"kvm||no_kvm"},
		},
		{
			name: "no access to /dev/kvm", os: "linux", path: []string{"limactl", "qemu-system-aarch64", "qemu-img"}, version: v2, kvm: "ro",
			want: []string{"kvm-access|" + usermod + "|kvm_access fix=" + usermod},
		},
		{
			name: "Linux, nothing there", os: "linux", kvm: "missing", globalConfig: true,
			want: []string{
				"lima||" + noLima, "qemu||program_missing name=QEMU program=qemu-system-aarch64", "kvm||no_kvm",
				"lima-config||lima_global_config files=<config>",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kvm == "ro" && os.Geteuid() == 0 {
				t.Skip("root opens any device")
			}
			l, f, _ := testLima(t, tc.os)
			if tc.os == "" {
				l.OS = "darwin"
			}
			if tc.arch != "" {
				l.Arch = tc.arch
			}
			if tc.macos != "" {
				v := tc.macos
				if v == "-" {
					v = ""
				}
				l.MacOS = func() string { return v }
			}
			l.Getenv = func(k string) string {
				if k == "USER" {
					return "alice"
				}
				return ""
			}
			switch tc.kvm {
			case "ro":
				if err := os.Chmod(l.KVM, 0o400); err != nil {
					t.Fatal(err)
				}
			case "missing":
				l.KVM = filepath.Join(t.TempDir(), "kvm")
			}
			config := filepath.Join(l.Dirs.Lima, "_config", "default.yaml")
			if tc.globalConfig {
				if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.version == "fail" {
				f.fail = []string{"limactl --version"}
			} else {
				f.out["limactl --version"] = tc.version
			}
			onlyOnPath(t, tc.path...)
			checkedUnderStatus(t, l, f)

			var got []string
			for _, p := range l.Problems(t.Context()) {
				got = append(got, p.Key+"|"+p.Fix+"|"+p.Msg.Names())
			}
			want := slices.Clone(tc.want)
			for i, w := range want {
				if w == "lima-config||lima_global_config files=<config>" {
					want[i] = "lima-config||lima_global_config files=" + config
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("Problems:\n%q\nwant\n%q", got, want)
			}
		})
	}
}
