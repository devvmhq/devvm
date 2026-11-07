//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/platform"
)

// sandbox is a slot's: its Lima home, state folder, Keychain service and
// lock. Slot 0's are ~/.lt-e2e-go (or $DEVVM_E2E_LIMA_HOME),
// /tmp/devvm-go-e2e and devvm-go-e2e; slot K's add -K to each, and
// --newest-base's -newest to slot 0's.
type sandbox struct {
	o       options
	root    string // the checkout
	work    string // the state folder: XDG folders, projects, logs
	lima    string // the Lima home, devvm's and limactl's
	service string // the Keychain service, fake tokens only
	ports   int    // added to each of slot 0's fixed ports
	devvm   string // devvm, built from the checkout
	env     []string
	tdir    string // the timings, slot 0's for every slot

	created  []string // VMs this run made: the only ones it deletes
	kept     []string // VMs --reuse keeps for the next run
	made     []string // files and folders this run made: deleted at its end
	projects []string // project folders this run made, unlocked and deleted at its end
	deferred []func(context.Context)
	ca       *CA // the run's CA, made the first time a section asks (NewCA)
}

// The sandbox's names.
const (
	workBase    = "/tmp/devvm-go-e2e"
	limaBase    = ".lt-e2e-go"
	serviceBase = "devvm-go-e2e"
)

// e2eUbuntuRelease is the dated release of Ubuntu's cloud image that base
// starts from here, in place of the newest a user's devvm pins, so base
// stays cached between runs (scripts/bump.sh e2e-ubuntu moves it). devvm
// still checks its signed checksums.
const e2eUbuntuRelease = "release-20261016"

// slotWork, slotLima and slotService are slot k's state folder, Lima home
// and Keychain service.
func slotWork(k int) string { return suffixed(workBase, k) }

func slotLima(k int) string {
	h := os.Getenv("DEVVM_E2E_LIMA_HOME")
	if h == "" {
		h = filepath.Join(os.Getenv("HOME"), limaBase)
	}
	return suffixed(h, k)
}

func slotService(k int) string { return suffixed(serviceBase, k) }

func suffixed(s string, k int) string {
	if k == 0 {
		return s
	}
	return s + "-" + strconv.Itoa(k)
}

// newSandbox is the slot's sandbox, its environment made, nothing on disk
// changed yet.
func newSandbox(o options) (*sandbox, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	sb := &sandbox{
		o: o, root: root, work: slotWork(o.slot), lima: slotLima(o.slot), service: slotService(o.slot),
		ports: 100 * o.slot, tdir: filepath.Join(slotWork(0), "timings", o.tier()),
	}
	if o.newest {
		sb.work, sb.lima, sb.service, sb.ports = sb.work+"-newest", sb.lima+"-newest", sb.service+"-newest", 1000
	}
	sb.devvm = filepath.Join(sb.work, "src-go", "devvm")
	set := map[string]string{
		"DEVVM_LIMA_HOME": sb.lima, "LIMA_HOME": sb.lima, "DEVVM_SECRET_SERVICE": sb.service,
		"XDG_DATA_HOME": sb.data(), "XDG_CONFIG_HOME": sb.config(),
		// Never checked for newer shared tools but by the tools section.
		"DEVVM_NO_TOOL_CHECK": "1",
	}
	if !o.newest {
		set["DEVVM_UBUNTU_RELEASE"] = e2eUbuntuRelease
	}
	sb.env = environ(os.Environ(), set)
	return sb, nil
}

// environ is env with what e2e never inherits taken out and set's
// variables in their place. Your SSH agent is never reached, devvm's
// output stays plain, and no setting of yours picks its messages, mise,
// Ubuntu's mirror or --from.
func environ(env []string, set map[string]string) []string {
	drop := []string{
		"DEVVM_MESSAGES", "DEVVM_UBUNTU_URL", "DEVVM_UBUNTU_RELEASE", "DEVVM_HOST_MISE", "CLAUDE_CODE_OAUTH_TOKEN",
		"DEVVM_FROM", "SSH_AUTH_SOCK", "FORCE_COLOR", "CLICOLOR_FORCE",
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; ok || slices.Contains(drop, k) {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}

// repoRoot is the checkout: the first folder up from here with devvm's
// go.mod (go test runs a package's tests in its folder).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(b, []byte("module github.com/devvmhq/devvm\n")) {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("run it in devvm's checkout (no go.mod of devvm's above this folder)")
		}
		dir = up
	}
}

