//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
)

// The container images the --from checks start from, by digest, so the
// bases made from them stay cached until these pins move
// (scripts/bump.sh e2e-from moves the digests). One check keeps a tag,
// so devvm's registry lookup is checked too: the company proxy's
// alpine:3.21, converted each run anyway.
const fromNodeDigest = "sha256:8fa96264f302f58bbb13aa7339c541547b56975766ce956c875da149452a3e40"

const fromAlpineDigest = "sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a"

const fromUbuntuDigest = "sha256:6a06111c3b0a8600feadb270aec771dc239bf407b25625f93f44ab00e11f4d3c"

const (
	fromNode   = "node:24@" + fromNodeDigest
	fromAlpine = "alpine:3@" + fromAlpineDigest
	fromUbuntu = "ubuntu:26.04@" + fromUbuntuDigest
)

// fromSection: VMs from node:24 (Debian 12) and alpine:3, base from
// ubuntu:26.04, with a docker that's installed but not running (it logs
// what devvm asked it, which may only be whether it has the image:
// launches ask only the backends a VM's state names), then a --from
// conversion behind a company proxy.
func fromSection(s *S) {
	s.Section("--from")
	s = s.Needing(backend.FeatureFrom)
	nd, _ := NoDocker(s)
	fromDebian(s, nd)
	fromAlpineVM(s, nd)
	// Ubuntu's container image has empty package lists, which apt.sh fills
	// from its own sources.
	s.Check("base builds from ubuntu:26.04", func() error {
		_, err := s.Run(Cmd{Args: []string{s.sb.devvm, "build", "--from", fromUbuntu, "base"}, Env: nd})
		return err
	}, Builds())
	s.Check("none of them needed Docker", func() error {
		b, err := os.ReadFile(DockerCalls(s))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if line != "" && !strings.HasPrefix(line, "docker image inspect ") {
				return errors.New("devvm asked docker: " + line)
			}
		}
		return nil
	})
	s.Check("and the build VM and its temporary folders are gone", func() error {
		r, _ := s.Limactl("list", "-q")
		if r.Has("devvm-oci-build") {
			return errors.New("the build VM is still there")
		}
		return ociTempsGone()
	})
	// An image only Docker has (built, never pushed) isn't checked: making
	// one would change Docker's images.
	fromCorp(s)
}

// ociTempsGone: no devvm-oci.* folder is left in the temporary folder.
func ociTempsGone() error {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	if m, _ := filepath.Glob(filepath.Join(tmp, "devvm-oci.*")); len(m) > 0 {
		return errors.New("left: " + strings.Join(m, " "))
	}
	return nil
}

// fromRecord is the key of a base built from that --from.
func fromRecord(p Paths, ref string) string {
	for _, k := range p.KeysOf("base") {
		if p.Record(k, "from") == ref {
			return k
		}
	}
	return ""
}

// toolField is a string field of a tool image's record (base: the base
// digest of a musl one; glibc: the newest glibc it needs).
func toolField(p Paths, file, field string) string {
	b, err := os.ReadFile(filepath.Join(p.Share, file+".json"))
	if err != nil {
		return ""
	}
	var rec map[string]any
	if json.Unmarshal(b, &rec) != nil {
		return ""
	}
	v, _ := rec[field].(string)
	return v
}

// toolsBuiltIn is the keys of the images the log's tool builds cloned.
func toolsBuiltIn(log string) []string {
	b, _ := os.ReadFile(log)
	var keys []string
	re := regexp.MustCompile(`^\+ limactl clone .* devvm-img-([0-9a-f]{12}) devvm-tools-build$`)
	for _, line := range strings.Split(string(b), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			keys = append(keys, m[1])
		}
	}
	return keys
}

// newerGlibc is whether glibc version need (major.minor) is newer than
// have.
func newerGlibc(need, have string) bool {
	n, h := strings.SplitN(need, ".", 3), strings.SplitN(have, ".", 3)
	for i := range 2 {
		var a, b int
		if i < len(n) {
			a, _ = strconv.Atoi(n[i])
		}
		if i < len(h) {
			b, _ = strconv.Atoi(h[i])
		}
		if a != b {
			return a > b
		}
	}
	return false
}

