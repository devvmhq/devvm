// Package secrets keeps tokens on the host for VMs: a VM holds a placeholder of its own, and its proxy puts the real value into
// requests for the token's hosts. Values live in the macOS Keychain, or in
// files only the user can read elsewhere. Agents' logins (each
// agents.Def's Logins) stay in devvm's store of them on the host
// (LoginFile), read with agentlogin.
package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// Section is the name of the secrets section in a VM's state.
const Section = "secrets"

// Bound is a VM's secrets section: the secrets bound to it, as asked
// (claude comes with its shared tools), and for each of yours among them
// what it was bound to (Yours).
type Bound struct {
	Names []string      `json:"names,omitempty"`
	Yours []BoundSecret `json:"yours,omitempty"`
}

// BoundSecret is a secret of yours as devvm secret use bound a VM to it:
// its name, its definition's ID, and the hosts it had then, beyond which
// the VM's proxy never puts it in, whatever hosts a later devvm secret set
// gives it, until devvm secret use binds the VM again (UsedAs).
type BoundSecret struct {
	Name  string   `json:"name"`
	ID    string   `json:"id"`
	Hosts []string `json:"hosts"`
}

// Of returns a VM's secrets section.
func Of(st *state.VM) (Bound, error) { return state.Section[Bound](st, Section) }

// Secret is a token devvm keeps on the host: the hosts it's for, the header
// the proxy puts it in, and the environment variable a VM finds its
// placeholder in, which starts with Prefix. A HostOnly one is devvm's own,
// which no VM is bound to: the company proxy's password
// (proxy.UpstreamSecret), which devvm's proxies send only to the company
// proxy. A Custom one is yours (custom.go), any other one of Known, and
// only yours have an ID, a Scheme, a Path, Methods or BasicUser
// (proxy.Binding's).
type Secret struct {
	Name      string
	Hosts     []string
	Header    string
	Env       string
	Prefix    string
	Scheme    string
	Path      string
	Methods   string
	BasicUser bool
	ID        string
	HostOnly  bool
	Custom    bool
	unnamed   bool // its placeholder doesn't name it: Claude Code's token's
}

// Known are the secrets devvm keeps, by name.
var Known = []Secret{
	{Name: ClaudeToken, Hosts: []string{"api.anthropic.com"}, Header: "Authorization", Env: "CLAUDE_CODE_OAUTH_TOKEN", Prefix: "sk-ant-oat01-", unnamed: true},
	{
		Name: "github", Hosts: []string{"api.github.com", "github.com", "codeload.github.com", "raw.githubusercontent.com"},
		Header: "Authorization", Env: "GH_TOKEN",
	},
	{Name: proxy.UpstreamSecret, HostOnly: true},
}

// Lookup returns the known secret by that name: devvm's own, never yours
// (Store.Lookup).
func Lookup(name string) (Secret, bool) {
	i := slices.IndexFunc(Known, func(s Secret) bool { return s.Name == name })
	if i < 0 {
		return Secret{}, false
	}
	return Known[i], true
}

// Placeholder is what a VM with that salt gets in place of the secret
// (agentlogin.Placeholder): its Prefix, devvm-proxy-managed, its name, and
// the salt. Yours have secret- before the name, which no agent's login's
// placeholder has, so none of yours ever has another's shape.
func (s Secret) Placeholder(salt string) string {
	what := s.Name
	switch {
	case s.unnamed:
		what = ""
	case s.Custom:
		what = "secret-" + s.Name
	}
	return agentlogin.Placeholder(s.Prefix, what, salt)
}

// ErrNone is Value's error for a secret with no value saved.
var ErrNone = errors.New("no value saved")

// isLogin reports whether kind is a kind of login file that stays on the
// host: one of an agent's (agents.LoginKinds).
func isLogin(kind string) bool {
	_, _, ok := agents.LoginOf(kind)
	return ok
}

