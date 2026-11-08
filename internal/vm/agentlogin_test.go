package vm

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/secrets"
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
	f.ExecFunc = func(_ string, argv []string, o backend.ExecOpts) error {
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
	if err := login.importLogins(t.Context(), tg, []byte(`{}`)); msg.NamesOf(err) != "opencode_import_too_much vm="+vm {
		t.Errorf("forever: %v", msg.NamesOf(err))
	}
	forever = false
	err = login.importLogins(t.Context(), tg, []byte(`{}`))
	if le, ok := errors.AsType[*backend.LoggedError](err); !ok || msg.NamesOf(le.Msg) != "opencode_import_failed providers=openai vm="+vm {
		t.Errorf("refused: %v", msg.NamesOf(err))
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
	const vm = "devvm-p-00000000000000"
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
	if err := store.SaveKey(t.Context(), "codex", "", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := e.saved(store, "codex", vm, before); !notSaved(err) {
		t.Errorf("an API key: %v", err)
	}
	want := []string{"agent_login_saved_for agent=codex email=a@b.test vm=" + vm, "agent_login_saved agent=opencode vm=" + vm}
	if names := got.Names(); !slices.Equal(names, want) {
		t.Errorf("said %q, want %q", names, want)
	}
}

// Each agent's own ChatGPT login: Codex's kept in its file, opencode's
// for OpenAI, in a browser or with a code.
func TestLoginArgv(t *testing.T) {
	for _, c := range []struct {
		o    AgentLoginOptions
		want []string
	}{
		{AgentLoginOptions{Agent: "codex"}, []string{"codex", "-c", `cli_auth_credentials_store="file"`, "login"}},
		{AgentLoginOptions{Agent: "codex", Device: true}, []string{"codex", "-c", `cli_auth_credentials_store="file"`, "login", "--device-auth"}},
		{AgentLoginOptions{Agent: "opencode"}, []string{"opencode", "auth", "login", "openai", "--method", "chatgpt-browser"}},
		{AgentLoginOptions{Agent: "opencode", Device: true}, []string{"opencode", "auth", "login", "openai", "--method", "chatgpt-headless"}},
		{AgentLoginOptions{Agent: "claude"}, []string{"env", "-u", "CLAUDE_CODE_OAUTH_TOKEN", "claude", "auth", "login", "--claudeai"}},
		{AgentLoginOptions{Agent: "claude", SetupToken: true}, []string{"env", "-u", "CLAUDE_CODE_OAUTH_TOKEN", "claude", "setup-token"}},
	} {
		if got := loginArgv(c.o); !slices.Equal(got, c.want) {
			t.Errorf("loginArgv(%+v) = %q, want %q", c.o, got, c.want)
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