// fromDebian: a VM from node:24 (Debian 12) with node, python, Claude
// Code and the docker and nix layers.
func fromDebian(s *S, nd []string) {
	f, _ := s.NewProject("fromdocker")
	_ = os.WriteFile(filepath.Join(f, "package.json"), []byte("{}\n"), 0o644) //nolint:gosec // a project's file, as a user's
	// The Claude Code VM's home: no agent config of yours.
	fh := filepath.Join(s.sb.work, "from-home")
	madeFile(s.sb, fh)
	_ = os.MkdirAll(fh, 0o700)
	env := append([]string{"HOME=" + fh}, nd...)
	vm, launched, _ := launchWith(s, f, env, "--from", fromNode, "--network", "open", "--tools", "node,python", "--agents", "claude", "--layers", "docker,nix")
	fromlog := NewestLog(s.Paths)
	st := s.Paths.State(vm)
	files := strings.Fields(st["tools.made.files"])
	// Any VM may mount gnu tool images, so a --from image's code never runs
	// in their builds. They may be cached from an earlier run: then the log
	// has no build.
	s.Check("its tool images built in a clone of devvm's own Ubuntu base, never the Debian image's, and record no base", func() error {
		for _, k := range toolsBuiltIn(fromlog) {
			if s.Paths.Record(k, "name") != "base" || s.Paths.Record(k, "from") != "" {
				return errors.New("built in " + k)
			}
		}
		for _, f := range files {
			if b := toolField(s.Paths, f, "base"); b != "" {
				return errors.New(f + " records a base: " + b)
			}
		}
		return nil
	})
	s.Check("a VM with node, python, Claude Code and the docker and nix layers from node:24 (Debian 12) boots, its base keyed by its pinned digest", func() error {
		if vm == "" {
			return errors.Join(errors.New("no VM: the launch printed"), errorsOf(launched))
		}
		r, _ := s.Limactl("shell", "--workdir", "/", vm, "cat", "/etc/os-release")
		switch {
		case !r.Has("VERSION_CODENAME=bookworm"):
			return errors.New("not Debian 12")
		case st["from"] != fromNode || st["tools.made.libc"] != "gnu":
			return errors.New("its state: from=" + st["from"] + " tools.made.libc=" + st["tools.made.libc"])
		case s.Paths.Record(fromRecord(s.Paths, fromNode), "digest") != fromNode:
			return errors.New("its base isn't keyed by its pinned digest")
		}
		return nil
	})
	s.Check("its base records the image's glibc, as its VM does, and no tool image of the VM needs a newer one", func() error {
		r, err := s.Limactl("shell", "--workdir", "/", vm, "getconf", "GNU_LIBC_VERSION")
		g := strings.TrimPrefix(out(r), "glibc ")
		switch {
		case err != nil || g == "":
			return errors.New("no glibc version")
		case s.Paths.Record(fromRecord(s.Paths, fromNode), "glibc") != g:
			return errors.New("its base doesn't record glibc " + g)
		case s.Paths.State(vm)["tools.made.glibc"] != g:
			return errors.New("its state doesn't record glibc " + g)
		}
		for _, f := range files {
			if n := toolField(s.Paths, f, "glibc"); n != "" && newerGlibc(n, g) {
				return fmt.Errorf("%s needs glibc %s", f, n)
			}
		}
		return nil
	})
	for _, f := range files {
		n := toolField(s.Paths, f, "glibc")
		if n == "" {
			n = "none"
		}
		s.r.out.line("        glibc: %s needs %s", f, n)
	}
	fromDebianRuns(s, vm)
	_, _ = s.Run(Cmd{Args: []string{s.sb.devvm, "rm", "--force", "--no-export"}, Dir: f, Env: []string{"HOME=" + fh}})
}

// inVMCheck is a check that script passes in the VM's login shell.
func inVMCheck(s *S, title, vm, script string, opts ...CheckOpt) {
	s.Check(title, func() error {
		_, err := s.InVM(vm, script)
		return err
	}, opts...)
}

// gnuTar: the VM's commands run GNU tar, which devvm history import needs.
func gnuTar(s *S, vm string) error {
	_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `tar --version | head -1 | grep -q "GNU tar"`)
	return err
}

