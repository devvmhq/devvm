package sshagent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
)

// fakeAgent stands in for ssh-agent, ssh-add and ssh-keygen: a key file's
// contents stand for the key, held as fingerprint SHA256:fp-<contents>.
type fakeAgent struct {
	up    bool
	held  []string // the contents of the keys it holds
	calls []string // ssh-agent and ssh-add calls, a key as key:<contents>
}

func (f *fakeAgent) run(_ context.Context, _ bool, argv ...string) (string, int) {
	switch argv[0] {
	case "ssh-agent":
		f.calls = append(f.calls, strings.Join(argv, " "))
		f.up = true
		return "", 0
	case "ssh-keygen":
		data, err := os.ReadFile(argv[len(argv)-1])
		if err != nil || len(data) == 0 {
			return "", 1
		}
		return "256 SHA256:fp-" + strings.TrimSpace(string(data)) + " c (ED25519)\n", 0
	}
	if !f.up {
		return "", 2
	}
	if argv[1] == "-l" {
		if len(f.held) == 0 {
			return "The agent has no identities.\n", 1
		}
		var b strings.Builder
		for _, k := range f.held {
			b.WriteString("256 SHA256:fp-" + k + " " + k + " comment (ED25519)\n")
		}
		return b.String(), 0
	}
	if argv[1] == "-L" { // a key's contents stand for its public key too
		return strings.Join(f.held, "\n") + "\n", 0
	}
	data, err := os.ReadFile(argv[len(argv)-1])
	if err != nil {
		return "", 1
	}
	k := strings.TrimSpace(string(data))
	f.calls = append(f.calls, strings.Join(append(slices.Clone(argv[:len(argv)-1]), "key:"+k), " "))
	if argv[1] == "-d" {
		f.held = slices.DeleteFunc(f.held, func(h string) bool { return h == k })
		return "", 0
	}
	f.held = append(f.held, k)
	return "", 0
}

type said struct{ lines []string }

func (s *said) add(prefix string) func(msg.M) {
	return func(m msg.M) { s.lines = append(s.lines, prefix+m.Names()) }
}

// ui is the agent's UI, keeping what it prints in s, by name, with no
// run log.
func (s *said) ui() *msg.Fake {
	return &msg.Fake{OnInfo: s.add("==> "), OnNote: s.add(""), OnWarn: s.add("devvm: ")}
}

// testAgent is devvm's agent on a fake, in a sandbox, with no VMs unless
// mounts is set; tmp is a folder outside them for keys.
func testAgent(t *testing.T) (a *Agent, f *fakeAgent, out *said, tmp string, mounts *[]Mount) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, out, mounts = &fakeAgent{}, &said{}, new([]Mount)
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	a = New(d, func(context.Context) ([]Mount, error) { return *mounts, nil }, out.ui())
	a.Run = f.run
	a.Getenv = func(string) string { return "" }
	a.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	tmp = filepath.Join(root, "keys")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	return a, f, out, tmp, mounts
}

