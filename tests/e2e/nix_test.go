//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
)

// nixpkgsRev is the nixpkgs a flake's dev shell takes its node from:
// nixos-26.05's, cached by Nix's servers.
const nixpkgsRev = "6f06d204d8f2d8761d9da78778745974a3216381"

// nixSection: the nix layer, built behind a company proxy, leaves no proxy
// settings for nix-daemon in its image; a node project with a flake gets
// Nix and direnv at their pins, a confined store and daemon behind the
// VM's proxy, the shared cache (a second VM's hits), the flake's dev shell
// through direnv, and the policy over the daemon's downloads, open too.
func nixSection(s *S) {
	s.Section("the nix layer")
	s = s.Needing(backend.FeatureNixLayer)
	nixImage(s)
	x, vm := nixProject(s)
	nixLaunchChecks(s, vm)
	nixCacheChecks(s, x, vm)
	nixShellChecks(s, x, vm)
	_, _ = s.Limactl("stop", vm)
}

// Behind a company proxy, the layer's script (as a layer of the run's own,
// so it builds each run) gets its build proxy's settings, which Nix's
// installer would write into nix-daemon's: none is anywhere on its disk.
// A layer of yours gets the prelude as nix.sh does.
func nixImage(s *S) {
	corp, cerr := StartCorp(s)
	f, lerr := s.Layer("e2enix.sh", readText(filepath.Join(s.sb.root, "assets", "layers", "nix.sh")))
	s.Check("built behind a company proxy, the layer leaves no proxy settings for nix-daemon in its image", func() error {
		if err := errors.Join(cerr, lerr); err != nil {
			return err
		}
		if _, err := s.Run(devvmCmd(s, "", "", corp.Env(), "build", "e2enix")); err != nil {
			return err
		}
		keys := s.Paths.KeysOf("e2enix")
		if len(keys) == 0 || !Exists(filepath.Join(s.sb.lima, "devvm-img-"+keys[0])) {
			return errors.New("no e2enix image")
		}
		if !corp.Saw(`releases\.nixos\.org`) {
			return errors.New("the build didn't reach releases.nixos.org through the company proxy")
		}
		return NoLeak([]string{"Environment=http_proxy=", "Environment=https_proxy=", corp.Password()}, filepath.Join(s.sb.lima, "devvm-img-"+keys[0]))
	}, Builds())
	s.Own("e2enix")
	if corp != nil {
		corp.Stop()
	}
	_ = os.Remove(f)
}

