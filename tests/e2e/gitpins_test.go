//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// On a Linux host that pins, .git is on: its hooks and config are pinned
// read-only in the user and mount namespace devvm runs the VM's QEMU in,
// and .git itself can't be renamed. These are the design note's checks
// (contributing/design.md, read-only paths).

// pinnedProcs are the processes of vm's QEMU namespace on this host: QEMU
// itself, by its arguments, once it runs in a namespace other than this
// one's, and its keeper, whose pins name the project folder p; by pid, and
// whether each is QEMU.
func pinnedProcs(p, vm string) map[int]bool {
	out := map[int]bool{}
	self, _ := os.Readlink("/proc/self/ns/mnt")
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := string(cmd)
		ns, _ := os.Readlink(filepath.Join("/proc", e.Name(), "ns", "mnt"))
		qemu := strings.Contains(args, "qemu-system") && strings.Contains(args, "/"+vm+"/") && !strings.Contains(args, "_qemu") && ns != self
		keeper := strings.Contains(args, "_qemu\x00pinned") && pinsRoot(string(env)) == p
		if qemu || keeper {
			out[pid] = qemu
		}
	}
	return out
}

// pinsRoot is the project folder whose pins a process's environment env
// (/proc/PID/environ) names: the root in the file DEVVM_QEMU_PINS holds;
// "" for none.
func pinsRoot(env string) string {
	for kv := range strings.SplitSeq(env, "\x00") {
		if f, ok := strings.CutPrefix(kv, "DEVVM_QEMU_PINS="); ok && f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return ""
			}
			var set struct {
				Root string `json:"root"`
			}
			_ = json.Unmarshal(b, &set)
			return set.Root
		}
	}
	return ""
}

