package vm

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// opencodeService is a service.json of this machine's opencode, as opencode v2.4.1's
// service-config.ts writes it: its service's password, with remote access on, the secret
// route it's served on, and its consent to remote access.
const opencodeService = `{"password": "testfake-service-password", "remote": {"route": "testfakeroute0001"}, "remoteAccepted": true}`

// opencodeServiceFiles are this machine's opencode service settings in its config folder: the
// default channels', another channel's, and one's temporary file.
func opencodeServiceFiles() map[string]string {
	return map[string]string{
		"service.json":           opencodeService,
		"service-local.json":     opencodeService,
		"service.json.4242.tmp":  opencodeService,
		"opencode.json":          `{"theme": "dark"}`,
		"agent/reviewer.md":      "# reviewer",
		"service-notes/notes.md": "a folder that's no service setting",
	}
}

// devvm agent import never copies opencode's service settings into the store: they hold this
// machine's service password and remote route, and each VM's opencode makes its own. Its other
// files and folders are copied, and leaving the service's out warns of nothing.
func TestOpencodeServiceNotImported(t *testing.T) {
	im := newImporter(t)
	folder := filepath.Join(im.Dirs.Home, agents.OpencodeConfig)
	write(t, folder, opencodeServiceFiles())
	if err := im.Run(t.Context(), []string{agents.Opencode}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, agents.Opencode), agents.OpencodeConfig)
	for _, n := range []string{"service.json", "service-local.json", "service.json.4242.tmp"} {
		if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied into the store: %v", n, err)
		}
		if slices.ContainsFunc(im.got, func(s string) bool { return strings.Contains(s, n) }) {
			t.Errorf("a message names %s: %q", n, im.got)
		}
	}
	for _, n := range []string{"opencode.json", "agent/reviewer.md", "service-notes/notes.md"} {
		if _, err := os.Stat(filepath.Join(store, n)); err != nil {
			t.Errorf("%s not copied: %v", n, err)
		}
	}
}

// A store that an earlier devvm agent import filled holds opencode's service settings, but no
// VM's setup gets them, and the setup gets the paths of each machine's own files that it takes
// out of the VM when they're devvm's copy.
func TestOpencodeServiceNotStaged(t *testing.T) {
	d := paths.Dirs{Config: t.TempDir()}
	store := agentStore(d, agents.Opencode)
	write(t, filepath.Join(store, agents.OpencodeConfig), opencodeServiceFiles())
	s := &stage{}
	if _, err := (syncer{Env: &Env{Dirs: d}}).stageAgents(s, []string{agents.Opencode}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range s.files {
		names = append(names, f.name)
		if bytes.Contains(f.data, []byte("testfake")) {
			t.Errorf("staged %s holds the service's password or route", f.name)
		}
	}
	if !slices.Contains(names, "home/"+agents.OpencodeConfig+"/opencode.json") ||
		slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, "/service") }) {
		t.Errorf("staged %q", names)
	}
	want := strings.Join(agents.MachineOwnFiles(), "\n") + "\n"
	if i := s.index("machine-own"); i < 0 || string(s.files[i].data) != want || !strings.Contains(want, agents.OpencodeConfig+"/service.json\n") {
		t.Errorf("machine-own isn't %q: %+v", want, s.files)
	}
}

// devvm agent save never takes the VM's own opencode service settings, which its opencode made
// with the VM's own password: the VM's stream leaves them out, so the save goes on, and the
// host refuses them as no setting when a VM sends them anyway.
func TestOpencodeServiceNotSaved(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	home := t.TempDir()
	write(t, filepath.Join(home, agents.OpencodeConfig), opencodeServiceFiles())
	cmd := exec.CommandContext(t.Context(), "bash", "-c", agentSaveScript([]string{agents.Opencode})) //nolint:gosec // devvm's own script
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "COPYFILE_DISABLE=1"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if bytes.Contains(out, []byte("testfake")) {
		t.Error("the stream holds the service's password or route")
	}
	s, err := readSent(bytes.NewReader(out), "vm", []string{agents.Opencode})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := s.entries[agents.OpencodeConfig+"/opencode.json"]; !ok {
		t.Errorf("entries = %v", s.entries)
	}
	for _, n := range []string{"service.json", "service-local.json", "service.json.4242.tmp"} {
		if _, _, _, ok := listedPath([]string{agents.Opencode}, agents.OpencodeConfig+"/"+n, false); ok {
			t.Errorf("%s is a setting a save takes", n)
		}
	}
}

