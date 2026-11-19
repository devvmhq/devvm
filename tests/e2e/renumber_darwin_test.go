//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// projectRenumbered: a project folder on a disk image of its own, which macOS numbers anew when
// it's detached and attached again after another image: the VM records the folder's volume, and
// once the folder has moved on that disk, devvm in it finds the VM by its volume, inode and
// birth time, offering it rather than making a second VM. hdiutil needs no root.
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
		for _, name := range []string{"a", "b"} {
			if out, err := run("hdiutil", "create", "-size", "64m", "-fs", "APFS", "-volname", "e2e-"+name, "-type", "SPARSE", img(name)); err != nil {
				return fmt.Errorf("hdiutil create: %w\n%s", err, out)
			}
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
	s.Check("the disk numbered anew and the folder moved: devvm finds the VM, and makes no second one", func() error {
		if vm == "" {
			return errors.New("no VM: the check before failed")
		}
		before := s.Paths.State(vm)["folder"]
		for _, d := range disks {
			if out, err := run("hdiutil", "detach", d); err != nil {
				return fmt.Errorf("hdiutil detach: %w\n%s", err, out)
			}
		}
		disks = nil
		db, err := attach("b", other)
		if err != nil {
			return err
		}
		disks = append(disks, db)
		da, err := attach("a", mnt)
		if err != nil {
			return err
		}
		disks = append(disks, da)
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
