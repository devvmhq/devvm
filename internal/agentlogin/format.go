package agentlogin

import (
	"context"
	"fmt"
	"slices"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// format is what devvm makes of one kind of login file (agents.Login's Kind), parsed as m:
// File's Bindings, VMCopy and Get go to the format of its Kind.
type format interface {
	// kind is the kind of login file it's the format of (agents.Login's Kind).
	kind() string
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

// keep is what of a JSON object in a login file the VM's copy holds: plain, the fields it copies
// when each is a plain value (plainField); bound, those the format puts its placeholders in,
// never copied; and nested, the objects it holds some of, each by its own keep. Everything else is
// withheld (keep.apply), so a field devvm doesn't know never reaches a VM
// (TestFormatsWithholdUnknown).
type keep struct {
	plain, bound []string
	nested       map[string]keep
}

// What each format keeps or binds, in one place: the formats' VM copies hold only these.
var (
	// withheldKeep is a withheld login entry (opencode's or pi's, neither bound nor passed): the
	// fields that name an account or how it logs in, never a key or a token.
	withheldKeep = keep{plain: []string{"type", "expires", "accountId", "enterpriseUrl", "projectId", "email", "scope", "scopes", "clientId"}}
	// codexKeep is Codex's auth.json (AuthDotJson and TokenData in codex-rs/login, checked at
	// 0.166.0): its API key and tokens bound, the account and login IDs and the refresh token's
	// issuer and client ID kept. Its other logins (agent_identity, personal_access_token,
	// bedrock_api_key, bedrock_access_keys) have no binding, so they're withheld.
	codexKeep = keep{
		plain: []string{"auth_mode", "last_refresh"},
		bound: []string{"OPENAI_API_KEY"},
		nested: map[string]keep{"tokens": {
			plain:  []string{"account_id", "login_owner_id"},
			bound:  []string{"id_token", "access_token", "refresh_token"},
			nested: map[string]keep{"oauth_provenance": {plain: []string{"issuer", "client_id"}}},
		}},
	}
	// claudeAccountKeep is ~/.claude.json's oauthAccount, as Claude Code writes it (checked at
	// 2.1.319: its profile, its roles and its rate-limit tiers). ccOnboardingFlags stays, empty,
	// as Claude Code fetches the profile again while it's missing; its flags are withheld.
	claudeAccountKeep = keep{nested: map[string]keep{"oauthAccount": {
		plain: []string{
			"accountUuid", "emailAddress", "organizationUuid", "hasExtraUsageEnabled", "billingType",
			"accountCreatedAt", "subscriptionCreatedAt", "claudeCodeTrialEndsAt", "claudeCodeTrialDurationDays",
			"seatTier", "planDisplayName", "displayName", "fullName", "profileFetchedAt", "organizationRole",
			"workspaceRole", "organizationName", "organizationType", "organizationRateLimitTier", "userRateLimitTier",
		},
		nested: map[string]keep{"ccOnboardingFlags": {}},
	}}}
)

// apply returns what of m the VM's copy holds by k, without k's bound fields, which the format
// fills in, and the paths of the fields it withholds (tokens.x for a nested one), sorted.
func (k keep) apply(m map[string]any) (map[string]any, []string) {
	out, withheld := map[string]any{}, []string{}
	for _, f := range sortedKeys(m) {
		v := m[f]
		switch n, nested := k.nested[f]; {
		case slices.Contains(k.bound, f):
		case nested:
			o, ok := v.(map[string]any)
			if !ok {
				withheld = append(withheld, f)
				continue
			}
			sub, w := n.apply(o)
			out[f] = sub
			for _, p := range w {
				withheld = append(withheld, f+"."+p)
			}
		case slices.Contains(k.plain, f) && plainField(v):
			out[f] = v
		default:
			withheld = append(withheld, f)
		}
	}
	return out, withheld
}

// formats are every login kind's format, by the kind each names (agents.LoginKinds;
// TestFormatsCoverLogins).
func formats() map[string]format {
	list := []format{claudeFormat{}, claudeOAuthFormat{}, codexFormat{}, opencodeFormat{}, piFormat{}, cursorFormat{}}
	out := make(map[string]format, len(list))
	for _, f := range list {
		out[f.kind()] = f
	}
	return out
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

func (codexFormat) kind() string       { return agents.Codex }
func (opencodeFormat) kind() string    { return agents.Opencode }
func (piFormat) kind() string          { return agents.Pi }
func (claudeFormat) kind() string      { return agents.Claude }
func (claudeOAuthFormat) kind() string { return agents.ClaudeOAuth }

// endpoints: OpenAI's, which opencode's Codex logins share.
func (codexFormat) endpoints() []endpoint {
	return []endpoint{{host: "auth.openai.com", path: "/oauth/token"}}
}

func (codexFormat) userAgents() []string { return nil }

func (codexFormat) capture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	return codexCapture(m, t, r, salt)
}

func (codexFormat) managedReply(m map[string]any, salt string) ([]byte, error) {
	tk := obj(m, "tokens")
	return openaiManagedReply(str(tk, "access_token"), str(tk, "id_token"), salt)
}

// exchange: its id_token for an API key.
func (codexFormat) exchange(t tokenRequest) bool { return apiKeyExchange(t) }

// endpoints: OpenAI's, Codex's too.
func (opencodeFormat) endpoints() []endpoint {
	return []endpoint{{host: "auth.openai.com", path: "/oauth/token"}}
}

// userAgents: Bun's, which opencode runs on, and opencode's own.
func (opencodeFormat) userAgents() []string { return []string{"bun/", "opencode/"} }

func (opencodeFormat) capture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	return opencodeCapture(m, t, r, salt)
}

func (opencodeFormat) managedReply(m map[string]any, salt string) ([]byte, error) {
	var at string
	if e := obj(m, "openai"); str(e, "type") == "oauth" {
		at = str(e, "access")
	}
	return openaiManagedReply(at, "", salt)
}

// refreshPlaceholder: its VM copy holds its access token's placeholder as its refresh token too.
func (opencodeFormat) refreshPlaceholder(m map[string]any, salt string) string {
	return openaiPlaceholder(str(obj(m, "openai"), "access"), salt)
}

// endpoints: Claude's, and the Console's, which older versions of Claude Code post to.
func (claudeOAuthFormat) endpoints() []endpoint {
	return []endpoint{{host: "platform.claude.com", path: "/v1/oauth/token"}, {host: "console.anthropic.com", path: "/v1/oauth/token"}}
}

func (claudeOAuthFormat) userAgents() []string { return nil }

func (claudeOAuthFormat) capture(m map[string]any, t tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	return claudeCapture(m, t, r, salt)
}

func (claudeOAuthFormat) managedReply(m map[string]any, salt string) ([]byte, error) {
	return claudeManagedReply(m, salt)
}

func (codexFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	return codexBindings(m, salt)
}

// vmCopy: Unknown are the fields codexKeep withholds.
func (codexFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	c, unknown := codexVM(m, f.Salt)
	return c, VMFile{Unknown: unknown}
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
	c := opencodeSplit(m, f.Pass)
	return opencodeVM(m, f.Salt, f.Pass), c
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
	c := piSplit(m, f.Pass)
	return piVM(m, f.Salt, f.Pass), c
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

// vmCopy: Unknown are the fields of oauthAccount that claudeAccountKeep withholds.
func (claudeFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	c := claudeVM(m, f.Salt)
	return c, VMFile{Unknown: claudeAccount(c)}
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