func fromDebianRuns(s *S, vm string) {
	inVMCheck(s, "its node, claude and every feature work", vm,
		`node --version && claude --version && eza --version && bat --version && fd --version && fzf --version && tldr --version && [[ -d ~/.oh-my-bash ]]`)
	py, cl := s.Paths.ToolVersion("python"), s.Paths.ToolVersion("claude")
	inVMCheck(s, "python, python3 and claude are the locked versions", vm,
		"[[ $(python --version) == 'Python "+py+"' && $(python3 --version) == 'Python "+py+"' && $(claude --version) == '"+cl+" '* ]]")
	s.Check("an interactive shell starts without errors", func() error {
		r, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-ic", "true")
		if err != nil {
			return err
		}
		return not(Matches(r.Out, `(?i)error|unknown option|not found`), "its shell printed an error")
	})
	s.Check("its commands run GNU tar, which devvm history import needs", func() error { return gnuTar(s, vm) })
	inVMCheck(s, "the docker layer's Docker runs in it (Docker's Debian repository), for the VM's user too", vm,
		`sudo docker run --rm hello-world | grep -q Hello && id -nG | grep -qw docker`, Needs(backend.FeatureDockerLayer))
	inVMCheck(s, "the nix layer's Nix runs in it (systemd): nix run nixpkgs#hello, and a build in Nix's sandbox", vm,
		`systemctl is-active --quiet nix-daemon.socket && nix run nixpkgs#hello | grep -qx 'Hello, world!' &&
  nix build --no-link --impure --expr '`+nixBuilt+`'`, Needs(backend.FeatureNixLayer))
}

// fromAlpineVM: Alpine: apk, OpenRC and musl, so its own (musl) tool
// images; the docker layer from Alpine's packages; the nix layer's daemon
// as an OpenRC service of devvm's; the desktop layer from Alpine's
// packages.
func fromAlpineVM(s *S, nd []string) {
	al, _ := s.NewProject("fromalpine")
	_ = os.WriteFile(filepath.Join(al, "package.json"), []byte("{}\n"), 0o644) //nolint:gosec // a project's file, as a user's
	_ = os.WriteFile(filepath.Join(al, "global.json"), []byte("{}\n"), 0o644)  //nolint:gosec // a project's file, as a user's
	vm, launched, _ := launchWith(s, al, nd, "--from", fromAlpine, "--network", "open", "--layers", "docker,nix,desktop")
	s.Check("an Alpine VM runs node from musl tool images", func() error {
		if vm == "" || s.Paths.State(vm)["tools.made.libc"] != "musl" || s.Paths.Record(fromRecord(s.Paths, fromAlpine), "libc") != "musl" {
			return errors.Join(errors.New("no Alpine VM on musl tool images: the launch printed"), errorsOf(launched))
		}
		return nil
	})
	alog := NewestLog(s.Paths)
	s.Check("its musl tool images built in its own base, and are keyed and recorded by that base's digest", func() error {
		b := fromRecord(s.Paths, fromAlpine)
		d := s.Paths.Record(b, "digest")
		if d == "" {
			return errors.New("no base with a digest")
		}
		for _, k := range toolsBuiltIn(alog) {
			if k != b {
				return errors.New("built in " + k)
			}
		}
		for _, f := range strings.Fields(s.Paths.State(vm)["tools.made.files"]) {
			if tb := toolField(s.Paths, f, "base"); tb != d {
				return errors.New(f + ": " + tb)
			}
		}
		return nil
	})
	inVMCheck(s, "and node, pnpm work there", vm,
		"grep -q '^ID=alpine' /etc/os-release && [[ $(node -v) == v"+s.Paths.ToolVersion("node")+` ]] && node -e 'require("os")' && pnpm -v`)
	inVMCheck(s, "and dotnet, its global.json's: the musl SDK, with ICU, makes and runs a console project", vm,
		"[[ $(dotnet --version) == "+s.Paths.ToolVersion("dotnet")+" ]] && cd /tmp && dotnet new console -o hello >/dev/null && [[ $(dotnet run --project hello) == 'Hello, World!' ]]")
	inVMCheck(s, "the docker layer's Docker runs in it (OpenRC)", vm, `sudo docker run --rm hello-world | grep -q Hello`, Needs(backend.FeatureDockerLayer))
	inVMCheck(s, "the desktop layer's programs are in it (Alpine's packages)", vm,
		`command -v Xvnc && command -v startxfce4 && command -v vncconfig && [[ -x /usr/local/bin/devvm-desktop ]]`)
	inVMCheck(s, "ports below 1024 are open to the VM's user after the setup (Alpine's kernel starts at 1024)", vm,
		`[[ $(cat /proc/sys/net/ipv4/ip_unprivileged_port_start) == 0 ]]`)
	inVMCheck(s, "and every feature", vm, `eza --version && bat --version && fd --version && fzf --version && tldr --version && [[ -d ~/.oh-my-bash ]]`)
	// As root: syslog's and acpid's pid files are root-only, so a user's
	// rc-status calls them crashed.
	inVMCheck(s, "no crashed services", vm, `[[ -z "$(sudo rc-status --crashed 2>/dev/null)" ]]`)
	inVMCheck(s, "the nix layer's Nix runs in it (OpenRC, musl): nix run nixpkgs#hello, and a build in Nix's sandbox", vm,
		`sudo rc-service nix-daemon status >/dev/null && nix run nixpkgs#hello | grep -qx 'Hello, world!' &&
  nix build --no-link --impure --expr '`+nixBuilt+`'`, Needs(backend.FeatureNixLayer))
	fromAlpineMore(s, nd, al, vm)
	_, _ = s.Devvm(al, "rm", "--force", "--no-export")
}

