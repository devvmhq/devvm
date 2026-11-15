//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/shell"
)

// projectMountCache: on a Linux host, a new VM's project folder is a 9p
// mount with cache none, where an extending write that a stat races keeps
// its data, SQLite's WAL mode fails while its default journal and its
// exclusive WAL work, and a shared writable mapping is refused. A VM whose
// mode nobody chose (no mode in its state, Lima's mmap in its lima.yaml,
// as one made before devvm chose none) keeps mmap untouched, its lima.yaml
// unedited, and each launch warns, until devvm mount-cache none, the
// opt-in, gives it none at its next start (devvm restart), which mounts
// the folder again; what SQLite and the race do under mmap is reported. On
// macOS, the folder is virtiofs, and devvm mount-cache refuses the VM's.
// The VM is running, with none, when it ends.
func projectMountCache(s *S, w *webVM) {
	s.Section("the project folder's cache")
	p, vm := w.p, w.vm
	if runtime.GOOS != "linux" {
		s.Check("macOS: the folder is virtiofs, and devvm mount-cache refuses the VM's", func() error {
			r, _ := s.InVM(vm, `findmnt -no FSTYPE `+shell.Quote(p))
			if Trim(r.Stdout) != "virtiofs" {
				return errors.New("its file system: " + r.Stdout)
			}
			n, err := s.DevvmNames(p, "mount-cache")
			if err == nil || !grepF(n.Out, "backend_lacks backend=lima feature=mount-cache what="+vm) {
				return errors.New("devvm mount-cache:\n" + n.Out)
			}
			if _, ok := w.state(s)["mount.cache"]; ok {
				return errors.New("its state has a cache mode")
			}
			return nil
		})
		return
	}
	s.Check("a new VM's folder is a 9p mount with cache none, as its state says", func() error {
		opts := mountOptions(s, vm, p)
		s.r.out.line("  mount options: %s", opts)
		if w.state(s)["mount.cache"] != "none" || cacheOption(opts) != "none" {
			return fmt.Errorf("its state's %q, its options %q", w.state(s)["mount.cache"], opts)
		}
		r, _ := s.Devvm(p, "mount-cache")
		if Trim(r.Out) != "none" {
			return errors.New("devvm mount-cache: " + r.Out)
		}
		return nil
	})
	s.Check("none: extending writes that a stat races all reach this machine", func() error {
		short, err := statRace(s, vm, p)
		if err != nil {
			return err
		}
		if len(short) > 0 {
			return errors.New("short files: " + strings.Join(short, " "))
		}
		return nil
	})
	s.Check("none: SQLite's WAL mode fails in the folder, its default journal works", func() error {
		wal, err := sqliteIn(s, vm, p, "wal")
		s.r.out.line("  WAL: %s", Trim(wal))
		if err == nil {
			return errors.New("WAL worked: " + wal)
		}
		rb, err := sqliteIn(s, vm, p, "delete")
		if err != nil || Trim(rb) != "delete\n1" {
			return fmt.Errorf("the default journal: %q, %w", rb, err)
		}
		return nil
	})
	noneTakesAway(s, vm, p)
	// An existing VM whose mode nobody chose, as a VM made before devvm chose one has it: no mode in
	// its state, and Lima's mmap in its lima.yaml, which a stopped VM gets by hand; its first boot
	// after that edit still mounts the folder by the old fstab line, so a second boot shows it.
	lyaml := filepath.Join(s.sb.lima, vm, "lima.yaml")
	s.Check("a VM whose mode nobody chose keeps mmap, unedited, and each launch warns", func() error {
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		if err := SetState(s.Paths, vm, "mount", "{}"); err != nil {
			return err
		}
		if r, err := s.Limactl("edit", "--tty=false", vm, "--set", `.mounts[0]["9p"].cache = "mmap"`); err != nil {
			return fmt.Errorf("limactl edit: %w: %s", err, r.Out)
		}
		warn := s.Msg("mount_cache_unchosen", "mode=mmap", "vm="+vm)
		if r, err := DevvmIn(s, p, "exit\n", nil); err != nil || !grepF(r.Out, FirstLine(warn)) {
			return fmt.Errorf("no warning: %w\n%s", err, r.Out)
		}
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		before, err := os.ReadFile(lyaml)
		if err != nil {
			return err
		}
		r, err := DevvmIn(s, p, "exit\n", nil)
		if err != nil {
			return err
		}
		after, _ := os.ReadFile(lyaml)
		opts := mountOptions(s, vm, p)
		s.r.out.line("  mount options: %s", opts)
		switch {
		case !grepF(r.Out, FirstLine(warn)):
			return errors.New("no warning at the next launch:\n" + r.Out)
		case string(after) != string(before):
			return errors.New("its lima.yaml was edited")
		case cacheOption(opts) != "mmap":
			return errors.New("its options: " + opts)
		}
		return nil
	})
	// What mmap does: Lima's own mode, which devvm doesn't set. Reported, not checked, as the race
	// is timing's and the lock bug the kernel's.
	s.Check("mmap: what SQLite's default journal does in the folder (reported, not checked)", func() error {
		ok, first := 0, ""
		for range 5 {
			out, err := sqliteIn(s, vm, p, "delete")
			switch {
			case err == nil && Trim(out) == "delete\n1":
				ok++
			case first == "":
				first = strings.ReplaceAll(Trim(out), "\n", " ")
			}
		}
		s.r.out.line("  SQLite's default journal under mmap: %d of 5 new databases worked %s", ok, first)
		return nil
	})
	s.Check("mmap: what a stat-while-write race does to extending writes (the issue's bug: reported, not checked)", func() error {
		short, err := statRace(s, vm, p)
		s.r.out.line("  short files under mmap: %d", len(short))
		return err
	})
	s.Check("devvm mount-cache none opts it in: none at its next start, and no warning", func() error {
		r, _ := s.DevvmNames(p, "mount-cache", "none")
		if !grepF(r.Out, "next_start count=1 vm="+vm) {
			return errors.New("devvm mount-cache none:\n" + r.Out)
		}
		if _, err := DevvmIn(s, p, "exit\n", nil, "restart"); err != nil {
			return err
		}
		opts := mountOptions(s, vm, p)
		s.r.out.line("  mount options: %s", opts)
		if cacheOption(opts) != "none" || w.state(s)["mount.cache"] != "none" {
			return fmt.Errorf("its options %q, its state's %q", opts, w.state(s)["mount.cache"])
		}
		r, _ = DevvmIn(s, p, "exit\n", nil)
		if grepF(r.Out, FirstLine(s.Msg("mount_cache_unchosen", "mode=mmap", "vm="+vm))) {
			return errors.New("it still warns")
		}
		return nil
	})
	_, _ = s.InVM(vm, `rm -rf `+shell.Quote(p+"/.e2e-race")+` `+shell.Quote(p)+`/e2e-*.db*`)
}

