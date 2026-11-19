//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// projectRenumbered: a project folder on a disk image of its own, which macOS numbers anew when
// it's detached and attached again after another image: the VM records the folder's volume, and
// once the folder has moved on that disk, devvm in it finds the VM by its volume, inode and
// birth time, offering it rather than making a second VM. The check first makes sure macOS gave
// the disk another device number, attaching more images first until it does, and is skipped
// when it never does. hdiutil needs no root.
func projectRenumbered(s *S) {
	s.Section("a moved folder on a disk numbered anew")
	tmp, err := os.MkdirTemp("", "devvm-e2e-renumber-")
	if err == nil {
		// devvm records a folder's physical path, never through /var's link.
		tmp, err = filepath.EvalSymlinks(tmp)
	}
	if err != nil {
		s.Check("disk images for the renumbering", func() error { return err })
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	mnt, other := filepath.Join(tmp, "mnt"), filepath.Join(tmp, "other")
	img := func(name string) string { return filepath.Join(tmp, name+".sparseimage") }
	run := func(args ...string) (string, error) {
		r, err := s.Run(Cmd{Args: args})
		return r.Out, err
	}
	device := regexp.MustCompile(`(?m)^/dev/(disk[0-9]+)\b`)
	attach := func(name, at string) (string, error) {
		out, err := run("hdiutil", "attach", "-nobrowse", "-mountpoint", at, img(name))
		if m := device.FindStringSubmatch(out); err == nil && m != nil {
			return m[1], nil
		}
		return "", fmt.Errorf("hdiutil attach %s: %w\n%s", name, err, out)
	}
	var disks []string
	defer func() {
		for _, d := range disks {
			_, _ = run("hdiutil", "detach", "-force", d)
		}
	}()
	var vm, p, moved string
	s.Check("a VM made for a folder on a disk image records the folder's volume", func() error {
		if out, err := run("hdiutil", "create", "-size", "64m", "-fs", "APFS", "-volname", "e2e-a", "-type", "SPARSE", img("a")); err != nil {
			return fmt.Errorf("hdiutil create: %w\n%s", err, out)
		}
		d, err := attach("a", mnt)
		if err != nil {
			return err
		}
		disks = append(disks, d)
		p = filepath.Join(mnt, "proj")
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
		s.sb.projects = append(s.sb.projects, p)
		var r Res
		vm, r, err = s.Launch(p, "exit", "--network", "open", "--no-git-protect", "--tools", "none", "--agents", "none")
		if err != nil {
			return fmt.Errorf("devvm: %w\n%s", err, r.Out)
		}
		st := s.Paths.State(vm)
		if !strings.HasPrefix(st["folder_volume"], "uuid:") {
			return fmt.Errorf("its state records no volume: %v", st)
		}
		_, err = s.Devvm(p, "stop")
		return err
	})
	// renumber detaches the disks and attaches others before the folder's, one more each try,
	// until macOS gives the folder's disk another device number than the one its state records:
	// whether it did, within three tries.
	renumber := func(before string) (bool, error) {
		for try := 1; try <= 3; try++ {
			for _, d := range disks {
				if out, err := run("hdiutil", "detach", d); err != nil {
					return false, fmt.Errorf("hdiutil detach: %w\n%s", err, out)
				}
			}
			disks = nil
			for i := range try {
				name := fmt.Sprintf("b%d", i)
				if _, err := os.Stat(img(name)); err != nil {
					if out, err := run("hdiutil", "create", "-size", "64m", "-fs", "APFS", "-volname", "e2e-"+name, "-type", "SPARSE", img(name)); err != nil {
						return false, fmt.Errorf("hdiutil create: %w\n%s", err, out)
					}
				}
				at := filepath.Join(other, name)
				if err := os.MkdirAll(at, 0o700); err != nil {
					return false, err
				}
				d, err := attach(name, at)
				if err != nil {
					return false, err
				}
				disks = append(disks, d)
			}
			d, err := attach("a", mnt)
			if err != nil {
				return false, err
			}
			disks = append(disks, d)
			var st syscall.Stat_t
			if err := syscall.Stat(p, &st); err != nil {
				return false, err
			}
			if dev, _, _ := strings.Cut(before, ":"); dev != fmt.Sprint(st.Dev) { // as FolderID writes it
				return true, nil
			}
		}
		return false, nil
	}
	const title = "the disk numbered anew and the folder moved: devvm finds the VM, and makes no second one"
	var before string
	renumbered, rerr := false, errors.New("no VM: the check before failed")
	if vm != "" {
		before = s.Paths.State(vm)["folder"]
		renumbered, rerr = renumber(before)
	}
	if rerr == nil && !renumbered {
		s.Skip(title, "macOS gave the folder's disk the device number it had, in three tries")
		_, _ = s.Devvm(p, "rm", "--force", "--no-export")
		return
	}
	s.Check(title, func() error {
		if rerr != nil {
			return rerr
		}
		moved = filepath.Join(mnt, "proj-moved")
		if err := os.Rename(p, moved); err != nil {
			return err
		}
		s.sb.projects = append(s.sb.projects, moved)
		r, err := s.Devvm(moved, "stop")
		if err == nil || !grepF(r.Out, s.Msg("vm_moved_here", "vm="+vm, "was="+p)) {
			return fmt.Errorf("not offered (its folder was %s): %w\n%s", before, err, r.Out)
		}
		if got := s.Paths.VMOf(moved); got != "" {
			return errors.New("a second VM: " + got)
		}
		return nil
	})
	if vm != "" {
		_, _ = s.Devvm(moved, "adopt", "--force")
		_, _ = s.Devvm(moved, "rm", "--force", "--no-export")
	}
}