// LoginRel is a login file of kind (agents.Login's Kind), relative to a
// home folder, where the agent keeps it and where devvm's store of its
// logins keeps it too: its agents.Login's Rel, "" for no agent's.
func LoginRel(kind string) string {
	l, _, _ := agents.LoginOf(kind)
	return l.Rel
}

// agentOf is the agent whose logins folder a login file of kind is in.
func agentOf(kind string) string {
	_, d, _ := agents.LoginOf(kind)
	return d.Name
}

// LoginFile is a login file in devvm's store of the agents' logins
// (paths.Dirs.AgentLogins), the only one devvm reads for VMs: never the
// home folder's.
func LoginFile(d paths.Dirs, kind string) string {
	return filepath.Join(d.AgentLogins(agentOf(kind)), filepath.FromSlash(LoginRel(kind)))
}

// SaveLogin puts an agent's login file into devvm's store, only the
// user's to read (mode 600, its folders 700), the agent's logins folder
// kept out of backups.
func (s *Store) SaveLogin(ctx context.Context, kind string, data []byte) error {
	dir := s.Dirs.AgentLogins(agentOf(kind))
	if err := state.PrivateDir(dir); err != nil {
		return err
	}
	if err := s.ExcludeFromBackup(ctx, dir); err != nil {
		return err
	}
	return state.WriteFile(LoginFile(s.Dirs, kind), data)
}

// loginHosts are the hosts of the token endpoints a VM's agents log in at
// (agentlogin.Endpoints), which its allowlist has: its proxy captures
// logins at every VM's, but only these are allowed.
func loginHosts(st *state.VM) []string {
	agents := image.AgentsOf(tools.Names(st))
	var hosts []string
	for _, c := range agentlogin.Endpoints() {
		if slices.Contains(agents, c.Agent) {
			hosts = append(hosts, c.Host)
		}
	}
	return hosts
}

// captureFile is the file in devvm's store a login the proxy captured for
// the agent goes in, its placeholders ending in salt: the login file whose
// format captures logins (agentlogin.CaptureKind), Claude Code's OAuth
// login's for claude.
func (s *Store) captureFile(agent, salt string) agentlogin.File {
	return s.login(agentlogin.CaptureKind(agent), salt)
}

// CaptureRequest returns a token request a VM's proxy captures the reply
// to, for the agent's login (proxy.Capturer's Request): its body as it
// goes on, or the reply the VM gets instead (agentlogin.File.CaptureRequest).
func (s *Store) CaptureRequest(agent, salt, ctype string, body []byte) (out, answer []byte, err error) {
	if !isLogin(agent) {
		return nil, nil, msg.NoSecret(agent)
	}
	return s.captureFile(agent, salt).CaptureRequest(ctype, body)
}

// Capture saves the agent's login in reply, its token endpoint's answer to
// req, in devvm's store (agentlogin.File.Capture), the agent's logins
// folder kept out of backups, and returns the reply the VM gets, with
// placeholders ending in salt. Claude Code's login is captured its own way
// (Claude.capture).
func (s *Store) Capture(ctx context.Context, agent, salt, ctype string, req, reply []byte) ([]byte, error) {
	if err := s.loginsDir(ctx, agent); err != nil {
		return nil, err
	}
	if vm, ok, err := ClaudeOf(s).capture(ctx, agent, salt, ctype, req, reply); ok {
		return vm, err
	}
	return s.captureFile(agent, salt).Capture(ctype, req, reply)
}

// loginsDir readies the agent's logins folder in devvm's store: the
// user's alone, out of backups.
func (s *Store) loginsDir(ctx context.Context, kind string) error {
	if !isLogin(kind) {
		return msg.NoSecret(kind)
	}
	dir := s.Dirs.AgentLogins(agentOf(kind))
	if err := state.PrivateDir(dir); err != nil {
		return err
	}
	return s.ExcludeFromBackup(ctx, dir)
}