// noneTakesAway: what else none takes away in the VM's folder dir, and
// what's left. A shared writable mapping of a file there (LMDB's lock
// file, mold's output) is refused with EINVAL, which the docs name; and
// SQLite in WAL mode with its exclusive locking mode, which keeps WAL's
// index on the heap rather than in a mapping, works, and opens again.
func noneTakesAway(s *S, vm, dir string) {
	s.Check("none: a shared writable mmap is refused (EINVAL); SQLite's exclusive WAL works", func() error {
		f := shell.Quote(filepath.Join(dir, "e2e-mmap"))
		r, _ := s.InVM(vm, `command -v python3 >/dev/null || { echo no-python3; exit 0; }; python3 -c '
import mmap, sys
f = open(sys.argv[1], "w+b"); f.truncate(4096)
try:
    mmap.mmap(f.fileno(), 4096, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE); print("mapped")
except OSError as e:
    print("refused:", e)
' `+f+` 2>&1; rm -f `+f)
		s.r.out.line("  shared writable mmap under none: %s", Trim(r.Stdout))
		db := shell.Quote(filepath.Join(dir, fmt.Sprintf("e2e-exclusive-%d.db", time.Now().UnixNano())))
		w, _ := s.InVM(vm, `sqlite3 -bail `+db+` "PRAGMA locking_mode=EXCLUSIVE; PRAGMA journal_mode=WAL; CREATE TABLE t(x); INSERT INTO t VALUES (1); SELECT count(*) FROM t;" 2>&1; `+
			`sqlite3 -bail `+db+` "PRAGMA locking_mode=EXCLUSIVE; SELECT count(*) FROM t;" 2>&1`)
		s.r.out.line("  exclusive WAL under none, then opened again: %s", strings.ReplaceAll(Trim(w.Stdout), "\n", " "))
		switch {
		case Trim(r.Stdout) != "no-python3" && !strings.Contains(r.Stdout, "Errno 22"):
			return errors.New("a shared writable mapping under none: " + r.Stdout)
		case Trim(w.Stdout) != "exclusive\nwal\n1\nexclusive\n1":
			return errors.New("exclusive WAL under none: " + w.Stdout)
		}
		return nil
	})
}

// mountOptions is the options of the VM's mount at dir, the mount's and
// its file system's, from the last line of /proc/self/mountinfo for it,
// as devvm's own check reads them, with a POSIX awk and no findmnt.
func mountOptions(s *S, vm, dir string) string {
	p := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(dir)
	r, _ := s.InVM(vm, `P=`+shell.Quote(p)+` awk '$5 == ENVIRON["P"] { l = $0 } END { print l }' /proc/self/mountinfo`)
	f := strings.Fields(Trim(r.Stdout))
	if len(f) < 10 {
		return Trim(r.Stdout)
	}
	sep := slices.Index(f[6:], "-")
	if sep < 0 || 6+sep+3 >= len(f) {
		return Trim(r.Stdout)
	}
	return f[5] + "," + f[6+sep+3]
}