// fromAlpineMore: with network policy, OpenRC's service reads the proxy
// from its settings; devvm history import works on Alpine as on Ubuntu:
// base's Alpine branch installs GNU tar, which the import's listing and
// unpacking need (busybox's refuses their options), and the rest of its
// script runs on busybox's grep and find. busybox's grep -r over the home
// and /tmp runs past the check's limit there, so the refusal's check
// looks for the file by name, on the root's disk alone.
func fromAlpineMore(s *S, nd []string, al, vm string) {
	s.Needing(backend.FeatureNixLayer).Check("devvm network allowlist: after devvm restart, its nix-daemon goes through the proxy", func() error {
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "network", "allowlist"}, Dir: al, Env: nd}); err != nil {
			return err
		}
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "restart"}, Dir: al, Env: nd, Stdin: "exit\n"}); err != nil {
			return err
		}
		if s.Paths.State(vm)["net.made.check"] != "ok" {
			return errors.New("its self-check didn't pass")
		}
		// A download only the daemon makes, from a host that isn't
		// allowed, gets the proxy's 403.
		_, err := s.InVM(vm, `sudo grep -q "^export http_proxy=http://devvm:" /etc/conf.d/nix-daemon &&
    ! nix build --no-link --impure --expr '`+nixFetch+`' 2>/tmp/fetch.err && grep -q "response 403" /tmp/fetch.err`)
		return err
	})
	s.Check("an Alpine VM's commands run GNU tar", func() error { return gnuTar(s, vm) })
	export := filepath.Join(s.sb.work, "export")
	madeFile(s.sb, export)
	s.Check("history import puts an export into an Alpine VM", func() error {
		x := filepath.Join(export, "alpine")
		_ = os.RemoveAll(x)
		if err := os.MkdirAll(filepath.Join(x, ".claude", "projects", "e2e"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(x, ".claude", "projects", "e2e", "t.jsonl"), []byte("e2e-alpine\n"), 0o600); err != nil {
			return err
		}
		if _, err := s.Run(Cmd{Args: []string{"tar", "-czf", "../alpine.tar.gz", ".claude"}, Dir: x}); err != nil {
			return err
		}
		if _, err := s.Devvm(al, "history", "import", filepath.Join(export, "alpine.tar.gz")); err != nil {
			return err
		}
		_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", `[[ $(cat ~/.claude/projects/e2e/t.jsonl) == e2e-alpine ]]`)
		return err
	})
	s.Check("and an export with a path outside the agents' history imports nothing there", func() error {
		ev := filepath.Join(export, "evil")
		_ = os.RemoveAll(ev)
		if err := os.MkdirAll(filepath.Join(ev, "d"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ev, "e2e-escape"), []byte("e2e-escape\n"), 0o600); err != nil {
			return err
		}
		if _, err := s.Run(Cmd{Args: []string{"tar", "-czPf", "../evil.tar.gz", "../e2e-escape"}, Dir: filepath.Join(ev, "d")}); err != nil {
			return err
		}
		tgz := filepath.Join(ev, "evil.tar.gz")
		r, err := s.Devvm(al, "history", "import", tgz)
		switch {
		case err == nil:
			return errors.New("devvm history import went on")
		case !strings.Contains(r.Out, s.Msg("history_import_unsafe", "file="+tgz)):
			return errors.New("devvm history import didn't say it's unsafe")
		}
		o, err := s.Limactl("shell", "--workdir", "/", vm, "sudo", "find", "/home", "/root", "/tmp", "/etc", "-xdev", "-name", "e2e-escape")
		if err != nil {
			return err
		}
		return not(strings.TrimSpace(o.Stdout) != "", "imported: "+o.Stdout)
	})
}