// SaveKey saves an API key as the agent's login in devvm's store, for
// opencode and pi as provider's (agentlogin.File.SetKey), the agent's logins
// folder kept out of backups.
func (s *Store) SaveKey(ctx context.Context, kind, provider, key string) error {
	if err := s.loginsDir(ctx, kind); err != nil {
		return err
	}
	return s.login(kind, "").SetKey(provider, key)
}

// LoginSum is a hash of the agent's login file in devvm's store, "" when
// it can't be read: what tells a login that changed it from one that
// didn't.
func (s *Store) LoginSum(kind string) string {
	data, err := safefile.Read(LoginFile(s.Dirs, kind), 64<<20, safefile.Opts{Follow: true})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ChatGPT is the ChatGPT login the agent's login file in devvm's store
// holds, with its account's email when it names one
// (agentlogin.File.ChatGPT).
func (s *Store) ChatGPT(kind string) (email string, ok bool, err error) {
	return s.login(kind, "").ChatGPT()
}

// HasLogin reports whether devvm's store has a login file for the agent.
func (s *Store) HasLogin(kind string) bool {
	fi, err := os.Lstat(LoginFile(s.Dirs, kind))
	return err == nil && fi.Mode().IsRegular()
}

// Store reads secrets' values and agents' logins. NewStore sets every
// field; tests change them.
type Store struct {
	Dirs paths.Dirs // its AgentLogins hold the agents' login files
	// Keychain: values are in the Keychain (KeychainGet), else in files.
	Keychain       bool
	KeychainGet    func(ctx context.Context, service, account string) (string, error)
	KeychainSet    func(ctx context.Context, service, account, label, value string) error
	KeychainDelete func(ctx context.Context, service, account string) error
	Getenv         func(string) string
	// ExcludeFromBackup keeps a folder out of backups (macOS).
	ExcludeFromBackup func(ctx context.Context, dir string) error
	// Reporter is where it warns.
	msg.Reporter
}

// NewStore returns the store for devvm's folders and the user's home,
// warning through r.
func NewStore(d paths.Dirs, r msg.Reporter) *Store {
	return &Store{
		Dirs: d, Keychain: platform.HasKeychain, Getenv: os.Getenv,
		KeychainGet: platform.KeychainGet, KeychainSet: platform.KeychainSet, KeychainDelete: platform.KeychainDelete,
		ExcludeFromBackup: platform.ExcludeFromBackup, Reporter: r,
	}
}

// login is an agent's login file in devvm's store, its placeholders
// ending in salt.
func (s *Store) login(kind, salt string) agentlogin.File {
	return agentlogin.File{Kind: kind, Path: LoginFile(s.Dirs, kind), Salt: salt}
}

// VMLogin returns the VM's copy of an agent's login file: placeholders
// ending in salt in place of its keys and tokens. It warns about the
// fields of Codex's file the copy leaves out, and about the logins it
// holds without their tokens. The copy of an agent that config.toml's [pass-keys] takes
// (agents.Def's PassKeys) holds as they are the API keys of the providers
// it lists for the agent (settings.PassKeys), which it notes, and
// withholds every other key devvm doesn't bind. A provider it lists whose
// key devvm binds gets its placeholder all the same, which it notes too.
func (s *Store) VMLogin(kind, salt string) ([]byte, error) {
	f := s.login(kind, salt)
	l, d, _ := agents.LoginOf(kind)
	if d.PassKeys {
		pass, err := settings.PassKeys(s.Dirs, d.Name)
		if err != nil {
			return nil, err
		}
		f.Pass = pass
	}
	c, err := f.VMCopy()
	if list := strings.Join(c.Kept, ", "); list != "" {
		s.Note(msg.LoginPassed(d.Display, list))
	}
	if list := strings.Join(c.Unknown, ", "); list != "" {
		s.Warn(msg.LoginFieldsWithheld(d.Display, path.Base(l.Rel), list))
	}
	if list := strings.Join(c.Withheld, ", "); list != "" {
		s.Warn(msg.LoginWithheld(d.Display, d.Name, list))
	}
	if list := strings.Join(c.PassBound, ", "); list != "" {
		s.Note(msg.LoginPassBound(d.Display, d.Name, list))
	}
	return c.Data, err
}

var serviceName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Service is the Keychain service secrets are saved under:
// DEVVM_SECRET_SERVICE, which keeps test secrets apart, else devvm.
func (s *Store) Service() (string, error) {
	svc := s.Getenv("DEVVM_SECRET_SERVICE")
	if svc == "" {
		return "devvm", nil
	}
	if !serviceName.MatchString(svc) {
		return "", msg.BadSecretService()
	}
	return svc, nil
}

// storageKey is what a secret's value is saved under, its Keychain account
// or its file's name: a known secret's name as it is, and one of yours'
// after custom., which no known secret's name has, so a known secret added
// later never finds a value of yours.
func storageKey(name string) string {
	if _, ok := Lookup(name); ok {
		return name
	}
	return "custom." + name
}

// File is where a secret's value is kept without a Keychain.
func (s *Store) File(name string) string { return filepath.Join(s.Dirs.Secrets(), storageKey(name)) }

// Value returns a known secret's value, or ErrNone. A claude token in
// devvm's environment (CLAUDE_CODE_OAUTH_TOKEN) wins: the proxy inherits it
// from the devvm that started it.
func (s *Store) Value(ctx context.Context, name string) (string, error) {
	if _, ok, err := YoursOf(s).Lookup(name); err != nil {
		return "", err
	} else if !ok {
		return "", ErrNone
	}
	if name == ClaudeToken {
		if v := ClaudeOf(s).Env(); v != "" {
			return v, nil
		}
	}
	return s.saved(ctx, name)
}

// saved returns a known secret's saved value, or ErrNone: from the
// Keychain, else from its file.
func (s *Store) saved(ctx context.Context, name string) (string, error) {
	var v string
	if s.Keychain {
		svc, err := s.Service()
		if err != nil {
			return "", err
		}
		if v, err = s.KeychainGet(ctx, svc, storageKey(name)); errors.Is(err, platform.ErrNoSecret) {
			return "", ErrNone
		} else if err != nil {
			return "", err
		}
	} else {
		b, err := os.ReadFile(s.File(name))
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNone
		} else if err != nil {
			return "", err
		}
		v = string(b)
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", ErrNone
	}
	return v, nil
}

