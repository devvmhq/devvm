package agentlogin

import (
	"context"
	"fmt"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// format is what devvm makes of one kind of login file (agents.Login's Kind), parsed as m:
// File's Bindings, VMCopy and Get go to the format of its Kind.
type format interface {
	// bindings are m's logins as bindings, of proxy.KindLogin, their placeholders ending in salt.
	bindings(m map[string]any, salt string) []proxy.Binding
	// vmCopy makes m the VM's copy, its placeholders ending in f.Salt, and returns it with what
	// it holds as it is (VMFile's Kept) and without its keys and tokens (Withheld).
	vmCopy(m map[string]any, f File) (map[string]any, VMFile)
	// get returns the value of the binding named name from f's file, read again.
	get(ctx context.Context, f File, name string) (string, error)
}

// keyFormat is a format that holds the API keys devvm agent login saves (agents.Keys): setKey
// returns m with key saved as provider's ("" for an agent's one key).
type keyFormat interface {
	setKey(m map[string]any, provider, key string) map[string]any
}

// chatGPTFormat is a format that can hold a ChatGPT login: chatGPT returns its tokens, the one
// that grants access last, none when m holds no ChatGPT login.
type chatGPTFormat interface {
	chatGPT(m map[string]any) []string
}

// formats are every login kind's format (agents.LoginKinds; TestFormatsCoverLogins).
func formats() map[string]format {
	return map[string]format{
		agents.Claude: claudeFormat{}, agents.ClaudeOAuth: claudeOAuthFormat{},
		agents.Codex: codexFormat{}, agents.Opencode: opencodeFormat{}, agents.Pi: piFormat{},
	}
}

// formatOf is the format of f's Kind.
func (f File) formatOf() (format, error) {
	if fm, ok := formats()[f.Kind]; ok {
		return fm, nil
	}
	return nil, f.errKind()
}

// errKind is a File of no agent devvm knows.
func (f File) errKind() error { return fmt.Errorf("unknown login kind %q", f.Kind) }

type (
	codexFormat       struct{}
	opencodeFormat    struct{}
	piFormat          struct{}
	claudeFormat      struct{}
	claudeOAuthFormat struct{}
)

func (codexFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	return codexBindings(m, salt)
}

func (codexFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	kept := codexOthers(m)
	return codexVM(m, f.Salt), VMFile{Kept: kept}
}

func (codexFormat) get(ctx context.Context, f File, name string) (string, error) {
	return f.codexGet(ctx, name)
}

// setKey: an API-key login, as codex login --with-api-key makes it, its ChatGPT login gone.
func (codexFormat) setKey(_ map[string]any, _, key string) map[string]any {
	return map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": key}
}

func (codexFormat) chatGPT(m map[string]any) []string {
	t := obj(m, "tokens")
	return []string{str(t, "id_token"), str(t, "access_token")}
}

func (opencodeFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	return opencodeBindings(m, salt)
}

// vmCopy: Kept are the providers File.Pass lists, whose entries go as they are.
func (opencodeFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	passed, withheld := opencodeSplit(m, f.Pass)
	return opencodeVM(m, f.Salt, f.Pass), VMFile{Kept: passed, Withheld: withheld}
}

func (opencodeFormat) get(ctx context.Context, f File, name string) (string, error) {
	return f.opencodeGet(ctx, name)
}

// setKey: provider's entry, its other providers kept.
func (opencodeFormat) setKey(m map[string]any, provider, key string) map[string]any {
	m[provider] = map[string]any{"type": "api", "key": key}
	return m
}

func (opencodeFormat) chatGPT(m map[string]any) []string {
	if e := obj(m, "openai"); str(e, "type") == "oauth" {
		return []string{str(e, "access")}
	}
	return nil
}

func (piFormat) bindings(m map[string]any, salt string) []proxy.Binding { return piBindings(m, salt) }

// vmCopy: Kept are the providers File.Pass lists, whose entries go as they are.
func (piFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	passed, withheld := piSplit(m, f.Pass)
	return piVM(m, f.Salt, f.Pass), VMFile{Kept: passed, Withheld: withheld}
}

func (piFormat) get(_ context.Context, f File, name string) (string, error) { return f.piGet(name) }

// setKey: provider's entry, its env kept, its other providers kept.
func (piFormat) setKey(m map[string]any, provider, key string) map[string]any {
	e := map[string]any{"type": "api_key", "key": key}
	if env := obj(obj(m, provider), "env"); len(env) > 0 {
		e["env"] = env
	}
	m[provider] = e
	return m
}

func (claudeFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	return claudeBindings(m, salt)
}

func (claudeFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	return claudeVM(m, f.Salt), VMFile{}
}

func (claudeFormat) get(_ context.Context, f File, name string) (string, error) {
	return f.claudeGet(name)
}

func (claudeOAuthFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	return claudeOAuthBindings(m, salt)
}

func (claudeOAuthFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	return claudeOAuthVM(m, f.Salt), VMFile{}
}

func (claudeOAuthFormat) get(ctx context.Context, f File, name string) (string, error) {
	return f.claudeOAuthGet(ctx, name)
}