// fromCorp: behind a company proxy with a password, a --from conversion
// (an Alpine release no other check uses, so it's converted each run)
// and the builds on it go through a proxy of devvm's for each build,
// which alone goes through the company proxy: the registry, base's and
// its features' downloads and a layer's own all reach it, while nothing a
// build runs sees its password, and no converted disk, image or log keeps
// it. The layer prints what its commands, and sudo's, have of the proxy
// settings.
func fromCorp(s *S) {
	corp, cerr := StartCorp(s)
	_, _ = s.Layer("e2ecorp.sh", "#!/bin/bash\nset -euo pipefail\n"+
		`echo "e2e-env: $(env | grep -i "_proxy=" | sort | tr "\n" " ")"`+"\n"+
		`echo "e2e-sudo: $(sudo env | grep -i "_proxy=" | sort | tr "\n" " ")"`+"\n"+
		"curl -fsS -m 30 -o /dev/null https://github.com\n")
	nd, _ := NoDocker(s)
	s.Check("behind a company proxy, --from's conversion and the builds on it reach it through devvm's own proxies", func() error {
		if cerr != nil {
			return cerr
		}
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "build", "--from", "alpine:3.21", "e2ecorp"}, Env: append(corp.Env(), nd...)}); err != nil {
			return err
		}
		switch {
		case !corp.Saw(`"event":"tunnel","host":"[a-z0-9.-]*docker\.io"`):
			return errors.New("the registry didn't go through the company proxy")
		case !corp.Saw(`"host":"dl-cdn\.alpinelinux\.org"`):
			return errors.New("Alpine's packages didn't go through the company proxy")
		case !corp.Saw(`"event":"tunnel","host":"github\.com"`):
			return errors.New("the layer's download didn't go through the company proxy")
		}
		return nil
	}, Builds())
	s.Own("e2ecorp")
	var kac, kcl string
	re := regexp.MustCompile(`^base=([0-9a-f]*) e2ecorp=.*$`)
	for _, k := range s.Paths.KeysOf("e2ecorp") {
		if m := re.FindStringSubmatch(s.Paths.Record(k, "chain")); m != nil && kac == "" {
			kac = m[1]
		}
	}
	if kac != "" {
		s.sb.created = append(s.sb.created, "devvm-img-"+kac)
	}
	if keys := s.Paths.KeysOf("e2ecorp"); len(keys) > 0 {
		kcl = keys[0]
	}
	// The run log writes a proxy key (32 to 64 hex digits) as <key>: its
	// stand-in there says the build's commands had one, and no key of
	// devvm's is left in the log.
	s.Check("what the builds' commands and sudo have is devvm's proxy for the build, with a password of its own", func() error { return corpLog(s) })
	s.Check("the company proxy's password is in no converted disk, image, log, build file or process's arguments", func() error {
		if cerr != nil {
			return cerr
		}
		img := func(k string) string { return filepath.Join(s.sb.lima, "devvm-img-"+k) }
		if kac == "" || kcl == "" || !Exists(img(kac)) || !Exists(img(kcl)) {
			return errors.New("no converted image")
		}
		if err := NoLeak([]string{corp.Password()}, img(kac), img(kcl), filepath.Join(s.Paths.Devvm, "logs"),
			filepath.Join(s.Paths.Devvm, "run"), filepath.Join(s.Paths.Devvm, "vms")); err != nil {
			return err
		}
		if have, err := ProcsHave(s, corp.Password()); err != nil || have {
			return errors.Join(errors.New("a process has the password in its arguments"), err)
		}
		if m, _ := filepath.Glob(filepath.Join(s.Paths.Devvm, "run", "builds", "*", "run", "build-proxy")); len(m) > 0 {
			return errors.New("left: " + strings.Join(m, " "))
		}
		return ociTempsGone()
	})
	if corp != nil {
		corp.Stop()
	}
}

// corpLog: the last run log with the layer's lines has devvm's proxy for
// the build, its key written <key>, and neither a key of devvm's nor the
// company proxy's port.
func corpLog(s *S) error {
	var log string
	for _, f := range s.Paths.RunLogs() {
		if b, err := os.ReadFile(f); err == nil && Matches(string(b), `^e2e-env: `) {
			log = string(b)
		}
	}
	switch {
	case log == "":
		return errors.New("no run log has the layer's lines")
	case !Matches(log, `^e2e-env: .*HTTPS_PROXY=http://devvm:<key>@192\.168\.5\.2:[0-9]+ `):
		return errors.New("the build's commands don't have devvm's proxy")
	case !Matches(log, `^e2e-sudo: .*HTTPS_PROXY=http://devvm:<key>@192\.168\.5\.2:[0-9]+ `):
		return errors.New("sudo's commands don't have devvm's proxy")
	case Matches(log, `devvm:[0-9a-f]{32,64}@`):
		return errors.New("a proxy key of devvm's is in the run log")
	case strings.Contains(log, ":"+strconv.Itoa(s.Port(corpPort))):
		return errors.New("the company proxy's port is in the run log")
	}
	return nil
}
