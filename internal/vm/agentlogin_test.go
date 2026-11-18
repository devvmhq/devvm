package vm

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/vmname"
)

// The providers opencode's import refused are the script's
// devvm-opencode-failed lines alone, in order, made printable.
func TestOpencodeRefused(t *testing.T) {
	for _, c := range []struct {
		out  string
		want []string
	}{
		{"", nil},
		{"Imported 1 credential\n", nil},
		{"devvm-opencode-failed openai\nnoise\ndevvm-opencode-failed openrouter\r\n", []string{"openai", "openrouter"}},
		{"devvm-opencode-failed \n", nil},
		{"devvm-opencode-failed a\x1bb\n", []string{msg.Printable("a\x1bb")}},
	} {
		if got := opencodeRefused(c.out); !slices.Equal(got, c.want) {
			t.Errorf("opencodeRefused(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// devvm reads at most importOutMax bytes of opencode's import of its
// logins: a script that keeps printing is stopped and refused, and one
// that prints a refusal names its providers.
func TestImportLoginsTooMuch(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	vm := existingVM(t, e, Options{Network: "open"}).Name()
	tg, err := (finder{e}).target(t.Context(), need{})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	forever := true
	f.ExecFunc = func(_ vmname.VM, argv []string, o backend.ExecOpts) error {
		if len(argv) != 4 || argv[3] != opencodeScript {
			return nil
		}
		line := []byte(strings.Repeat("devvm-opencode-failed openai\n", 1<<10))
		for forever {
			if _, err := o.Stdout.Write(line); err != nil {
				return err
			}
		}
		_, _ = o.Stdout.Write([]byte("devvm-opencode-failed openai\n"))
		return errors.New("exit 1")
	}
	login := AgentLogin{e}
	if err := login.importLogins(t.Context(), tg, agents.Opencode, []byte(`{}`)); msg.NamesOf(err) != "opencode_import_too_much vm="+vm.String() {
		t.Errorf("forever: %v", msg.NamesOf(err))
	}
	forever = false
	err = login.importLogins(t.Context(), tg, agents.Opencode, []byte(`{}`))
	if le, ok := errors.AsType[*backend.LoggedError](err); !ok || msg.NamesOf(le.Msg) != "opencode_import_failed providers=openai vm="+vm.String() {
		t.Errorf("refused: %v", msg.NamesOf(err))
	}
	// Another agent's login imports nothing, and runs nothing in the VM.
	forever = true
	if err := login.importLogins(t.Context(), tg, agents.Pi, []byte(`{}`)); err != nil {
		t.Errorf("pi's: %v", err)
	}
}

// What devvm agent login says the store saved: a new ChatGPT login, with
// the account's email when its tokens name one; an API key, the same
// login as before, or none at all, fails.
func TestAgentLoginSaved(t *testing.T) {
	root := t.TempDir()
	d := paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	var got msg.Recorder
	fake := &msg.Fake{OnAny: got.Add}
	store := secrets.NewStore(d, fake)
	store.ExcludeFromBackup = func(context.Context, string) error { return nil }
	e := AgentLogin{&Env{Dirs: d, Reporter: fake}}
	const vm vmname.VM = "devvm-p-00000000000000"
	notSaved := func(err error) bool {
		m, ok := errors.AsType[msg.M](err)
		return ok && m.ID() == "agent_login_not_saved"
	}

	if err := e.saved(store, "codex", vm, store.LoginSum("codex")); !notSaved(err) {
		t.Errorf("no login: %v", err)
	}
	before := store.LoginSum("codex")
	jwt := func(claims string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
	}
	write(t, d.AgentLogins("codex"), map[string]string{".codex/auth.json": `{"tokens": {"id_token": "` +
		jwt(`{"email":"a@b.test"}`) + `", "access_token": "` + jwt(`{}`) + `"}}`})
	if err := e.saved(store, "codex", vm, before); err != nil {
		t.Fatal(err)
	}
	if err := e.saved(store, "codex", vm, store.LoginSum("codex")); !notSaved(err) {
		t.Errorf("the same login: %v", err)
	}
	write(t, d.AgentLogins("opencode"), map[string]string{".local/share/opencode/auth.json": `{"openai": {"type": "oauth", "access": "opaque"}}`})
	if err := e.saved(store, "opencode", vm, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveKey(t.Context(), "codex", "", redact.NewSecret("sk-test")); err != nil {
		t.Fatal(err)
	}
	if err := e.saved(store, "codex", vm, before); !notSaved(err) {
		t.Errorf("an API key: %v", err)
	}
	// A login that isn't ChatGPT's (Cursor's): saved when its file changed.
	if err := e.saved(store, agents.Cursor, vm, store.LoginSum(agents.Cursor)); !notSaved(err) {
		t.Errorf("no Cursor login: %v", err)
	}
	cursorBefore := store.LoginSum(agents.Cursor)
	write(t, d.AgentLogins(agents.Cursor), map[string]string{".config/cursor/auth.json": `{"accessToken": "at", "refreshToken": "rt"}`})
	if err := e.saved(store, agents.Cursor, vm, cursorBefore); err != nil {
		t.Fatal(err)
	}
	if err := e.saved(store, agents.Cursor, vm, store.LoginSum(agents.Cursor)); !notSaved(err) {
		t.Errorf("the same Cursor login: %v", err)
	}
	want := []string{
		"agent_login_saved_for agent=codex email=a@b.test vm=" + vm.String(), "agent_login_saved agent=opencode vm=" + vm.String(),
		"agent_own_login_saved display=Cursor CLI vm=" + vm.String(),
	}
	if names := got.Names(); !slices.Equal(names, want) {
		t.Errorf("said %q, want %q", names, want)
	}
}

// An agent's login in the VM gets devvm's terminal as the files they are
// when devvm has one, so Lima gives the login a terminal of its own in
// the VM (claude setup-token draws on it), and devvm's own writers,
// pipes, when it hasn't.
func TestAgentLoginTerminal(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	existingVM(t, e, Options{Network: "open"})
	tg, err := (finder{e}).target(t.Context(), need{})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	errOut, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errOut.Close()
	loginOpts := func() backend.ExecOpts {
		t.Helper()
		calls := f.Calls()
		for i := len(calls) - 1; i >= 0; i-- {
			c := calls[i]
			if argv, ok := c.Args[1].([]string); c.Method == "Exec" && ok && len(argv) > 2 && argv[2] == loginScript {
				o, _ := c.Args[2].(backend.ExecOpts)
				return o
			}
		}
		t.Fatal("the login didn't run")
		return backend.ExecOpts{}
	}
	login := AgentLogin{e}
	o := AgentLoginOptions{Agent: agents.Claude, SetupToken: true}
	for _, c := range []struct {
		name     string
		terminal func() (stdout, stderr *os.File)
		stdout   io.Writer
		stderr   io.Writer
		warned   []string
	}{
		{"a terminal", func() (*os.File, *os.File) { return out, errOut }, out, errOut, nil},
		{"no terminal", func() (*os.File, *os.File) { return nil, nil }, e.Stdout(), e.Stderr(), []string{"setup_token_no_terminal agent=claude"}},
		{"no seam", nil, e.Stdout(), e.Stderr(), []string{"setup_token_no_terminal agent=claude"}},
	} {
		var said msg.Recorder
		fake(e).OnWarn = said.Add
		e.Terminal = c.terminal
		if err := login.login(t.Context(), tg, o); err != nil {
			t.Fatalf("%s: %v", c.name, msg.NamesOf(err))
		}
		if names := said.Names(); !slices.Equal(names, c.warned) {
			t.Errorf("%s: warned %q, want %q", c.name, names, c.warned)
		}
		got := loginOpts()
		if got.Stdout != c.stdout || got.Stderr != c.stderr {
			t.Errorf("%s: the login's stdout %T, stderr %T, want %T, %T", c.name, got.Stdout, got.Stderr, c.stdout, c.stderr)
		}
		if got.Stdin != os.Stdin {
			t.Errorf("%s: the login's stdin %v, want devvm's", c.name, got.Stdin)
		}
	}
}

// pi has no login of its own here: without an API key, Run refuses
// before it finds or starts a VM.
func TestAgentLoginPiKeyOnly(t *testing.T) {
	if err := (AgentLogin{}).Run(t.Context(), AgentLoginOptions{Agent: "pi", Device: true}); msg.NamesOf(err) != "agent_login_key_only agent=pi" {
		t.Errorf("Run(pi) = %v", msg.NamesOf(err))
	}
	if !slices.Contains(agents.LoginAgents(), "pi") {
		t.Error("LoginAgents lacks pi")
	}
}