// The sandbox's folders and files.
func (sb *sandbox) data() string     { return filepath.Join(sb.work, "data") }
func (sb *sandbox) config() string   { return filepath.Join(sb.work, "config") }
func (sb *sandbox) devvmDir() string { return filepath.Join(sb.data(), "devvm") }
func (sb *sandbox) images() string   { return filepath.Join(sb.devvmDir(), "images") }
func (sb *sandbox) share() string    { return filepath.Join(sb.devvmDir(), "tools") }
func (sb *sandbox) layers() string   { return filepath.Join(sb.config(), "devvm", "layers") }
func (sb *sandbox) toolLock() string { return filepath.Join(sb.devvmDir(), "mise.lock") }
func (sb *sandbox) logFile() string  { return filepath.Join(sb.work, "check.log") }
func (sb *sandbox) lockFile() string { return sb.work + ".lock" }

// lock takes the slot's lock, a symlink to the pid of the run holding it:
// one run at a time in a slot, taking over a lock whose process has
// ended.
func (sb *sandbox) lock() error {
	if err := os.MkdirAll(sb.work, 0o700); err != nil {
		return err
	}
	return takeLock(sb.lockFile())
}

// unlock lets the slot's lock go, when it's this run's.
func (sb *sandbox) unlock() { dropLock(sb.lockFile()) }