func projectGitPins(s *S, w *webVM) {
	p, vm := w.p, w.vm
	s.Check("QEMU runs in a namespace that pins .git's hooks and config read-only, with no capability", func() error {
		var qemu int
		for pid, isQEMU := range pinnedProcs(p, vm) {
			if isQEMU {
				qemu = pid
			}
		}
		if qemu == 0 {
			return errors.New("no QEMU with pins for " + p)
		}
		status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", qemu))
		mounts, _ := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", qemu))
		for _, want := range []string{"CapInh:\t0000000000000000", "CapEff:\t0000000000000000", "CapPrm:\t0000000000000000", "CapAmb:\t0000000000000000", "NoNewPrivs:\t1"} {
			if !strings.Contains(string(status), want) {
				return fmt.Errorf("QEMU's status lacks %q:\n%s", want, status)
			}
		}
		for _, rel := range []string{".git/config", ".git/hooks"} {
			found := false
			for l := range strings.Lines(string(mounts)) {
				f := strings.Fields(l)
				found = found || len(f) > 5 && f[4] == filepath.Join(p, rel) && strings.HasPrefix(f[5], "ro")
			}
			if !found {
				return fmt.Errorf("%s isn't pinned read-only in QEMU's namespace", rel)
			}
		}
		return nil
	})
	for _, c := range []struct{ what, script string }{
		{"add a hook", "echo x > .git/hooks/pre-commit"},
		{"change the config", "echo x >> .git/config"},
		{"replace the config", "cp .git/config /tmp/c && echo x >> /tmp/c && mv -f /tmp/c .git/config"},
		{"rename .git", "mv .git .git-moved"},
		{"move the hooks folder", "mv .git/hooks .git/hooks-moved"},
		{"remove the config", "rm -f .git/config"},
	} {
		s.Check("root in the VM can't "+c.what, func() error {
			if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "cd "+p+" && "+c.script); err == nil {
				return errors.New("root could " + c.what)
			}
			if !Exists(filepath.Join(p, ".git", "config")) || Exists(filepath.Join(p, ".git", "hooks", "pre-commit")) || Exists(filepath.Join(p, ".git-moved")) {
				return errors.New(".git changed")
			}
			return nil
		})
	}
	s.Check("git in the VM can commit", func() error {
		_, err := s.InVM(vm, "cd "+p+` && for i in 1 2 3 4 5 6 7 8 9 10; do git status >/dev/null 2>&1 && break; sleep 1; done &&
    git add -A && git -c user.email=e@e -c user.name=e commit -qm vm`)
		return err
	})
	s.Check("git config on this machine reaches the VM, and the new config stays pinned", func() error {
		if _, err := s.Run(Cmd{Args: []string{"git", "config", "e2e.host", "pinned"}, Dir: p}); err != nil {
			return err
		}
		if err := s.Until(15*time.Second, func() error {
			r, _ := s.InVM(vm, "git -C "+p+" config e2e.host")
			if Trim(r.Stdout) != "pinned" {
				return errors.New("the VM doesn't see it: " + r.Stdout)
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "echo x >> "+p+"/.git/config"); err == nil {
			return errors.New("root changed the config git config on this machine wrote")
		}
		if g, _ := s.Run(Cmd{Args: []string{"git", "config", "e2e.host"}, Dir: p}); Trim(g.Stdout) != "pinned" {
			return errors.New("this machine's config: " + g.Stdout)
		}
		return nil
	})
	// Every pin missing: .git, then what's read-only.
	missing := FirstLine(s.Msg("git_pins_missing", "paths="+p+"/.git, "+p+"/.git/hooks, "+p+"/.git/config", "vm="+vm))
	s.Check("a launch of the running VM finds its pins", func() error {
		r, err := DevvmIn(s, p, "exit\n", nil)
		if err != nil {
			return err
		}
		// Any of them missing says so, whatever the paths.
		if before, _, _ := strings.Cut(missing, p); grepF(r.Out, before) {
			return errors.New("it says the pins are missing:\n" + r.Out)
		}
		return nil
	})
	s.Check("devvm stop names the config git config here replaced while the VM ran, pinned again and changed", func() error {
		r, err := s.Devvm(p, "stop")
		if err != nil {
			return err
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_pin_again", "path="+p+"/.git/config", "vm="+vm))) ||
			!grepF(r.Out, FirstLine(s.Msg("git_changed", "paths=.git/config", "vm="+vm))) {
			return errors.New("devvm stop didn't name the config:\n" + r.Out)
		}
		if grepF(r.Out, FirstLine(s.Msg("git_pins_unrecorded", "vm="+vm))) {
			return errors.New("devvm stop says the record isn't whole:\n" + r.Out)
		}
		// Put back while it's stopped, and recorded so by another check.
		_, _ = s.Run(Cmd{Args: []string{"git", "config", "--unset", "e2e.host"}, Dir: p})
		if again, _ := s.Devvm(p, "stop"); grepF(again.Out, FirstLine(s.Msg("git_pin_again", "path="+p+"/.git/config", "vm="+vm))) {
			return errors.New("named twice:\n" + again.Out)
		}
		_, err = DevvmIn(s, p, "exit\n", nil)
		return err
	})
	s.Check("started with limactl start, a launch names its missing pins", func() error {
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		if _, err := s.Limactl("start", vm); err != nil {
			return err
		}
		r, _ := DevvmIn(s, p, "exit\n", nil)
		if !grepF(r.Out, missing) {
			return errors.New("the launch didn't name the missing pins:\n" + r.Out)
		}
		_, _ = s.Devvm(p, "stop")
		_, err := DevvmIn(s, p, "exit\n", nil)
		return err
	})
}

func projectGitProtectPins(s *S, w *webVM) {
	p, vm := w.p, w.vm
	s.Check("a submodule git folder the VM adds is reported by devvm stop, and pinned at the next start", func() error {
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", "cd "+p+` &&
      for i in 1 2 3 4 5 6 7 8 9 10; do git rev-parse >/dev/null 2>&1 && break; sleep 1; done && git init -q vmsub &&
      git -C vmsub -c user.email=e@e -c user.name=e commit -q --allow-empty -m s &&
      git update-index --add --cacheinfo 160000,$(git -C vmsub rev-parse HEAD),vmsub &&
      mkdir -p src/hooks evil && echo 'export {}' > src/hooks/useThing.ts && echo 'gitdir: ../src' > evil/.git &&
      git update-index --add --cacheinfo 160000,$(git -C vmsub rev-parse HEAD),evil`); err != nil {
			return err
		}
		r, err := s.Devvm(p, "stop")
		if err != nil {
			return err
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_embedded_new", "git="+p+"/vmsub/.git"))) || s.Status(vm) != "Stopped" {
			return errors.New("devvm stop didn't report it:\n" + r.Out)
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_gitdir_not_git", "git="+p+"/src", "sub="+p+"/evil"))) {
			return errors.New("devvm stop didn't report evil:\n" + r.Out)
		}
		if len(pinnedProcs(p, vm)) > 0 {
			return errors.New("QEMU's namespace outlived it")
		}
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "echo x >> "+p+"/vmsub/.git/config"); err == nil {
			return errors.New("root changed the new submodule's config")
		}
		if _, err := s.InVM(vm, "echo 'export {}' >> "+p+"/src/hooks/useThing.ts"); err != nil {
			return errors.New("src/hooks, which isn't a git folder's, is pinned")
		}
		return nil
	})
	s.Check("git-protect off on a running VM waits for it to stop, and the next start pins nothing", func() error {
		r, _ := s.Devvm(p, "git-protect", "off")
		if !grepF(r.Out, FirstLine(s.Msg("git_protect_next_stop", "mode=off", "vm="+vm))) {
			return errors.New("devvm git-protect off:\n" + r.Out)
		}
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		if _, err := DevvmIn(s, p, "exit\n", nil); err != nil {
			return err
		}
		if len(pinnedProcs(p, vm)) > 0 {
			return errors.New("it runs with pins")
		}
		if _, err := s.InVM(vm, "printf '# e2e\\n' >> "+p+"/.git/config"); err != nil {
			return errors.New("off, the VM can't write .git/config")
		}
		return nil
	})
	s.Check("a VM an earlier devvm left strict is on at its next launch, and pinned", func() error {
		_, _ = s.Limactl("stop", vm)
		if _, err := s.Devvm(p, "git-protect", "on"); err != nil {
			return err
		}
		// As strict left it: diff.ignoreSubmodules = all in the config, and
		// its mode.
		if _, err := s.Run(Cmd{Args: []string{"git", "config", "--file", ".git/config", "diff.ignoreSubmodules", "all"}, Dir: p}); err != nil {
			return err
		}
		if err := SetState(s.Paths, vm, "git.mode", `"strict"`); err != nil {
			return err
		}
		r, err := DevvmIn(s, p, "exit\n", nil)
		if err != nil {
			return err
		}
		if !grepF(r.Out, FirstLine(s.Msg("git_strict_converted", "vm="+vm))) {
			return errors.New("the launch didn't say it's on:\n" + r.Out)
		}
		g, _ := s.Run(Cmd{Args: []string{"git", "config", "--file", ".git/config", "diff.ignoreSubmodules"}, Dir: p})
		m, _ := s.Devvm(p, "git-protect")
		if Trim(g.Stdout) != "" || Trim(m.Stdout) != "on" {
			return fmt.Errorf("diff.ignoreSubmodules %q, mode %q", g.Stdout, m.Stdout)
		}
		if _, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "sh", "-c", "echo x >> "+p+"/.git/config"); err == nil {
			return errors.New("on again, root changed the config")
		}
		r, err = s.Devvm(p, "stop")
		if err != nil {
			return err
		}
		if grepF(r.Out, FirstLine(s.Msg("git_strict_converted", "vm="+vm))) || strings.Contains(r.Out, ".git/config") {
			return errors.New("devvm stop named it again:\n" + r.Out)
		}
		return nil
	})
}