// nixProject is a node project with a flake: node, detected, and --layers
// nix, with network policy, launched; the shared Nix cache emptied first,
// so its VM fetches into it and a second VM gets hits. The flake's dev
// shell has a node of its own.
func nixProject(s *S) (string, string) {
	x, _ := s.NewProject("nixflake")
	_ = os.WriteFile(filepath.Join(x, "package.json"), []byte("{}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(x, ".envrc"), []byte("use flake\n"), 0o600)
	flake := `{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/` + nixpkgsRev + `";
  outputs = { self, nixpkgs }: {
    devShells = nixpkgs.lib.genAttrs [ "aarch64-linux" "x86_64-linux" ] (s: let pkgs = nixpkgs.legacyPackages.${s}; in {
      default = pkgs.mkShellNoCC { packages = [ (pkgs.writeShellScriptBin "node" "echo e2e-flake-node") ]; };
    });
  };
}
`
	_ = os.WriteFile(filepath.Join(x, "flake.nix"), []byte(flake), 0o600)
	_ = os.RemoveAll(nixCache(s))
	vm, res, _ := s.Launch(x, "exit", "--network", "allowlist", "--no-git-protect", "--layers", "nix")
	nv, dv := nixPin(s, "nix_version"), nixPin(s, "direnv_version")
	s.Check("a flake.nix and a package.json: a node VM with the nix layer and network policy, Nix and direnv at their pins", func() error {
		st := s.Paths.State(vm)
		switch {
		case vm == "":
			return errNo
		case !grepF(res.Out, s.Msg("new_vm_summary", "vm="+vm)) || !grepF(res.Out, s.Msg("new_vm_toolchain_for", "file=package.json", "tool=node")) ||
			!grepF(res.Out, s.Msg("new_vm_layer_from", "from=--layers", "layer=nix")):
			return errors.New("the launch didn't say what it made: its summary, node for package.json and nix from --layers")
		case st["image"] != "nix" || st["tools.names"] != "node" || st["net.tier"] != "allowlist" || st["net.made.check"] != "ok":
			return fmt.Errorf("its state: image %q, tools %q, tier %q, self-check %q", st["image"], st["tools.names"], st["net.tier"], st["net.made.check"])
		}
		return inVM(s, vm, `[[ $(nix --version) == "nix (Nix) `+nv+`" && $(direnv version) == `+dv+` ]]`)
	})
	return x, vm
}

// nixCache is the shared Nix cache on this machine.
func nixCache(s *S) string { return filepath.Join(s.Paths.Devvm, "cache", "nix-cache") }

// nixPin is the layer's pin name (nix_version) in assets/layers/nix.sh.
func nixPin(s *S, name string) string {
	m := regexp.MustCompile(`(?m)^` + name + `=(.*)$`).FindStringSubmatch(readText(filepath.Join(s.sb.root, "assets", "layers", "nix.sh")))
	if m == nil {
		return ""
	}
	return m[1]
}

func nixLaunchChecks(s *S, vm string) {
	s.Check("Nix's store is root's, builds are sandboxed, only root is trusted, and the user's own caches are ignored", func() error {
		//nolint:misspell // substituters is Nix's setting
		return inVM(s, vm, `
  [[ -z $(sudo find /nix/store -mindepth 1 -maxdepth 1 ! -user root -print -quit) ]] &&
  [[ $(nix config show sandbox) == true && $(nix config show sandbox-fallback) == false ]] &&
  [[ $(nix config show trusted-users) == root ]] &&
  nix store info --option substituters https://example.invalid 2>&1 | grep -q "ignoring untrusted substituter"`)
	})
	s.Check("nix-daemon gets the VM's proxy alone, and its NO_PROXY, in devvm's drop-in", func() error {
		return inVM(s, vm, `
  [[ $(ls /etc/systemd/system/nix-daemon.service.d) == devvm-proxy.conf && $NO_PROXY == localhost,127.0.0.1,::1 ]] &&
  e=$(systemctl show nix-daemon.service -p Environment --value) &&
  [[ $e == "http_proxy=$HTTP_PROXY https_proxy=$HTTP_PROXY no_proxy=$NO_PROXY" ]]`)
	})
	s.Check("the VM's Nix tries the shared cache first, then cache.nixos.org, trusting cache.nixos.org's key alone", func() error {
		//nolint:misspell // substituters is Nix's setting
		return inVM(s, vm, `
  [[ -f /etc/nix/devvm-cache.conf && $(nix config show substituters) == "http://devvm.proxy/nix https://cache.nixos.org"* ]] &&
  [[ $(nix config show trusted-public-keys) == "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=" ]]`)
	})
	s.Check("nix run nixpkgs#hello works with network policy, through the shared cache", func() error {
		return inVM(s, vm, `nix run nixpkgs#hello 2>/tmp/hello.err | grep -qx 'Hello, world!' && grep -q 'from .http://devvm.proxy/nix' /tmp/hello.err &&
   nix build --no-link github:NixOS/nixpkgs/`+nixpkgsRev+`#hello`)
	})
}

func nixCacheChecks(s *S, x, vm string) {
	cache := nixCache(s)
	ev := filepath.Join(s.Paths.VMDir(vm), "logs", "network.jsonl")
	s.Check("... which the host fetched into the shared cache, each file the user's alone", func() error {
		narinfo, _ := filepath.Glob(filepath.Join(cache, "*.narinfo"))
		nars, _ := filepath.Glob(filepath.Join(cache, "nar", "*.nar*"))
		switch {
		case !Matches(readText(ev), `"event":"nix-cache".*"reason":"fetched"`):
			return errors.New("no event shows a path fetched into the shared cache")
		case len(narinfo) == 0 || len(nars) == 0:
			return errors.New("the shared cache holds no narinfo or NAR")
		}
		return cacheFiles(cache, func(path string, d fs.DirEntry) error {
			if m := modeOf(path); (d.IsDir() && m != "700") || (!d.IsDir() && m != "600") {
				return fmt.Errorf("%s is mode %s", path, m)
			}
			return nil
		})
	})
	s.Check("a VM's writes to the shared cache, and its requests for anything else through it, are refused", func() error {
		return inVM(s, vm, `
  c() { curl -s -o /dev/null -w "%{http_code}" --proxy "$HTTP_PROXY" "$@"; }
  z=00000000000000000000000000000000
  [[ $(c -X PUT --data-binary x http://devvm.proxy/nix/$z.narinfo) == 400 ]] &&
  [[ $(c -X POST --data-binary x http://devvm.proxy/nix/nar/${z}00000000000000000000.nar.xz) == 400 ]] &&
  [[ $(c http://devvm.proxy/nix/https://example.com/x) == 400 ]] &&
  [[ $(c "http://devvm.proxy/nix/$z.narinfo?u=https://example.com") == 400 ]] &&
  [[ $(c --path-as-is http://devvm.proxy/nix/nar/../../etc/passwd) == 400 ]] &&
  [[ $(c http://devvm.proxy/nix/nar/${z}00000000000000000000.nar.xz) == 404 ]] &&
  [[ $(c http://devvm.proxy/nix/$z.narinfo) == 404 ]]`)
	})
	s.Check("... and the host kept none of them, asking cache.nixos.org alone", func() error {
		e := readText(ev)
		switch {
		case Exists(filepath.Join(cache, "00000000000000000000000000000000.narinfo")):
			return errors.New("the host kept the refused narinfo")
		case !Matches(e, `"path":"/nix/00000000000000000000000000000000.narinfo".*"reason":"not found: cache.nixos.org answered 404"`):
			return errors.New("no event shows cache.nixos.org asked for the narinfo")
		case !Matches(e, `"path":"/nix/nar/0*.nar.xz".*"reason":"not found: no checked narinfo names it"`):
			return errors.New("no event shows the NAR refused, no narinfo naming it")
		}
		return cacheFiles(cache, func(path string, d fs.DirEntry) error {
			if !d.IsDir() && !strings.HasPrefix(d.Name(), ".") && !strings.HasSuffix(d.Name(), ".narinfo") && !strings.Contains(path, "/nar/") {
				return errors.New("the shared cache keeps " + path)
			}
			return nil
		})
	})
	// A second VM of a Nix project gets what the first fetched from the
	// shared cache, as hits.
	y, _ := s.NewProject("nixflake2")
	for _, f := range []string{"flake.nix", "flake.lock", "package.json"} {
		if b, err := os.ReadFile(filepath.Join(x, f)); err == nil {
			_ = os.WriteFile(filepath.Join(y, f), b, 0o600)
		}
	}
	vm2, _, _ := s.Launch(y, "exit", "--network", "allowlist", "--no-git-protect", "--layers", "nix")
	s.Check("a second VM gets the paths the first fetched as hits from the shared cache, fetching none again", func() error {
		if vm2 == "" || vm2 == vm {
			return errors.New("no second VM")
		}
		if err := inVM(s, vm2, "nix build --no-link github:NixOS/nixpkgs/"+nixpkgsRev+"#hello 2>/tmp/hello.err && grep -q 'from .http://devvm.proxy/nix' /tmp/hello.err"); err != nil {
			return err
		}
		e := readText(filepath.Join(s.Paths.VMDir(vm2), "logs", "network.jsonl"))
		switch {
		case !Matches(e, `"event":"nix-cache",.*"path":"/nix/nar/[^"]*".*"reason":"hit"`):
			return errors.New("no event shows a hit")
		case Matches(e, `"event":"nix-cache".*"reason":"fetched"`):
			return errors.New("the second VM fetched a path again")
		}
		return nil
	})
	_, _ = s.Limactl("stop", vm2)
}

// cacheFiles runs fn on each file and folder of the shared cache, the
// cache's own folder among them: its first error.
func cacheFiles(cache string, fn func(string, fs.DirEntry) error) error {
	return filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return fn(path, d)
	})
}