// setup gets the sandbox ready before the first section: never your real Lima home, devvm built from the checkout with
// its files embedded (so editing the repo mid-run is harmless), base's
// features chosen in config.toml so nothing asks, and the pinned tool
// lock in place. With --clean, the cached images go first.
func (sb *sandbox) setup(ctx context.Context, out *output) error {
	if err := sb.refuseRealLima(); err != nil {
		return err
	}
	for _, d := range []string{sb.lima, sb.layers(), sb.data()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(sb.devvmDir(), 0o700); err != nil {
		return err
	}
	if err := sb.build(ctx); err != nil {
		return err
	}
	if err := sb.writeConfig(); err != nil {
		return err
	}
	if err := sb.pinLock(); err != nil {
		return err
	}
	if sb.o.clean {
		sb.clean(ctx, out)
	}
	return nil
}

// refuseRealLima refuses a Lima home that's ~/.lima or devvm's own.
func (sb *sandbox) refuseRealLima() error {
	if err := os.MkdirAll(sb.lima, 0o700); err != nil {
		return err
	}
	mine, err := filepath.EvalSymlinks(sb.lima)
	if err != nil {
		return err
	}
	home := os.Getenv("HOME")
	for _, yours := range []string{filepath.Join(home, ".lima"), filepath.Join(home, ".local", "share", "devvm", "lima")} {
		if r, err := filepath.EvalSymlinks(yours); err == nil {
			yours = r
		}
		if mine == yours {
			return fmt.Errorf("refusing to use your real Lima home (%s)", yours)
		}
	}
	return nil
}

// build makes devvm from the checkout, a test build, into the sandbox:
// with go, else mise's.
func (sb *sandbox) build(ctx context.Context) error {
	if err := os.RemoveAll(filepath.Dir(sb.devvm)); err != nil {
		return err
	}
	args := []string{"build", "-ldflags", "-X github.com/devvmhq/devvm/internal/testbuild.testBuild=yes", "-o", sb.devvm, "./cmd/devvm"}
	name := "go"
	if _, err := exec.LookPath("go"); err != nil {
		name, args = "mise", append([]string{"exec", "--", "go"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = sb.root, append(slices.Clone(sb.env), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't build devvm: %w\n%s", err, b)
	}
	sb.made = append(sb.made, filepath.Dir(sb.devvm))
	return nil
}

// writeConfig chooses base's features: every one in the features tier,
// none otherwise, so only a run that checks them builds them.
func (sb *sandbox) writeConfig() error {
	var fs []string
	if sb.o.every {
		m, err := filepath.Glob(filepath.Join(sb.root, "assets", "features", "*.sh"))
		if err != nil {
			return err
		}
		for _, f := range m {
			fs = append(fs, strconv.Quote(strings.TrimSuffix(filepath.Base(f), ".sh")))
		}
	}
	return os.WriteFile(filepath.Join(sb.config(), "devvm", "config.toml"), []byte("features = ["+strings.Join(fs, ", ")+"]\n"), 0o600)
}

// pinLock puts tests/e2e-mise.lock in place as the shared tools' lock, so
// the tool images built for it stay cached. Its first line is the SHA-256
// of the shared-tools/mise.toml it was resolved from: for another, the run
// stops here.
func (sb *sandbox) pinLock() error {
	lock, err := os.ReadFile(filepath.Join(sb.root, "tests", "e2e-mise.lock"))
	if err != nil {
		return err
	}
	toml, err := os.ReadFile(filepath.Join(sb.root, "assets", "shared-tools", "mise.toml"))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(toml)
	first, _, _ := strings.Cut(string(lock), "\n")
	if first != "# devvm e2e: shared-tools/mise.toml sha256:"+hex.EncodeToString(sum[:]) {
		return errors.New("tests/e2e-mise.lock isn't resolved from this assets/shared-tools/mise.toml: run scripts/bump.sh e2e-tools")
	}
	if err := os.WriteFile(sb.toolLock(), lock, 0o600); err != nil {
		return err
	}
	return os.WriteFile(sb.toolLock()+".toml", toml, 0o600)
}

// clean deletes the sandbox's cached images and tool images.
func (sb *sandbox) clean(ctx context.Context, out *output) {
	out.line("")
	out.line("== cleaning cached images")
	if b, err := sb.command(ctx, "limactl", "list", "-q").Output(); err == nil {
		for n := range strings.FieldsSeq(string(b)) {
			if strings.HasPrefix(n, "devvm-img-") {
				_ = sb.command(ctx, "limactl", "delete", "-f", n).Run()
			}
		}
	}
	_ = os.RemoveAll(sb.images())
	_ = os.RemoveAll(sb.share())
	if sb.o.newest {
		// A newest base's packages come from the mirrors, not an earlier
		// run's cache.
		_ = os.RemoveAll(filepath.Join(sb.devvmDir(), "packages"))
	}
}

// command is a host command in the sandbox's environment, run from its
// state folder (Lima writes lima.REJECTED.yaml into the current one).
func (sb *sandbox) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = sb.env, sb.work
	return cmd
}

// cleanup deletes what the run made: its VMs (an image's record too) and
// their folders, its files, its project folders (unlocked first on
// macOS), the sandbox's forwarders and devvm's own SSH agent, and what
// each section deferred. A VM --reuse keeps stays.
func (sb *sandbox) cleanup(ctx context.Context, out *output) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for i := len(sb.deferred) - 1; i >= 0; i-- {
		sb.deferred[i](ctx)
	}
	for _, n := range sb.created {
		if slices.Contains(sb.kept, n) {
			continue
		}
		_ = sb.command(ctx, "limactl", "delete", "-f", n).Run()
		if k, ok := strings.CutPrefix(n, "devvm-img-"); ok && k != "" {
			_ = os.RemoveAll(filepath.Join(sb.images(), k))
		}
		_ = os.RemoveAll(sb.paths().VMDir(n))
	}
	for _, f := range sb.made {
		_ = os.RemoveAll(f)
	}
	for _, p := range sb.projects {
		unlock(ctx, p)
		_ = os.RemoveAll(p)
	}
	_ = sb.command(ctx, "pkill", "-f", "[s]sh-agent -a "+filepath.Join(sb.devvmDir(), "agent.sock")).Run()
	_ = sb.command(ctx, "pkill", "-f", "[_]ports -vm .* -lifeline "+sb.lima+"/").Run()
	if len(sb.kept) > 0 {
		out.line("e2e: kept for --reuse: %s", strings.Join(sb.kept, " "))
	}
}

// unlock takes .git's lock off a project folder, so it can go: on macOS
// with chflags, on Linux with devvm-immutable, where it's installed.
func unlock(ctx context.Context, dir string) {
	if runtime.GOOS == "darwin" {
		_ = exec.CommandContext(ctx, "chflags", "-R", "nouchg", dir).Run()
		return
	}
	if _, err := os.Stat(dir); err == nil {
		_ = platform.Locker{Helper: platform.ImmutableHelper}.Chflags(ctx, "nouchg", dir)
	}
}

// tool is a host command the chosen sections use, with what installs it:
// Homebrew's package, Debian's and Fedora's ("-" where there's none).
type tool struct{ cmd, brew, apt, dnf string }

// baseTools are what every run uses: Lima's limactl, and pkill for the
// sandbox's forwarders at the end.
func baseTools() []tool {
	return []tool{
		{"limactl", "lima", "", ""},
		{"pkill", "proctools", "procps", "procps-ng"},
	}
}

// checkTools names every host command the chosen sections use that this
// machine lacks, each with what installs it, before anything is built.
func (sb *sandbox) checkTools(ctx context.Context, sel []string) error {
	if len(sel) == 0 {
		return errors.New("no section chosen")
	}
	need := baseTools()
	for _, s := range sections() {
		if sb.o.wants(s.name) {
			need = append(need, s.tools...)
		}
	}
	var missing []string
	for _, t := range need {
		if _, err := exec.LookPath(t.cmd); err != nil {
			missing = append(missing, "  "+t.cmd+": "+installs(t))
		} else if t.cmd == "openssl" {
			missing = append(missing, libreSSL(ctx, t)...)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errors.New("the chosen sections need these, which this machine lacks:\n" + strings.Join(slices.Compact(missing), "\n"))
}

// libreSSL is what to do when the openssl first on PATH isn't OpenSSL's:
// the CA checks' HTTPS server is openssl s_server, and LibreSSL's (macOS's
// /usr/bin/openssl) cuts the VM's OpenSSL client off mid-request.
func libreSSL(ctx context.Context, t tool) []string {
	b, _ := exec.CommandContext(ctx, "openssl", "version").Output()
	v := strings.TrimSpace(string(b))
	if strings.HasPrefix(v, "OpenSSL") {
		return nil
	}
	fix := installs(t)
	if runtime.GOOS == "darwin" {
		fix = "brew install openssl@3, then put $(brew --prefix openssl@3)/bin first on PATH"
	}
	return []string{"  openssl (OpenSSL, not " + v + "): " + fix}
}

// installs is the command that installs t with this system's package
// manager.
func installs(t tool) string {
	switch {
	case t.cmd == "limactl" && runtime.GOOS != "darwin":
		return "Lima's release tarball (https://lima-vm.io/docs/installation/)"
	case runtime.GOOS == "darwin":
		return "brew install " + t.brew
	}
	if _, err := exec.LookPath("apt-get"); err == nil {
		return "sudo apt-get install " + t.apt
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return "sudo dnf install " + t.dnf
	}
	return "your distribution's package for it (" + t.apt + " on Debian, " + t.dnf + " on Fedora)"
}

// lastLine is f's last line, its carriage returns and terminal escapes
// taken out, at most 100 characters: what the heartbeat shows of it.
func lastLine(f string) string {
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	var last string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		last = sc.Text()
	}
	last = escapes().ReplaceAllString(strings.ReplaceAll(last, "\r", ""), "")
	if r := []rune(last); len(r) > 100 {
		last = string(r[:100])
	}
	return last
}

// escapes matches a terminal's escape sequences.
func escapes() *regexp.Regexp { return regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`) }