func writeKey(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readKeys(t *testing.T, a *Agent) []AddedKey {
	t.Helper()
	keys, err := a.added()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func saveKeys(t *testing.T, a *Agent, keys ...AddedKey) {
	t.Helper()
	if err := a.saveAdded(keys); err != nil {
		t.Fatal(err)
	}
}

// agent-keys.json holds each key's file, fingerprint and options as JSON:
// a missing file has none, and a key devvm can't use is refused by name.
func TestAddedKeysFile(t *testing.T) {
	a, _, _, _, _ := testAgent(t)
	if got := readKeys(t, a); got != nil {
		t.Fatalf("no file: %+v", got)
	}
	want := []AddedKey{
		{File: "/k/deploy key", Fingerprint: "SHA256:fp-deploy"},
		{File: "/k/other", Fingerprint: "SHA256:fp-other", AddOptions: AddOptions{Confirm: true, Lifetime: "30m"}},
	}
	saveKeys(t, a, want...)
	data, err := os.ReadFile(a.Dirs.AgentKeys())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"file": "/k/other",`) || !strings.Contains(string(data), `"confirm": true,`) ||
		!strings.Contains(string(data), `"lifetime": "30m"`) || strings.Contains(string(data), `"confirm": false`) {
		t.Errorf("file:\n%s", data)
	}
	if got := readKeys(t, a); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, bad := range []AddedKey{{File: "rel"}, {File: "/k", AddOptions: AddOptions{Lifetime: "soon"}}} {
		if err := a.saveAdded([]AddedKey{bad}); err == nil {
			t.Errorf("saveAdded(%+v): want an error", bad)
		}
	}
	for _, data := range []string{`{"keys": [{"file": "key"}]}`, `{"keys": [{"file": "/k", "lifetime": "1y"}]}`, `{"keys": 1}`} {
		if err := os.WriteFile(a.Dirs.AgentKeys(), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.added(); !errors.As(err, new(*state.CorruptError)) {
			t.Errorf("%s: got %v, want a CorruptError", data, err)
		}
	}
}

// add records the key and its fingerprint, starts the agent and loads the
// key there, kept 12 hours.
func TestAdd(t *testing.T) {
	a, f, out, tmp, _ := testAgent(t)
	key := filepath.Join(tmp, "deploy_key")
	writeKey(t, key, "deploy")
	if err := a.Add(t.Context(), key, AddOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got, want := readKeys(t, a), []AddedKey{{File: key, Fingerprint: "SHA256:fp-deploy"}}; !slices.Equal(got, want) {
		t.Errorf("keys = %+v, want %+v", got, want)
	}
	if want := []string{"ssh-agent -a " + a.Socket(), "ssh-add -t 12h key:deploy"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if want := []string{"==> agent_holds keys=deploy comment", "agent_holds_note"}; !slices.Equal(out.lines, want) {
		t.Errorf("said %q, want %q", out.lines, want)
	}
	if fi, err := os.Stat(a.Dirs.AgentKeys()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("agent keys file: %v, %v", fi, err)
	}
}

// --confirm and --lifetime are kept per key and used when loading; adding
// it again replaces them. A lifetime that isn't one, and --confirm without
// an ssh-askpass to ask with, are refused.
func TestAddOptions(t *testing.T) {
	a, f, _, tmp, _ := testAgent(t)
	key := filepath.Join(tmp, "k")
	writeKey(t, key, "k")
	a.Getenv = func(k string) string { return map[string]string{"SSH_ASKPASS": "/usr/bin/true"}[k] } //nolint:gosec // a program, not a credential
	if err := a.Add(t.Context(), key, AddOptions{Confirm: true, Lifetime: "30m"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := readKeys(t, a); len(got) != 1 || got[0].AddOptions != (AddOptions{Confirm: true, Lifetime: "30m"}) {
		t.Errorf("keys = %+v", got)
	}
	if !slices.Contains(f.calls, "ssh-add -c -t 30m key:k") {
		t.Errorf("calls = %q", f.calls)
	}
	if err := a.Add(t.Context(), key, AddOptions{Lifetime: Forever}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := readKeys(t, a); len(got) != 1 || got[0].AddOptions != (AddOptions{Lifetime: Forever}) {
		t.Errorf("keys = %+v", got)
	}
	if f.calls[len(f.calls)-1] != "ssh-add key:k" {
		t.Errorf("calls = %q", f.calls)
	}
	if err := a.Add(t.Context(), key, AddOptions{Lifetime: "soon"}); msg.NamesOf(err) != "bad_duration value=soon" {
		t.Errorf("--lifetime soon: %s", msg.NamesOf(err))
	}
	a.Getenv = func(string) string { return "" }
	if err := a.Add(t.Context(), key, AddOptions{Confirm: true}); msg.NamesOf(err) != "agent_confirm_needs_askpass" {
		t.Errorf("--confirm without ssh-askpass: %s", msg.NamesOf(err))
	}
	a.LookPath = func(string) (string, error) { return "/x/ssh-askpass", nil }
	if err := a.Add(t.Context(), key, AddOptions{Confirm: true}); err != nil {
		t.Errorf("--confirm with ssh-askpass on PATH: %s", msg.NamesOf(err))
	}
	if err := a.Add(t.Context(), filepath.Join(tmp, "none"), AddOptions{}); msg.NamesOf(err) != "agent_key_not_found key="+filepath.Join(tmp, "none") {
		t.Errorf("missing key: %s", msg.NamesOf(err))
	}
	if err := a.Add(t.Context(), tmp, AddOptions{}); !strings.HasPrefix(msg.NamesOf(err), "agent_key_not_found ") {
		t.Errorf("a folder: %s", msg.NamesOf(err))
	}
}

// After a reboot (no agent) it starts one and loads the recorded keys; a
// running agent with its keys needs nothing; a key whose lifetime ran out
// is loaded again.
func TestEnsure(t *testing.T) {
	a, f, out, tmp, _ := testAgent(t)
	deploy, other := filepath.Join(tmp, "deploy"), filepath.Join(tmp, "other")
	writeKey(t, deploy, "deploy")
	writeKey(t, other, "other")
	saveKeys(t, a,
		AddedKey{File: deploy, Fingerprint: "SHA256:fp-deploy"},
		AddedKey{File: other, Fingerprint: "SHA256:fp-other", AddOptions: AddOptions{Lifetime: Forever}},
	)
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"ssh-agent -a " + a.Socket(), "ssh-add -t 12h key:deploy", "ssh-add key:other"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	f.calls = nil
	if err := a.Ensure(t.Context()); err != nil || len(f.calls) != 0 {
		t.Errorf("running, with its keys: %q, %v", f.calls, err)
	}
	f.held = slices.DeleteFunc(f.held, func(k string) bool { return k == "deploy" }) // its lifetime ran out
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"ssh-add -t 12h key:deploy"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if len(out.lines) != 0 {
		t.Errorf("said %q", out.lines)
	}
}

// With no keys added, a new agent says it has none.
func TestEnsureNoKeys(t *testing.T) {
	a, f, out, _, _ := testAgent(t)
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !f.up || !slices.Equal(out.lines, []string{"devvm: agent_no_keys"}) {
		t.Errorf("up %v, said %q", f.up, out.lines)
	}
}

// A key is loaded only while its file holds the key that was added: not a
// link swapped to another, nor one recorded without a fingerprint.
func TestEnsurePinned(t *testing.T) {
	a, f, out, tmp, _ := testAgent(t)
	deploy, main := filepath.Join(tmp, "deploy"), filepath.Join(tmp, "main")
	writeKey(t, deploy, "deploy")
	writeKey(t, main, "main")
	if err := a.Add(t.Context(), deploy, AddOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	*f = fakeAgent{} // a reboot
	if err := os.Remove(deploy); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(main, deploy); err != nil { // what a VM that could write there would do
		t.Fatal(err)
	}
	out.lines = nil
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"devvm: agent_key_changed key=" + deploy}; !slices.Equal(out.lines, want) {
		t.Errorf("said %q, want %q", out.lines, want)
	}
	if slices.Contains(f.held, "main") {
		t.Error("loaded the key the link names now")
	}
	saveKeys(t, a, AddedKey{File: main})
	out.lines = nil
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"devvm: agent_key_unpinned key=" + main}; !slices.Equal(out.lines, want) || slices.Contains(f.held, "main") {
		t.Errorf("said %q, held %q", out.lines, f.held)
	}
}

// A key in a folder a VM mounts is refused, a link to it too; one added
// before a VM mounted its folder isn't loaded.
func TestMountedKey(t *testing.T) {
	a, f, out, tmp, mounts := testAgent(t)
	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(proj, "deploy_key")
	writeKey(t, key, "deploy")
	*mounts = []Mount{{VM: "devvm-other-0123456789abcd", Dir: "/elsewhere"}, {VM: "devvm-proj-0123456789abcd", Dir: proj}}
	want := "agent_key_mounted key=" + key + " vm=devvm-proj-0123456789abcd"
	if err := a.Add(t.Context(), key, AddOptions{}); msg.NamesOf(err) != want {
		t.Errorf("add: %s, want %s", msg.NamesOf(err), want)
	}
	if len(readKeys(t, a)) != 0 || len(f.held) != 0 {
		t.Error("the key was recorded or loaded")
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(t.Context(), link, AddOptions{}); !strings.HasPrefix(msg.NamesOf(err), "agent_key_mounted ") {
		t.Errorf("add a link to it: %s", msg.NamesOf(err))
	}
	// A sibling folder whose name starts the same isn't inside it.
	*mounts = []Mount{{VM: "devvm-pro-0123456789abcd", Dir: filepath.Join(tmp, "pro")}}
	if err := a.Add(t.Context(), key, AddOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	*f = fakeAgent{}
	*mounts = []Mount{{VM: "devvm-proj-0123456789abcd", Dir: proj}}
	out.lines = nil
	if err := a.Ensure(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(out.lines, []string{"devvm: " + want}) || len(f.held) != 0 {
		t.Errorf("said %q, held %q", out.lines, f.held)
	}
}

// keys lists what the agent holds, then each key added: its file,
// fingerprint and options, its lifetime shown when it's the default.
func TestKeys(t *testing.T) {
	a, _, _, tmp, _ := testAgent(t)
	k1, k2 := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	writeKey(t, k1, "a")
	writeKey(t, k2, "b")
	if err := a.Add(t.Context(), k1, AddOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := a.Add(t.Context(), k2, AddOptions{Lifetime: "1h"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	var b strings.Builder
	if err := a.Keys(t.Context(), &b); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := "256 SHA256:fp-a a comment (ED25519)\n256 SHA256:fp-b b comment (ED25519)\n" +
		k1 + ": SHA256:fp-a lifetime=12h\n" + k2 + ": SHA256:fp-b lifetime=1h\n"
	if b.String() != want {
		t.Errorf("keys:\n%s\nwant:\n%s", b.String(), want)
	}
	listed, err := a.KeyList(t.Context())
	wantList := []ListedKey{
		{File: k1, Fingerprint: "SHA256:fp-a", Lifetime: "12h", Loaded: true},
		{File: k2, Fingerprint: "SHA256:fp-b", Lifetime: "1h", Loaded: true},
	}
	if err != nil || !slices.Equal(listed, wantList) {
		t.Errorf("KeyList = %+v, %v; want %+v", listed, msg.NamesOf(err), wantList)
	}
}

// With no key added, keys writes nothing and starts no agent.
func TestKeysNone(t *testing.T) {
	a, f, out, _, _ := testAgent(t)
	var b strings.Builder
	if err := a.Keys(t.Context(), &b); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if b.Len() != 0 || len(f.calls) != 0 || f.up || len(out.lines) != 0 {
		t.Errorf("wrote %q, calls %q, said %q", b.String(), f.calls, out.lines)
	}
	if l, err := a.KeyList(t.Context()); err != nil || l != nil || len(f.calls) != 0 {
		t.Errorf("KeyList = %+v, %v, calls %q", l, err, f.calls)
	}
}

// rm takes the key out of the agent, by its fingerprint, and out of the
// keys file, leaving the others and the key file itself. Its file needn't
// exist, a relative path finds it, and one not added is refused, with
// nothing changed.
func TestRemove(t *testing.T) {
	a, f, out, tmp, _ := testAgent(t)
	k1, k2 := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	writeKey(t, k1, "a")
	writeKey(t, k2, "b")
	for _, k := range []string{k1, k2} {
		if err := a.Add(t.Context(), k, AddOptions{}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	if err := a.Remove(t.Context(), filepath.Join(tmp, "nope")); msg.NamesOf(err) != "agent_key_not_recorded key="+filepath.Join(tmp, "nope") {
		t.Errorf("not added: %s", msg.NamesOf(err))
	}
	if len(readKeys(t, a)) != 2 || len(f.held) != 2 {
		t.Fatalf("not added: changed %+v %q", readKeys(t, a), f.held)
	}
	if err := os.Remove(k1); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)
	out.lines = nil
	if err := a.Remove(t.Context(), "a"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got, want := readKeys(t, a), []AddedKey{{File: k2, Fingerprint: "SHA256:fp-b"}}; !slices.Equal(got, want) {
		t.Errorf("keys = %+v, want %+v", got, want)
	}
	if !slices.Equal(f.held, []string{"b"}) || !slices.Contains(f.calls, "ssh-add -d key:a") {
		t.Errorf("held %q, calls %q", f.held, f.calls)
	}
	if want := []string{"==> agent_key_removed key=" + k1}; !slices.Equal(out.lines, want) {
		t.Errorf("said %q, want %q", out.lines, want)
	}
	if _, err := os.Stat(k2); err != nil {
		t.Errorf("the other key's file: %v", err)
	}
	// An agent that doesn't run, or no longer holds it: only the record goes.
	f.up = false
	if err := a.Remove(t.Context(), k2); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(readKeys(t, a)) != 0 {
		t.Errorf("keys = %+v", readKeys(t, a))
	}
}

// With OpenSSH's own tools: a throwaway key, in an agent of the test's
// own, whose socket the test names. No other agent is reached.
func TestRealAgent(t *testing.T) {
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " isn't installed")
		}
	}
	// /tmp keeps the socket's path short.
	root, err := os.MkdirTemp("/tmp", "dva.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data")) // the run log
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	d := paths.Dirs{Data: filepath.Join(root, "data", "devvm"), Config: filepath.Join(root, "config", "devvm")}
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	agent := exec.CommandContext(t.Context(), "ssh-agent", "-D", "-a", d.AgentSock()) //nolint:gosec // the test's own socket
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	for range 100 {
		if _, err := os.Stat(d.AgentSock()); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	key := filepath.Join(root, "key")
	keygen := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "devvm-test-key", "-f", key) //nolint:gosec // the test's own file
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	var out said
	a := New(d, func(context.Context) ([]Mount, error) { return nil, nil }, out.ui())
	if err := a.Add(t.Context(), key, AddOptions{Lifetime: "1h"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(out.lines) == 0 || out.lines[0] != "==> agent_holds keys=devvm-test-key" {
		t.Errorf("said %q", out.lines)
	}
	keys := readKeys(t, a)
	if len(keys) != 1 || keys[0].File != key || !strings.HasPrefix(keys[0].Fingerprint, "SHA256:") || keys[0].Lifetime != "1h" {
		t.Errorf("keys = %+v", keys)
	}
	var b strings.Builder
	if err := a.Keys(t.Context(), &b); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !strings.Contains(b.String(), " "+keys[0].Fingerprint+" devvm-test-key (ED25519)\n") ||
		!strings.Contains(b.String(), key+": "+keys[0].Fingerprint+" lifetime=1h\n") {
		t.Errorf("keys:\n%s", b.String())
	}
	// rm takes it out of the agent and the record, and leaves its file.
	if err := a.Remove(t.Context(), key); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if held, code := a.list(t.Context()); code != 1 {
		t.Errorf("after rm, the agent holds (%d):\n%s", code, held)
	}
	if len(readKeys(t, a)) != 0 {
		t.Errorf("after rm, keys = %+v", readKeys(t, a))
	}
	if _, err := os.Stat(key); err != nil {
		t.Errorf("after rm, the key's file: %v", err)
	}
}

func TestMode(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir(), Config: t.TempDir()}
	st, err := state.New(d, "devvm-web-0123456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend = "base", "/p", "lima"
	if err := st.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for val, want := range map[string]string{"": Off, "off": Off, "on": On, "devvm": Devvm, "bogus": Off} {
		if err := st.Update(t.Context(), func(st *state.VM) error { return SetMode(st, val) }); err != nil {
			t.Fatal(err)
		}
		if got := Mode(st); got != want {
			t.Errorf("Mode(%q) = %q, want %q", val, got, want)
		}
	}
	// What devvm made of it: the start it last started with, kept by a
	// change of its choice.
	if err := RecordStart(t.Context(), st, Devvm, "start-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(t.Context(), func(st *state.VM) error { return SetMode(st, On) }); err != nil {
		t.Fatal(err)
	}
	again, err := state.ReadVM(d, st.Name())
	if err != nil {
		t.Fatal(err)
	}
	if Running(again, "start-1") != Devvm || Running(again, "start-2") != On {
		t.Errorf("Running = %q, %q; want devvm for its start, else on", Running(again, "start-1"), Running(again, "start-2"))
	}
	// Relayed only for the start recorded so.
	if !Relayed(again, "start-1") || Relayed(again, "start-2") || Relayed(again, "") {
		t.Errorf("Relayed = %v, %v, %v; want only start-1's", Relayed(again, "start-1"), Relayed(again, "start-2"), Relayed(again, ""))
	}
	if ReachOf(again, "start-1") != Relay || ReachOf(again, "start-2") != None {
		t.Errorf("ReachOf = %v, %v; want the relay for its start, none for one not recorded", ReachOf(again, "start-1"), ReachOf(again, "start-2"))
	}
	// Every start this devvm records that forwards an agent is relayed,
	// whichever agent: only an older devvm's record lacks it.
	if err := RecordStart(t.Context(), st, On, "start-1"); err != nil {
		t.Fatal(err)
	}
	if !Relayed(st, "start-1") || ReachOf(st, "start-1") != Relay {
		t.Error("a start of this devvm's that forwards your agent isn't relayed")
	}
	olderStart(t, st, Devvm, "start-1")
	if Relayed(st, "start-1") || ReachOf(st, "start-1") != Raw || ReachOf(st, "") != None {
		t.Error("an older devvm's start: not relayed, the agent's own socket, and none for no start")
	}
	if err := RecordStart(t.Context(), st, Off, "start-1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := Of(st); ReachOf(st, "start-1") != None || a.Made.Relayed {
		t.Error("a start without an agent reaches one, or is relayed")
	}
	if err := RecordStart(t.Context(), st, Devvm, ""); err != nil {
		t.Fatal(err)
	}
	if a, _ := Of(st); a.Made.Started != "" || a.Made.Relayed {
		t.Errorf("no start recorded: %+v", a.Made)
	}
}

// olderStart records start as an older devvm did: the start, without
// Made.Relayed, as that devvm forwarded the agent itself.
func olderStart(t *testing.T, st *state.VM, mode, start string) {
	t.Helper()
	if err := RecordStart(t.Context(), st, mode, start); err != nil {
		t.Fatal(err)
	}
	err := st.Update(t.Context(), func(st *state.VM) error {
		a, err := Of(st)
		if err != nil {
			return err
		}
		a.Made.Relayed = false
		return Set(st, a)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A lifetime is forever or a duration as every flag takes one.
func TestCheckLifetime(t *testing.T) {
	for s, want := range map[string]bool{"30m": true, "8h": true, "1d": true, "2w": true, "forever": true, "1h30m": false, "3600": false, "": false, "soon": false, "-1h": false, "8 h": false} {
		if got := CheckLifetime(s) == nil; got != want {
			t.Errorf("CheckLifetime(%q) ok = %v, want %v", s, got, want)
		}
	}
}