// The nix expressions the checks build: one Nix's sandbox runs, and a
// fixed-output fetch only the daemon makes, from a host the policy
// doesn't allow.
const (
	nixBuilt = `derivation { name = "e2e-sandboxed"; system = builtins.currentSystem; builder = "/bin/sh"; args = [ "-c" "echo ok > $out" ]; }`
	nixFetch = `derivation { name = "e2e"; system = builtins.currentSystem; builder = "builtin:fetchurl"; url = "https://example.com/devvm-e2e"; outputHashMode = "flat"; outputHashAlgo = "sha256"; outputHash = "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="; }`
)

func nixShellChecks(s *S, x, vm string) {
	nv := nixPin(s, "nix_version")
	s.Check("a build runs in Nix's sandbox, which it would fail without", func() error {
		return inVM(s, vm, "nix build --no-link --impure --expr '"+nixBuilt+"'")
	})
	s.Check("sudo nix finds Nix, and so does a command over ssh", func() error {
		r, err := s.InVM(vm, "sudo nix --version")
		if err != nil || !HasLine(r.Stdout, "nix (Nix) "+nv) {
			return errors.New("sudo nix --version doesn't find Nix")
		}
		r, err = s.Run(Cmd{Args: []string{"ssh", "-F", filepath.Join(s.sb.lima, vm, "ssh.config"), "lima-" + vm, "nix --version"}})
		if err != nil || !HasLine(r.Stdout, "nix (Nix) "+nv) {
			return errors.New("a command over ssh doesn't find Nix")
		}
		return nil
	})
	// An interactive shell, its commands on its input, as a terminal's:
	// direnv's and mise's hooks run before each prompt.
	sh, _ := s.Run(Cmd{
		Args: []string{"limactl", "shell", "--workdir", "/", vm, "bash", "-i"},
		Stdin: strings.Join([]string{
			"cd " + x, "direnv allow", "cd /", "cd " + x, `echo "e2e-a=$(node -v)"`, "true", `echo "e2e-b=$(node -v)"`,
			"cd /", `echo "e2e-c=$(node -v)"`,
		}, "\n") + "\n",
	})
	s.Check("the flake's dev shell, through direnv, comes before the shared node, prompt after prompt, and goes", func() error {
		if !HasLine(sh.Stdout, "e2e-a=e2e-flake-node") || !HasLine(sh.Stdout, "e2e-b=e2e-flake-node") || !HasLine(sh.Stdout, "e2e-c=v"+s.Paths.ToolVersion("node")) {
			return errors.New("the shell's node isn't the flake's in the project, then the shared one:\n" + tailLines(sh.Out, 20))
		}
		return nil
	})
	s.Check("nix-direnv's cache is in the VM's home, not the project folder", func() error {
		if Exists(filepath.Join(x, ".direnv")) || !Exists(filepath.Join(x, "flake.lock")) {
			return errors.New("the project folder has a .direnv, or no flake.lock")
		}
		return inVM(s, vm, "ls -d ~/.cache/direnv/layouts/*/flake-profile-*")
	})
	s.Check("a download from a host that isn't allowed fails, and devvm-why names it", func() error {
		return inVM(s, vm, "! nix build --no-link --impure --expr '"+nixFetch+"' 2>/tmp/fetch.err && grep -q 'response 403' /tmp/fetch.err &&\n  devvm-why | grep -q 'devvm network allow example.com'")
	})
	// Open, it keeps its proxy, which applies no rules: nix-daemon still
	// goes through it, so the host the allowlist refused is reached, and
	// the shared cache stays first.
	s.Check("devvm network open: from the next start it's open, and nix-daemon still goes through its proxy", func() error {
		if _, err := s.Devvm(x, "network", "open"); err != nil {
			return err
		}
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "restart"}, Dir: x, Stdin: "exit\n"}); err != nil {
			return err
		}
		if r, _ := s.Devvm(x, "network"); FirstLine(r.Stdout) != "open" {
			return errors.New("devvm network doesn't say open")
		}
		return inVM(s, vm, `systemctl show nix-daemon.service -p Environment --value | grep -q "http_proxy=http://devvm:[0-9a-f]*@192\.168\.5\.2:"`)
	})
	s.Check("... which, open, no longer refuses the host it did", func() error {
		return inVM(s, vm, "nix build --no-link --impure --expr '"+nixFetch+"' 2>/tmp/fetch.err || ! grep -q 'response 403' /tmp/fetch.err")
	})
	s.Check("... and the shared cache stays first", func() error {
		return inVM(s, vm, `[[ -e /etc/nix/devvm-cache.conf && $(nix config show substituters) == http://devvm.proxy/nix* ]]`) //nolint:misspell // substituters is Nix's setting
	})
}