// ProxyPassword returns the value of name, the secret config.toml's
// company proxy takes its password from (proxy.UpstreamSecret): its
// Value, or proxy_password_missing while it isn't saved. It is
// settings.CompanyProxy's Password.
func (s *Store) ProxyPassword(ctx context.Context, name string) (string, error) {
	v, err := s.Value(ctx, name)
	if errors.Is(err, ErrNone) {
		return "", msg.ProxyPasswordMissing()
	}
	return v, err
}

// ValueMax is the longest value a secret may have: the Keychain's command
// line (security -i) is read in lines of 4096 bytes, and a longer one would
// be cut, its end run as commands of its own.
const ValueMax = 3800

// Token returns a token as given for the secret (pasted, or a line on
// stdin) without its whitespace. It refuses an empty one, one with a quote
// or a backslash (they'd end the Keychain's command line), one past
// ValueMax, one with anything but printable ASCII (a request's header
// carries no other, and the Keychain gives what isn't back as hex), one
// that holds a VM's placeholder (agentlogin.HasPlaceholder: copied from a
// VM, it would never be the real value), and for claude anything but a
// setup-token token. The company proxy's password (proxy.UpstreamSecret)
// loses only the whitespace around it: a password may hold spaces.
func Token(name, given string) (string, error) {
	v := strings.Join(strings.Fields(given), "")
	lowest := rune(0x21) // a header's value: no space
	if name == proxy.UpstreamSecret {
		v, lowest = strings.TrimSpace(given), 0x20
	}
	switch {
	case v == "":
		return "", msg.NoToken()
	case strings.ContainsAny(v, `"\`):
		return "", msg.TokenQuotes()
	case strings.ContainsFunc(v, func(r rune) bool { return r < lowest || r > 0x7e }):
		return "", msg.TokenNotAscii()
	case len(v) > ValueMax:
		return "", msg.TokenTooLong(strconv.Itoa(ValueMax))
	case agentlogin.HasPlaceholder([]byte(v)):
		return "", msg.TokenIsPlaceholder()
	case name == ClaudeToken && !strings.HasPrefix(v, "sk-ant-oat01-"):
		return "", msg.NotSetupToken()
	}
	return v, nil
}

// Save saves a known secret's value (one Token returned): in the Keychain,
// else in a file only the user can read. A Claude Code token takes the
// Claude Code OAuth login in devvm's store away, as devvm keeps one Claude
// Code login, the newest. Yours are saved with Yours.Save.
func (s *Store) Save(ctx context.Context, name, value string) error {
	if _, ok := Lookup(name); !ok {
		return msg.NoSecret(name)
	}
	if name != ClaudeToken {
		return s.save(ctx, name, value)
	}
	c := ClaudeOf(s)
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return c.saveToken(ctx, value)
}

// save saves a secret's value under its storageKey: Save's, alone. One
// past ValueMax is refused before the saved one is touched.
func (s *Store) save(ctx context.Context, name, value string) error {
	if len(value) > ValueMax {
		return msg.TokenTooLong(strconv.Itoa(ValueMax))
	}
	key := storageKey(name)
	if !s.Keychain {
		if err := state.PrivateDir(s.Dirs.Secrets()); err != nil {
			return err
		}
		return state.WriteFile(s.File(name), []byte(value+"\n"))
	}
	svc, err := s.Service()
	if err != nil {
		return err
	}
	if s.KeychainSet(ctx, svc, key, "devvm "+key, value) != nil {
		return msg.KeychainSaveFailed(name)
	}
	return nil
}

// Delete deletes a secret's saved value, a known one's or one of yours',
// whose definition goes with Yours.Delete: ErrNone when there's none.
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, ok := Lookup(name); !ok && !nameOK(name) {
		return ErrNone
	}
	key := storageKey(name)
	if !s.Keychain {
		// A save that never finished leaves its value in a temporary file
		// (state.WriteFile's, .<key>.tmp-*): gone with the secret.
		tmps, _ := filepath.Glob(filepath.Join(s.Dirs.Secrets(), "."+key+".tmp-*"))
		for _, f := range tmps {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Remove(s.File(name)); errors.Is(err, os.ErrNotExist) {
			return ErrNone
		} else if err != nil {
			return err
		}
		return nil
	}
	svc, err := s.Service()
	if err != nil {
		return err
	}
	if err := s.KeychainDelete(ctx, svc, key); errors.Is(err, platform.ErrNoSecret) {
		return ErrNone
	} else if err != nil {
		return err
	}
	return nil
}

// Get returns a binding's value for the VM's proxy, which runs it in its
// own process (proxy.IO.Secret): a login's (proxy.KindLogin), from its
// agent's file (agentlogin.File.Get, which refreshes an OAuth token when
// it's due), or a known secret's (proxy.KindSecret). Anything else is
// no_secret. devvm keeps one Claude Code login, so a VM's placeholder of
// the one it had at its launch gets the other once the newest replaced
// it: the claude secret's, the OAuth login's access token, and the other
// way round.
func (s *Store) Get(ctx context.Context, b proxy.Binding) (string, error) {
	switch b.Kind {
	case proxy.KindLogin:
		if !isLogin(b.Agent) {
			return "", msg.NoSecret(b.Name)
		}
		f := s.login(b.Agent, "")
		v, err := f.Get(ctx, b.Name)
		if errors.Is(err, agentlogin.ErrClaudeNoLogin) {
			if tok, terr := s.Value(ctx, ClaudeToken); terr == nil {
				return tok, nil
			}
			return "", msg.ClaudeNoLogin().Because(err)
		}
		if err != nil {
			return "", loginFailed(f.Path, err)
		}
		return strings.TrimSpace(v), nil
	case proxy.KindSecret:
		if err := fits(s, b); err != nil {
			return "", err
		}
		v, err := s.Value(ctx, b.Name)
		if c := ClaudeOf(s); errors.Is(err, ErrNone) && c.ownSecret(b.Name) {
			return c.oauthToken(ctx)
		}
		if errors.Is(err, ErrNone) {
			return "", msg.NoSecret(b.Name)
		}
		return v, err
	}
	return "", msg.NoSecret(b.Name)
}

// fits checks a secret's binding against the secret as it's saved now,
// before its value goes to the proxy that asks for it: the bindings file
// is devvm's, but a VM's proxy may run with one written before a change,
// so its hosts must be within the secret's, and its header, scheme, path,
// way into Basic credentials, definition (ID) and placeholder its own.
// One that doesn't is secret_binding_changed, which the VM's next launch
// or devvm secret use writes again.
func fits(s *Store, b proxy.Binding) error {
	sec, ok, err := YoursOf(s).Lookup(b.Name)
	if err != nil {
		return err
	}
	if !ok {
		return msg.NoSecret(b.Name)
	}
	// The company proxy's password: the proxy asks for it itself, as it
	// starts, with no hosts or placeholder, and never puts it in a VM's
	// request (proxy.UpstreamSecret). A binding of it that has either is
	// no proxy's own.
	if sec.HostOnly {
		if len(b.Hosts) > 0 || b.Placeholder != "" || b.Header != "" {
			return msg.SecretBindingChanged(b.Name)
		}
		return nil
	}
	salt, own := strings.CutPrefix(b.Placeholder, sec.Placeholder("")+"-")
	switch {
	case slices.ContainsFunc(b.Hosts, func(h string) bool { return !slices.Contains(sec.Hosts, h) }),
		b.Header != sec.Header, b.Scheme != sec.Scheme, b.Path != sec.Path, b.Methods != sec.Methods, b.BasicUser != sec.BasicUser, b.ID != sec.ID,
		!own || salt == "" || strings.Trim(salt, "0123456789abcdef") != "":
		return msg.SecretBindingChanged(b.Name)
	}
	return nil
}

// loginFailed is the message for a login's lookup that failed: what
// logging in again with its agent on this machine fixes, or the login file
// at path that can't be read.
func loginFailed(path string, err error) msg.M {
	var re *agentlogin.RefreshError
	switch {
	case errors.Is(err, agentlogin.ErrCodexNoKey):
		return msg.CodexNoKey().Because(err)
	case errors.Is(err, agentlogin.ErrCodexNoChatGPT):
		return msg.CodexNoChatgpt().Because(err)
	case errors.As(err, &re) && re.Provider != "":
		return msg.AgentProviderRefreshFailed(re.Agent, agents.Of(re.Agent).Display, re.Provider, re.Err.Error()).Because(err)
	case errors.As(err, &re):
		return msg.AgentRefreshFailed(re.Agent, agents.Of(re.Agent).Display, re.Err.Error()).Because(err)
	}
	return msg.LoginReadFailed(path, err.Error()).Because(err)
}

// Names returns the secrets a VM uses: those bound to it, then its
// agents' (agents.Def's Secret: claude when it has Claude Code).
// A section that can't be read (the folder's VM's is checked as it's
// found) binds none, and a HostOnly secret is never a VM's, whatever its
// section says: its value would reach the VM, or its proxy's bindings.
func Names(st *state.VM) []string {
	b, _ := Of(st)
	names := slices.DeleteFunc(slices.Clone(b.Names), func(n string) bool {
		s, ok := Lookup(n)
		return ok && s.HostOnly
	})
	for _, a := range image.AgentsOf(tools.Names(st)) {
		if sec := agents.Of(a).Secret; sec != "" && !slices.Contains(names, sec) {
			names = append(names, sec)
		}
	}
	return names
}

// FromTools reports whether a VM uses the secret because of its shared
// tools: an agent's of the VM's (agents.Def's Secret), such as claude
// when it has Claude Code.
func FromTools(st *state.VM, name string) bool {
	for _, a := range image.AgentsOf(tools.Names(st)) {
		if name != "" && agents.Of(a).Secret == name {
			return true
		}
	}
	return false
}

// Bind binds a known secret to a VM (use), or unbinds any secret, in st
// alone: inside st.Update. claude stays with a VM that has Claude Code.
func Bind(st *state.VM, name string, use bool) error {
	b, err := Of(st)
	if err != nil {
		return err
	}
	b.Names = slices.DeleteFunc(b.Names, func(n string) bool { return n == name })
	b.Yours = slices.DeleteFunc(b.Yours, func(y BoundSecret) bool { return y.Name == name })
	if use {
		b.Names = append(b.Names, name)
	}
	return state.SetSection(st, Section, b)
}

// BindYours binds a secret of yours to a VM, in st alone (inside
// st.Update): its definition, by its ID, and its hosts now, which are all
// the VM's proxy ever puts it in for (UsedAs).
func BindYours(st *state.VM, sec Secret) error {
	if err := Bind(st, sec.Name, true); err != nil {
		return err
	}
	b, err := Of(st)
	if err != nil {
		return err
	}
	b.Yours = append(b.Yours, BoundSecret{Name: sec.Name, ID: sec.ID, Hosts: slices.Clone(sec.Hosts)})
	return state.SetSection(st, Section, b)
}

// UsedAs is a secret of yours as the VM st uses it: sec with only the
// hosts it had when the VM was bound to it. ok is false when the VM was
// bound to another definition by its name (stale: a secret saved again
// after devvm secret rm), or to none, or none of its hosts are left.
func UsedAs(st *state.VM, sec Secret) (Secret, bool) {
	b, _ := Of(st)
	i := slices.IndexFunc(b.Yours, func(y BoundSecret) bool { return y.Name == sec.Name })
	if i < 0 || b.Yours[i].ID != sec.ID {
		return Secret{}, false
	}
	sec.Hosts = slices.DeleteFunc(slices.Clone(sec.Hosts), func(h string) bool { return !slices.Contains(b.Yours[i].Hosts, h) })
	return sec, len(sec.Hosts) > 0
}

// Stale reports whether the VM st names a secret of yours that it wasn't
// bound to as it's saved now: bound to another definition by its name (a
// secret saved again after devvm secret rm), or to none.
func Stale(st *state.VM, sec Secret) bool {
	b, _ := Of(st)
	i := slices.IndexFunc(b.Yours, func(y BoundSecret) bool { return y.Name == sec.Name })
	return i < 0 || b.Yours[i].ID != sec.ID
}

// Logins returns the login kinds a VM with these shared tools uses, in
// agents.LoginKinds' order: its agents' that have a login file in devvm's
// store, Claude Code's OAuth login only when it's the Claude Code login
// VMs get (ClaudeLogin), never with the Claude Code token.
func (s *Store) Logins(ctx context.Context, tools []string) []string {
	as := image.AgentsOf(tools)
	var kinds []string
	for _, k := range agents.LoginKinds() {
		if !slices.Contains(as, agentOf(k)) || !s.HasLogin(k) || !ClaudeOf(s).used(ctx, k) {
			continue
		}
		kinds = append(kinds, k)
	}
	return kinds
}

// WriteFiles writes the proxy's view of a VM's secrets: the bindings of
// those with a value saved and of its agents' logins, and the hosts they
// all imply, on 443 only (where the proxy puts values in). The proxy reads
// both again when they change. Claude Code's OAuth login has a binding of
// its own (Claude.bindings).
func (s *Store) WriteFiles(ctx context.Context, st *state.VM, salt string) error {
	var logins []proxy.Binding
	names := tools.Names(st)
	// A login that can't be read has no bindings.
	for _, kind := range s.Logins(ctx, names) {
		if ClaudeOf(s).bound(kind) {
			continue
		}
		bs, err := s.login(kind, salt).Bindings()
		if err != nil {
			continue
		}
		logins = append(logins, bs...)
	}
	bs, err := ClaudeOf(s).bindings(ctx, names, salt)
	if err != nil {
		return err
	}
	return s.writeFiles(ctx, st, salt, append(logins, bs...))
}

// RefreshFiles writes a VM's secret files again for its secrets as they
// are now, keeping its logins' bindings as WriteFiles last wrote them:
// they come from the login files in the home of the devvm that started
// its proxy.
func (s *Store) RefreshFiles(ctx context.Context, st *state.VM, salt string) error {
	l, err := proxy.ReadBindings(s.Dirs.VM(st.Name()).Secrets())
	if err != nil {
		return err
	}
	logins := slices.DeleteFunc(l.Bindings, func(b proxy.Binding) bool { return b.Kind != proxy.KindLogin })
	return s.writeFiles(ctx, st, salt, logins)
}

// writeFiles writes the VM's secret files: the bindings of its secrets
// with a value saved, then its logins', and the allowlist they imply,
// every bound secret's hosts (saved or not), its logins' and its agents'
// token endpoints' (loginHosts), on 443.
func (s *Store) writeFiles(ctx context.Context, st *state.VM, salt string, logins []proxy.Binding) error {
	var list []proxy.Binding
	var hosts []string
	all, err := YoursOf(s).Using(Names(st))
	if err != nil {
		return err
	}
	for _, name := range Names(st) {
		sec, ok := Find(all, name)
		if ok && sec.Custom {
			sec, ok = UsedAs(st, sec)
		}
		if !ok {
			continue
		}
		hosts = append(hosts, sec.Hosts...)
		// The claude secret's binding gets the OAuth login, without a token (Get).
		if _, err := s.Value(ctx, name); errors.Is(err, ErrNone) && !ClaudeOf(s).OAuthInstead(name) {
			continue
		} else if err != nil && !errors.Is(err, ErrNone) {
			return err
		}
		list = append(list, proxy.Binding{
			Name: name, Kind: proxy.KindSecret, Hosts: sec.Hosts, Header: sec.Header, Placeholder: sec.Placeholder(salt),
			Path: sec.Path, Methods: sec.Methods, Scheme: sec.Scheme, BasicUser: sec.BasicUser, ID: sec.ID,
		})
	}
	for _, b := range logins {
		hosts = append(hosts, b.Hosts...)
	}
	// Its agents log in through its proxy, on every tier.
	hosts = append(hosts, loginHosts(st)...)
	list = append(list, logins...)
	// The VM's secret bindings, for its proxy (Config.Secrets): names,
	// kinds, hosts, headers and placeholders, never values; and the
	// allowlist they imply.
	files := s.Dirs.VM(st.Name())
	if err := state.PrivateDir(files.Run()); err != nil {
		return err
	}
	if err := state.WriteFile(files.Secrets(), proxy.BindingList{Bindings: list}.Encode()); err != nil {
		return err
	}
	var pol []byte
	for _, l := range proxy.InterceptLines(hosts) {
		pol = append(pol, l+"\n"...)
	}
	return state.WriteFile(files.SecretsPolicy(), pol)
}