// Each start takes opencode's service settings out of a store that an earlier devvm agent import
// filled, says so once, and leaves its other files; a store without them warns of nothing.
func TestCleanStoreOpencodeService(t *testing.T) {
	d := paths.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	folder := filepath.Join(agentStore(d, agents.Opencode), agents.OpencodeConfig)
	write(t, folder, opencodeServiceFiles())
	var warned msg.Recorder
	e := &Env{Dirs: d, Reporter: &msg.Fake{OnWarn: warned.Add}}
	if err := e.cleanStore(t.Context(), []string{agents.Opencode}); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent_store_own_cleaned agent=opencode dir=" + folder + " entries=service-local.json, service.json, service.json.4242.tmp"}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
	for n := range opencodeServiceFiles() {
		_, err := os.Lstat(filepath.Join(folder, n))
		if gone := errors.Is(err, os.ErrNotExist); gone != agents.MachineOwn(agents.OpencodeConfig+"/"+n) {
			t.Errorf("%s gone: %v (%v)", n, gone, err)
		}
	}
	warned.Reset()
	if err := e.cleanStore(t.Context(), []string{agents.Opencode}); err != nil || len(warned.Names()) > 0 {
		t.Errorf("again: %v, warned %q", err, warned.Names())
	}
}

// The setup takes out of the VM a file of each machine's own that holds devvm's placeholder: a
// copy of this machine's opencode service settings that an earlier devvm gave it, with the
// password every VM had, so its opencode makes its own. The VM's own files, a link and a folder
// stay, and so does everything when the placeholder is unknown.
func TestSyncCleansMachineOwn(t *testing.T) {
	home := t.TempDir()
	redacted := `{"password": "` + agentlogin.RedactMark + `", "remote": {"route": "testfakeroute0001"}}`
	write(t, filepath.Join(home, agents.OpencodeConfig), map[string]string{
		"service.json":          redacted,
		"service-local.json":    `{"password": "the-vms-own"}`,
		"service-beta.json/x":   redacted,
		"opencode.json":         redacted,
		"elsewhere/target.json": redacted,
	})
	folder := filepath.Join(home, agents.OpencodeConfig)
	if err := os.Symlink(filepath.Join(folder, "elsewhere/target.json"), filepath.Join(folder, "service-link.json")); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(t.TempDir(), "machine-own")
	if err := os.WriteFile(list, []byte(strings.Join(agents.MachineOwnFiles(), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fn := "clean_machine_own() {\n" + between(t, "guest/sync.sh", "clean_machine_own() {", "}") + "\n}\n"
	run := func(mark string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", "-c", fn+`clean_machine_own "$1"`, "bash", list) //nolint:gosec // sync.sh's own function, on the test's files
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "DEVVM_REDACTED=" + mark}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("")
	if _, err := os.Stat(filepath.Join(folder, "service.json")); err != nil {
		t.Errorf("without the placeholder, service.json went: %v", err)
	}
	run(agentlogin.RedactMark)
	if _, err := os.Lstat(filepath.Join(folder, "service.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("devvm's copy stayed: %v", err)
	}
	for _, n := range []string{"service-local.json", "service-beta.json/x", "opencode.json", "service-link.json", "elsewhere/target.json"} {
		if _, err := os.Lstat(filepath.Join(folder, n)); err != nil {
			t.Errorf("%s went: %v", n, err)
		}
	}
	// A copy that can't be taken out, in a folder the VM's user can't write, fails the setup
	// with the cause, rather than leave the shared password in place without a word. root
	// writes any folder, so it's not a case there.
	if os.Geteuid() == 0 {
		return
	}
	if err := os.WriteFile(filepath.Join(folder, "service.json"), []byte(redacted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(folder, 0o500); err != nil { //nolint:gosec // a folder of the test's own, read-only on purpose
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o700) })                                                //nolint:gosec // its own folder, writable again so the test can delete it
	cmd := exec.CommandContext(t.Context(), "bash", "-c", fn+`clean_machine_own "$1"`, "bash", list) //nolint:gosec // sync.sh's own function, on the test's files
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "DEVVM_REDACTED=" + agentlogin.RedactMark}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "can't be taken out") {
		t.Errorf("a copy that can't be taken out: %v, %q", err, out)
	}
}