// mountCacheSwitch: on a Linux host, vm, dir's VM, given Lima's mmap with
// no mode chosen, as a VM made before devvm chose one, gets devvm
// mount-cache none at its next start, whose remount devvm checks in
// /proc/self/mountinfo, portably, with no findmnt. Where the remount
// fails, as an OpenRC guest's mount of the share it just unmounted does,
// the launch restarts the VM once, and its boot mounts the folder with
// none: the launch succeeds either way.
func mountCacheSwitch(s *S, vm, dir, title string) {
	if runtime.GOOS != "linux" {
		return
	}
	s.Check(title+": devvm mount-cache none switches a VM left on Lima's mmap at its next start, its remount checked", func() error {
		if _, err := s.Devvm(dir, "stop"); err != nil {
			return err
		}
		if err := SetState(s.Paths, vm, "mount", "{}"); err != nil {
			return err
		}
		if r, err := s.Limactl("edit", "--tty=false", vm, "--set", `.mounts[0]["9p"].cache = "mmap"`); err != nil {
			return fmt.Errorf("limactl edit: %w: %s", err, r.Out)
		}
		if r, err := s.Devvm(dir, "mount-cache", "none"); err != nil {
			return fmt.Errorf("devvm mount-cache none: %w: %s", err, r.Out)
		}
		if r, err := DevvmIn(s, dir, "exit\n", nil); err != nil {
			return fmt.Errorf("the launch: %w\n%s", err, r.Out)
		}
		opts := mountOptions(s, vm, dir)
		s.r.out.line("  %s's mount options: %s", title, opts)
		if cacheOption(opts) != "none" || s.Paths.State(vm)["mount.cache"] != "none" {
			return fmt.Errorf("its options %q, its state's %q", opts, s.Paths.State(vm)["mount.cache"])
		}
		return nil
	})
}

// cacheOption is the cache mode 9p's options name, as the kernel shows
// it: cache=0x5 (or cache=5, cache=mmap) is mmap, and cache=0x0, none, or
// no cache option at all is none.
func cacheOption(opts string) string {
	m := regexp.MustCompile(`(?:^|,)cache=([^,]*)`).FindStringSubmatch(opts)
	if m == nil {
		return "none"
	}
	switch m[1] {
	case "0x0", "0", "none":
		return "none"
	case "0x5", "5", "mmap":
		return "mmap"
	}
	return m[1]
}

// statRace writes 40 files of 256 KiB into the VM's folder dir in 4 KiB
// writes, each made longer by every one, with fsync, while a loop stats
// the file, then lists the files that reached this machine short.
func statRace(s *S, vm, dir string) ([]string, error) {
	race := filepath.Join(dir, ".e2e-race")
	script := `set -e
d=` + shell.Quote(race) + `
rm -rf "$d"; mkdir -p "$d"
for i in $(seq 1 40); do
  f="$d/f$i"
  : > "$f"
  ( while [ ! -e "$d/done$i" ]; do stat -c %s "$f" >/dev/null 2>&1 || true; done ) &
  dd if=/dev/zero of="$f" bs=4096 count=64 conv=fsync,notrunc status=none
  : > "$d/done$i"
  wait
done`
	if _, err := s.InVM(vm, script); err != nil {
		return nil, fmt.Errorf("the race's script: %w", err)
	}
	var short []string
	for i := 1; i <= 40; i++ {
		f := filepath.Join(race, fmt.Sprintf("f%d", i))
		fi, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		if fi.Size() != 64*4096 {
			short = append(short, fmt.Sprintf("f%d:%d", i, fi.Size()))
		}
	}
	return short, nil
}

// sqliteIn makes a new database in the VM's folder dir in the journal
// mode mode (wal, delete), writes a row and reads it back: the mode SQLite
// set, then the row count, or SQLite's error. Each is a file of a name
// never used before, as the host can give a new file a deleted one's
// inode, whose pages a 9p client that caches may still hold.
func sqliteIn(s *S, vm, dir, mode string) (string, error) {
	db := shell.Quote(filepath.Join(dir, fmt.Sprintf("e2e-%s-%d.db", mode, time.Now().UnixNano())))
	r, err := s.InVM(vm, `sqlite3 -bail `+db+` "PRAGMA journal_mode=`+mode+`; CREATE TABLE t(x); INSERT INTO t VALUES (1); SELECT count(*) FROM t;" 2>&1`)
	return r.Stdout, err
}
