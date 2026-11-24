// Package agents is the coding agents devvm supports, one Def each, in Defs: every list of the
// agents, and every fact devvm keeps of one (its shared tool, its settings, logins and history,
// the environment its VMs get, how devvm agent login logs it in, its row in the docs' Supported
// agents table), comes from there. What one agent alone does (Claude Code's OAuth login,
// opencode's database, pi's settings filter) is code of its own elsewhere, in files named for
// the agent, which name it by its constant here: the structure test fails on code elsewhere that
// compares with one. Each agent's shared tool is also its line in
// assets/shared-tools/mise.toml's [tool_alias] and its policy, assets/shared-tools/<Name>.policy,
// and its login files' formats are internal/agentlogin's (TestDefsComplete checks them all).
// Providers are the model providers whose API keys the VM's proxy puts in for opencode and pi.
// Adding an agent is a Def here and those files; Contributing's "Adding a coding agent" lists
// them.
package agents

import (
	"path"
	"slices"
	"strings"
)

// The agents' names: what special-cased code elsewhere names one by.
const (
	Claude   = "claude"
	Codex    = "codex"
	Opencode = "opencode"
	Pi       = "pi"
	Cursor   = "cursor"
)

// Folders of the agents' below the home folder that code of one agent's own reads.
const (
	// ClaudeJSON is Claude Code's .claude.json, its settings, account state and MCP servers,
	// which devvm merges into the VM's own rather than copying it.
	ClaudeJSON = ".claude.json"
	// ClaudePlugins is Claude Code's plugins folder, whose JSON files name their paths: in a VM,
	// the VM's own, and the store's its seed (Seed).
	ClaudePlugins = ".claude/plugins"
	// ClaudePluginSeed is where a VM links the store's plugins folder, its Claude Code's seed.
	ClaudePluginSeed = ".claude/plugins-seed"
	// OpencodeConfig is opencode's config folder: its settings, every entry but node_modules
	// and lock files (WholeFolder).
	OpencodeConfig = ".config/opencode"
	// OpencodeData is opencode's data folder, which holds its login file and its database.
	OpencodeData = ".local/share/opencode"
	// OpencodeState is opencode's state folder: its prompt history (prompt-history.jsonl) and
	// the TUI's state, beside what is its machine's own, which an export leaves out.
	OpencodeState = ".local/state/opencode"
	// PiFolder is pi's agent folder: its settings, its logins (auth.json) and its sessions.
	PiFolder = ".pi/agent"
	// PiSettings is pi's settings.json, whose VM copy leaves out what undoes devvm's settings
	// (agentlogin.PiSettings).
	PiSettings = PiFolder + "/settings.json"
)

// Format is how a VM's copy of a settings file is made.
type Format string

// The formats of settings files.
const (
	// AsIs is copied as it is, such as Markdown.
	AsIs Format = ""
	// JSON is parsed whole, its secret-looking values taken out (agentlogin.Redact), and
	// written again as JSON: JSONC's comments go.
	JSON Format = "json"
	// TOML is parsed whole, its secret-looking values taken out, and written again: its
	// comments go.
	TOML Format = "toml"
)

// File is one of an agent's settings files, by its path relative to the home folder, and how
// its VM copy is made.
type File struct {
	Rel    string
	Format Format
}

// Login is one of an agent's login files: its kind (agentlogin.File's Kind, its bindings'
// Agent, whose format agentlogin has), and its path relative to the home folder, where the
// agent keeps it and devvm's store of the agent's logins keeps it too.
type Login struct {
	Kind string
	Rel  string
}

// Keys are the API keys devvm agent login --api-key saves for an agent.
type Keys int

// The kinds of API keys.
const (
	// NoKeys: devvm agent login saves none.
	NoKeys Keys = iota
	// OneKey: one key for the agent's own provider, given without --provider (Codex's OpenAI
	// key).
	OneKey
	// ProviderKeys: a key for each of Providers, named with --provider (agentlogin.KeyProviders).
	ProviderKeys
)

// Provider is a model provider whose API keys the VM's proxy puts in for the agents with a key
// for each provider (ProviderKeys).
type Provider struct {
	// ID is its entry's name in those agents' login files, and --provider's value.
	ID string
	// Display is its name as people know it: OpenRouter.
	Display string
	// Hosts are the hosts the agents send its API key to, and no others: the proxy puts the key
	// in for them alone, and allows them.
	Hosts []string
	// Path, when set, is the path below which the agents send its API key on those hosts, and
	// the proxy puts the key in for paths below it alone: it starts and ends with /, as
	// OpenRouter's /api/ does, so another of the host's pages never gets the key.
	Path string
	// Headers are the request headers the agents send the key in, each a binding of its own,
	// where the proxy swaps the placeholder for it: the header's value is the placeholder alone,
	// or for Authorization, Bearer and the placeholder. Most providers have one; OpenRouter's
	// Anthropic models take the key in x-api-key, as Anthropic's API does.
	Headers []string
	// Prefix starts the VM's placeholder for its key, as an agent may tell its key from another
	// credential by how it starts: pi 1.5.0 takes an OpenAI key that doesn't start with sk- for
	// a ChatGPT sign-in's token, and leaves max_output_tokens, temperature and prompt caching's
	// settings out of its requests.
	Prefix string
}

// Providers are the providers whose API keys the proxy puts in, sorted by ID: every list of
// them comes from here, and the docs' table of them names each (TestProvidersComplete). Each
// is checked against opencode 2.2.6's and pi 1.5.0's own source (2026-11): both name it by the
// same ID, send its key in those headers alone, as the whole value or after Bearer, never in a
// URL, and to those hosts alone (Gemini's API, not Vertex AI's), below Path where it's set
// (OpenRouter's /api/), and neither checks how a key starts but where Prefix says. OpenRouter's
// x-api-key is pi's for its anthropic/* models (api anthropic-messages, base
// https://openrouter.ai/api, so /api/v1/messages), and opencode's when models.dev lists a model's
// messages API first (route() in packages/core/src/models-dev.ts takes the first).
//
// A bound key still lets the VM mint credentials that work without the proxy, which the proxy
// allows because opencode 2.2.6's experimental realtime mode mints them itself
// (packages/ai/src/experimental/realtime.ts): Gemini's POST /v1*/auth_tokens, ephemeral Live API
// tokens of up to about 20 h whose expireTime and uses the VM sets; xAI's POST
// /v1/realtime/client_secrets, its expiry the VM's choice; and OpenAI's
// /v1/realtime/client_secrets, up to 2 h. Some providers' own tools also fetch URLs the VM
// names (Gemini's url_context and Search grounding, xAI's live search, Groq's compound,
// Mistral's agents' web_search), a way out the allowlist doesn't cover, as Anthropic's and
// OpenAI's are. The safety docs' attack vectors name both.
var Providers = []Provider{
	{ID: "anthropic", Display: "Anthropic", Hosts: []string{"api.anthropic.com"}, Headers: []string{"x-api-key"}},
	{ID: "deepseek", Display: "DeepSeek", Hosts: []string{"api.deepseek.com"}, Headers: []string{"Authorization"}},
	{ID: "google", Display: "Google Gemini", Hosts: []string{"generativelanguage.googleapis.com"}, Headers: []string{"x-goog-api-key"}},
	{ID: "groq", Display: "Groq", Hosts: []string{"api.groq.com"}, Headers: []string{"Authorization"}},
	{ID: "mistral", Display: "Mistral", Hosts: []string{"api.mistral.ai"}, Headers: []string{"Authorization"}},
	{ID: "openai", Display: "OpenAI", Hosts: []string{"api.openai.com"}, Headers: []string{"Authorization"}, Prefix: "sk-"},
	{ID: "openrouter", Display: "OpenRouter", Hosts: []string{"openrouter.ai"}, Path: "/api/", Headers: []string{"Authorization", "x-api-key"}},
	{ID: "xai", Display: "xAI", Hosts: []string{"api.x.ai"}, Headers: []string{"Authorization"}},
}

// ProviderIDs are the providers' IDs, sorted.
func ProviderIDs() []string {
	out := make([]string, 0, len(Providers))
	for _, p := range Providers {
		out = append(out, p.ID)
	}
	return out
}

// ProviderOf returns the provider whose ID is id, false when none is.
func ProviderOf(id string) (Provider, bool) {
	i := slices.IndexFunc(Providers, func(p Provider) bool { return p.ID == id })
	if i < 0 {
		return Provider{}, false
	}
	return Providers[i], true
}

// Env is a variable of the guest environment that every VM's shells get: its name, and its
// value, which with Home is a path in the VM's home folder, relative to it.
type Env struct {
	Name, Value string
	Home        bool
}

// Seed is one of an agent's settings folders, among its Dirs, that the agent reads in a VM as
// a read-only seed of a folder of the VM's own at the same path, rather than as that folder:
// Claude Code's plugins, whose folder Claude Code writes (each plugin's data, its installs, its
// marketplaces' refreshes), so it can't be the store's, which every VM shares read-only. The VM
// links the store's folder, mounted read-only as Dirs are, at Link, and the agent's Env names
// both.
type Seed struct {
	// Rel is the folder, relative to the home folder: the store's at that path below it, and in
	// a VM, the VM's own.
	Rel string
	// Link is where the VM's home links the store's folder: the seed.
	Link string
	// Entries are the folder's entries that a seed has, which devvm agent import copies of it
	// from this machine and devvm agent save from a VM: anything else in it is each machine's
	// own, such as each plugin's data, and stays there.
	Entries []string
}

// Seeds are every agent's seeds, in Defs' order.
func Seeds() []Seed {
	var out []Seed
	for _, d := range Defs {
		out = append(out, d.Seeds...)
	}
	return out
}

// SeedOf returns the seed whose Rel is rel, a folder relative to the home folder, false when
// none is.
func SeedOf(rel string) (Seed, bool) {
	for _, s := range Seeds() {
		if s.Rel == rel {
			return s, true
		}
	}
	return Seed{}, false
}

// MachineOwn reports whether rel, relative to the home folder, is each machine's own, which
// neither devvm agent import nor devvm agent save copies: in a seed's folder but in none of its
// Entries, or an entry of a WholeFolder agent's Folder that its FolderOwn names, or below one.
func MachineOwn(rel string) bool {
	for _, s := range Seeds() {
		if rest, ok := strings.CutPrefix(rel, s.Rel+"/"); ok {
			first, _, _ := strings.Cut(rest, "/")
			return !slices.Contains(s.Entries, first)
		}
	}
	for _, d := range Defs {
		if rest, ok := strings.CutPrefix(rel, d.Folder+"/"); ok && d.WholeFolder {
			first, _, _ := strings.Cut(rest, "/")
			return slices.ContainsFunc(d.FolderOwn, func(p string) bool {
				m, _ := path.Match(p, first)
				return m
			})
		}
	}
	return false
}

// MachineOwnFiles are every agent's FolderOwn, each relative to the home folder as a pattern of
// the same form, in Defs' order: what a VM's setup takes out of the VM when it's a copy that an
// earlier devvm gave it (guest/sync.sh's clean_machine_own).
func MachineOwnFiles() []string {
	var out []string
	for _, d := range Defs {
		for _, p := range d.FolderOwn {
			out = append(out, d.Folder+"/"+p)
		}
	}
	return out
}

// LoginCmd is an agent's own login, as devvm agent login runs it in the VM, in a login shell:
// each command line starts with the agent's command, or with env and its arguments.
type LoginCmd struct {
	// Argv is the login's command line, none for an agent without a login of its own.
	Argv []string
	// Device is the command line of its login with a code to type into a browser on any
	// machine (--device-code), so nothing is forwarded: none when it has no such login.
	Device []string
	// SetupToken is the command line that gets a long-lived token instead (--setup-token),
	// which devvm saves as the agent's Secret: none when it has no such token.
	SetupToken []string
	// Forward: its login in a browser on this machine comes back to the agent's server in the
	// VM, on a port devvm forwards while it runs (a ChatGPT login's).
	Forward bool
	// Paste: its login shows an address to open in a browser on any machine, whose code you
	// paste back on the terminal.
	Paste bool
}

// Run returns the command line of the login devvm agent login runs: with a code (device),
// or for a long-lived token (setupToken), when the agent has one.
func (l LoginCmd) Run(device, setupToken bool) []string {
	switch {
	case setupToken && l.SetupToken != nil:
		return slices.Clone(l.SetupToken)
	case device && l.Device != nil:
		return slices.Clone(l.Device)
	}
	return slices.Clone(l.Argv)
}

// Def is one coding agent.
type Def struct {
	// Name is its shared tool's name, as --agents and devvm tools name it, its command in the
	// VM, and its folder's name in devvm's stores.
	Name string
	// Display is the agent's name as people know it: Claude Code.
	Display string
	// Command is its command in the VM when it isn't Name: Cursor's CLI is cursor-agent (and
	// agent), as cursor is its editor's.
	Command string
	// Folder is the folder of the home folder its settings are in, which devvm agent import
	// copies them from, following no link out of it.
	Folder string
	// Dirs are its settings folders, relative to the home folder, mounted read-only from the
	// store and linked into the VM's home, at their path there, or a Seed's at its Link.
	Dirs []string
	// Seeds are its folders among Dirs that it reads in a VM as a seed of a folder of the
	// VM's own.
	Seeds []Seed
	// Files are its settings files, copied in on every launch.
	Files []File
	// Names are the tables of its settings files whose keys are names you choose, such as its
	// MCP servers' or its plugins' marketplaces', each a path of keys joined by dots, * standing
	// for any one key. A table under one of those names isn't judged by its name as the VM's copy
	// is made (agentlogin.Redact), so a server named secret-scanner keeps its settings, while its
	// own fields, such as its env or an apiKey, are judged as anywhere else.
	Names []string
	// NamedValues are the tables whose keys are names you choose and whose values are settings,
	// such as Claude Code's skillOverrides' "off": neither a table nor a setting's word in them,
	// such as off or name-only, is judged by its name, while any other value still is.
	NamedValues []string
	// WholeFolder: its settings are every entry of Folder, read as they are now: each folder
	// but node_modules is mounted, and each file but lock files is copied, a JSON, JSONC or
	// TOML one as a settings file, Markdown as it is, and any other withheld, as devvm can't
	// take secrets out of it.
	WholeFolder bool
	// FolderOwn are the names of a WholeFolder agent's Folder's entries that are each machine's
	// own (MachineOwn), as path.Match patterns, such as opencode's service.json, which holds
	// its background service's password: never settings, so devvm agent import and devvm agent
	// save never copy them, no VM gets this machine's, and each VM's agent makes its own.
	FolderOwn []string
	// Logins are its login files, the one devvm agent import copies from the home folder
	// first.
	Logins []Login
	// History is what devvm rm exports of it from a VM, relative to the home folder, and
	// HistoryExcludes what of that an export leaves out and an import never writes: its logins.
	History, HistoryExcludes []string
	// HistoryOwn is what of its History is the VM's own as a machine, such as a private key
	// that other machines trust, or what another VM must never take from it, such as a git
	// repository whose config can name a command that the agent's git runs: an export leaves
	// it out and an import never writes it, as HistoryExcludes, but a VM made again keeps
	// it, as that's the same machine.
	HistoryOwn []string
	// Env is the guest environment its VMs' shells get: its self-updates and telemetry off.
	Env []Env
	// Secret is the secret (secrets.Known) every VM with the agent uses: Claude Code's token.
	Secret string
	// GlibcOnly: its releases are built for glibc alone, so a VM on a musl base (Alpine)
	// can't have it.
	GlibcOnly bool
	// Login is the agent's own login, as devvm agent login runs it in the VM: none for an
	// agent devvm saves only API keys for.
	Login LoginCmd
	// ChatGPT: its login file holds a ChatGPT login, which devvm agent login makes in a
	// browser on this machine or with a code (--device-code).
	ChatGPT bool
	// Keys are the API keys devvm agent login saves for it.
	Keys Keys
	// PassKeys: config.toml's [pass-keys] takes it, so the providers it lists for the agent
	// go to its VMs as they are, and the logins devvm neither binds nor passes are withheld.
	PassKeys bool
	// Docs is its row in the docs' Supported agents table, which mise run cli-docs writes.
	Docs Docs
}

// Docs is what an agent's row in the docs' Supported agents table says, beside its name, its
// Display name and its policy's hosts.
type Docs struct {
	// Site is the address its Display name links to, for an agent people may not know.
	Site string
	// Adds is what devvm adds: which release, and what devvm turns off in it.
	Adds string
	// Reaches is what its allowlist's hosts are for, which the table lists after it.
	Reaches string
}

// Defs are the agents, in the order every list of them has.
var Defs = []Def{
	{
		Name: Claude, Display: "Claude Code", Folder: ".claude",
		Dirs:  []string{".claude/skills", ".claude/agents", ".claude/commands", ".claude/output-styles", ClaudePlugins},
		Files: []File{{".claude/settings.json", JSON}, {".claude/CLAUDE.md", AsIs}},
		// From code.claude.com/docs/en/settings and /mcp (2026-11): mcpServers is
		// ~/.claude.json's, whose copy devvm agent save's diff shows.
		Names: []string{
			"extraKnownMarketplaces", "additionalMarketplaces", "pluginConfigs", "pluginConfigs.*.mcpServers",
			"modelSettings", "modelPricing.overrides", "mcpServers", "managedMcpServers",
		},
		NamedValues:     []string{"enabledPlugins", "skillOverrides", "modelOverrides"},
		Logins:          []Login{{Claude, ClaudeJSON}, {ClaudeOAuth, ".claude/.credentials.json"}},
		History:         []string{".claude", ClaudeJSON},
		HistoryExcludes: []string{".claude/.credentials.json"},
		// From code.claude.com/docs/en/plugins/org.md's seed containers (2026-11): a seed is laid
		// out as the plugins folder, its marketplaces' records, their copies and the plugins'
		// cache, which Claude Code never writes; the plugins folder's other entries are each
		// machine's own (plugins/loading.md).
		Seeds: []Seed{{Rel: ClaudePlugins, Link: ClaudePluginSeed, Entries: []string{"known_marketplaces.json", "marketplaces", "cache"}}},
		Env: []Env{
			{Name: "DISABLE_AUTOUPDATER", Value: "1"},
			{Name: "DISABLE_TELEMETRY", Value: "1"},
			{Name: "DISABLE_ERROR_REPORTING", Value: "1"},
			{Name: "CLAUDE_CODE_PLUGIN_CACHE_DIR", Value: ClaudePlugins, Home: true},
			{Name: "CLAUDE_CODE_PLUGIN_SEED_DIR", Value: ClaudePluginSeed, Home: true},
		},
		Secret: Claude,
		// Its subscription's login, or claude setup-token's token, never with a
		// CLAUDE_CODE_OAUTH_TOKEN of the VM's environment, which Claude Code would use instead.
		Login: LoginCmd{
			Argv:       []string{"env", "-u", "CLAUDE_CODE_OAUTH_TOKEN", Claude, "auth", "login", "--claudeai"},
			SetupToken: []string{"env", "-u", "CLAUDE_CODE_OAUTH_TOKEN", Claude, "setup-token"},
			Paste:      true,
		},
		Docs: Docs{
			Adds:    "the newest release, with its auto-update off, since the shared copy is read-only, and its telemetry off",
			Reaches: "Anthropic's API, Claude's login, Claude Code's downloads and claude.ai's connectors",
		},
	},
	{
		Name: Codex, Display: "Codex", Folder: ".codex",
		Dirs:  []string{".codex/prompts", ".codex/skills"},
		Files: []File{{".codex/config.toml", TOML}, {".codex/AGENTS.md", AsIs}},
		// From its configuration reference (2026-11), and hooks.state, where it records which
		// hooks you trusted, by the hook's plugin and event.
		Names: []string{
			"mcp_servers", "mcp_servers.*.tools", "model_providers", "profiles", "projects", "marketplaces",
			"plugins", "plugins.*.mcp_servers", "plugins.*.mcp_servers.*.tools", "apps", "apps.*.tools", "agents",
			"permissions", "hooks.state", "otel.exporter", "otel.trace_exporter", "desktop.custom_file_handlers",
			"browser_use.origins", "tui.presets",
		},
		NamedValues: []string{
			"shell_environment_policy.filters", "notice.model_migrations", "permissions.*.filesystem", "permissions.*.filesystem.*",
			"permissions.*.network.domains", "permissions.*.network.unix_sockets", "permissions.*.workspace_roots",
		},
		Logins:          []Login{{Codex, ".codex/auth.json"}},
		History:         []string{".codex"},
		HistoryExcludes: []string{".codex/auth.json"},
		// Its ChatGPT login, kept in its auth.json, which the VM's proxy fills with
		// placeholders, never in a keyring.
		Login: LoginCmd{
			Argv:    []string{Codex, "-c", `cli_auth_credentials_store="file"`, "login"},
			Device:  []string{Codex, "-c", `cli_auth_credentials_store="file"`, "login", "--device-auth"},
			Forward: true,
		},
		ChatGPT: true, Keys: OneKey,
		Docs: Docs{
			Adds:    "the newest release, with its analytics and metrics off unless your config sets them",
			Reaches: "the OpenAI API and the ChatGPT login",
		},
	},
	{
		Name: Opencode, Display: "opencode", Folder: OpencodeConfig, WholeFolder: true,
		// From opencode.ai/docs (2026-11): 2.x's names and 1.x's singular ones, which it still
		// reads, with 1.x's permission tables of patterns.
		Names: []string{
			"mcp", "mcp.servers", "provider", "providers", "provider.*.models", "providers.*.models",
			"agent", "agents", "command", "commands", "mode", "formatter", "lsp", "references",
		},
		NamedValues: []string{"permission", "permission.*", "agent.*.permission", "agent.*.permission.*"},
		// From opencode v2.4.1's source (service-config.ts), its background service's settings:
		// service.json on its latest, dev, beta and next channels, service-<channel>.json on any
		// other (and an earlier build's service-<hash>.json), each written through
		// <file>.<pid>.tmp. They hold the service's password, which opencode makes when the file
		// has none, and with remote access on, the secret route it's served on.
		FolderOwn: []string{"service.json", "service-*.json", "service.json.*", "service-*.json.*"},
		Logins:    []Login{{Opencode, OpencodeData + "/auth.json"}},
		History:   []string{OpencodeData, OpencodeState},
		HistoryExcludes: []string{
			OpencodeData + "/auth.json", OpencodeData + "/account.json", OpencodeData + "/mcp-auth.json", OpencodeData + "/bin",
		},
		// From opencode v2.4.1's source, what of its state folder is its machine's own:
		// machine/home-key, the private Ed25519 key of its home, which every machine it pairs
		// trusts and which it never replaces; its background service's registration, with the
		// service's password, service.json on its latest, dev, beta and next channels and
		// service-<channel>.json on the others (service-registration.ts, registration.rs),
		// written through <file>.<id>.tmp, beside <file>.pty-handoff, a terminal's handoff
		// ticket, written through <file>.pty-handoff.<uuid>.tmp (handoff.rs); and pty, its
		// terminals' daemons' sockets and the access tokens they take. Of its data folder, its git
		// repositories, whose config and attributes can name commands that git runs, such as a
		// clean filter or core.fsmonitor, which opencode's own git commands would then run in
		// another VM: snapshot, a repository for each project's snapshots, which opencode uses
		// again when it's there (snapshot.ts), repos, its cache of repositories, and worktree,
		// its git worktrees. A pattern holds *, as GNU tar's --exclude and the shell read it.
		HistoryOwn: []string{
			OpencodeState + "/machine", OpencodeState + "/service.json", OpencodeState + "/service-*.json",
			OpencodeState + "/service.json.*", OpencodeState + "/service-*.json.*", OpencodeState + "/pty",
			OpencodeData + "/snapshot", OpencodeData + "/repos", OpencodeData + "/worktree",
		},
		Env: []Env{{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"}},
		// Its Codex login to OpenAI (its methods chatgpt-browser and chatgpt-headless), whose
		// token endpoint the proxy captures: its Sign in with ChatGPT gets its tokens from
		// another endpoint, whose replies the proxy refuses.
		Login: LoginCmd{
			Argv:    []string{Opencode, "auth", "login", "openai", "--method", "chatgpt-browser"},
			Device:  []string{Opencode, "auth", "login", "openai", "--method", "chatgpt-headless"},
			Forward: true,
		},
		ChatGPT: true, Keys: ProviderKeys, PassKeys: true,
		Docs: Docs{
			Adds:    "the newest 2.x release, because opencode marks its 1.x releases as its latest on GitHub",
			Reaches: "opencode's own service and its model catalog",
		},
	},
	{
		Name: Pi, Display: "pi", Folder: PiFolder,
		Dirs: []string{PiFolder + "/extensions", PiFolder + "/skills", PiFolder + "/prompts", PiFolder + "/themes"},
		Files: []File{
			{PiSettings, JSON},
			{PiFolder + "/keybindings.json", AsIs},
			{PiFolder + "/models.json", JSON},
			{PiFolder + "/mcp.json", JSON},
			{PiFolder + "/AGENTS.md", AsIs},
			{PiFolder + "/SYSTEM.md", AsIs},
			{PiFolder + "/APPEND_SYSTEM.md", AsIs},
		},
		// From its docs' settings.md, models.md and mcp.md (2026-11).
		Names:       []string{"providers", "providers.*.modelOverrides", "mcpServers", "compaction.modelOverrides"},
		NamedValues: []string{"modelThinkingLevels", "mcpServers.*.toolExposure"},
		Logins:      []Login{{Pi, PiFolder + "/auth.json"}},
		History:     []string{PiFolder + "/sessions"},
		Env:         []Env{{Name: "PI_SKIP_VERSION_CHECK", Value: "1"}, {Name: "PI_TELEMETRY", Value: "0"}},
		GlibcOnly:   true, Keys: ProviderKeys, PassKeys: true,
		Docs: Docs{
			Site:    "https://pi.dev",
			Adds:    "the newest release, with its check for a newer pi and its install report off, and `fd`, which pi finds files with, in base",
			Reaches: "pi's model catalog",
		},
	},
	// Cursor's CLI, checked against its Linux package 2026.11.17-f532c45 (read, not run): its
	// settings in ~/.cursor (CURSOR_CONFIG_DIR unset), its login in ~/.config/cursor
	// (XDG_CONFIG_HOME's), its chats in ~/.cursor/chats. Its login shows an address to open in a
	// browser on any machine and polls for the login (NO_OPEN_BROWSER=1 prints it rather than
	// opening one), so nothing comes back to the VM. It has no variable that turns off its
	// self-update or its telemetry: a VM's setup sets its channel to static in cli-config.json,
	// which its updates skip.
	{
		Name: Cursor, Display: "Cursor CLI", Command: "cursor-agent", Folder: ".cursor",
		Dirs:      []string{".cursor/rules", ".cursor/commands"},
		Files:     []File{{".cursor/cli-config.json", JSON}, {".cursor/mcp.json", JSON}},
		Names:     []string{"mcpServers"},
		Logins:    []Login{{Cursor, ".config/cursor/auth.json"}},
		History:   []string{".cursor/chats"},
		GlibcOnly: true,
		Login:     LoginCmd{Argv: []string{"env", "NO_OPEN_BROWSER=1", "cursor-agent", "login"}},
		Docs: Docs{
			Site:    "https://cursor.com/cli",
			Adds:    "the release devvm pins, run as `cursor-agent` or `agent`, with its self-update off through its static channel",
			Reaches: "Cursor's API, its agent's hosts and its codebase indexing",
		},
	},
}

// ClaudeOAuth is the kind of Claude Code's OAuth login file, its .credentials.json, beside its
// .claude.json (kind Claude), which holds its MCP servers' secrets.
const ClaudeOAuth = "claude-oauth"

// Names are the agents' names, in Defs' order.
func Names() []string { return Select(func(Def) bool { return true }) }

// Select returns the names of the agents keep keeps, in Defs' order.
func Select(keep func(Def) bool) []string {
	var out []string
	for _, d := range Defs {
		if keep(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

// Get returns the agent named name, false when none is.
func Get(name string) (Def, bool) {
	i := slices.IndexFunc(Defs, func(d Def) bool { return d.Name == name })
	if i < 0 {
		return Def{}, false
	}
	return Defs[i], true
}

// Of returns the agent named name, which must be one: code of one agent's own asks for it by
// its constant.
func Of(name string) Def {
	d, ok := Get(name)
	if !ok {
		panic("no agent " + name)
	}
	return d
}

// LoginKinds are every agent's login files' kinds, in Defs' order.
func LoginKinds() []string {
	var out []string
	for _, d := range Defs {
		for _, l := range d.Logins {
			out = append(out, l.Kind)
		}
	}
	return out
}

// LoginOf returns the login file of kind, and the agent whose it is.
func LoginOf(kind string) (Login, Def, bool) {
	for _, d := range Defs {
		for _, l := range d.Logins {
			if l.Kind == kind {
				return l, d, true
			}
		}
	}
	return Login{}, Def{}, false
}

// LoginAgents are the agents devvm agent login logs in: those with a login of their own or API
// keys.
func LoginAgents() []string {
	return Select(func(d Def) bool { return d.Login.Argv != nil || d.Keys != NoKeys })
}

// KeysOnly reports whether devvm agent login saves only API keys for the
// agent: it has no login of its own (pi).
func (d Def) KeysOnly() bool { return d.Login.Argv == nil && d.Keys != NoKeys }

// file returns the agent's settings file at rel, by its path relative to the home folder, if
// it's one of its Files.
func (d Def) file(rel string) (File, bool) {
	if i := slices.IndexFunc(d.Files, func(f File) bool { return f.Rel == rel }); i >= 0 {
		return d.Files[i], true
	}
	return File{}, false
}

// SettingsFormat returns the format of an agent's settings file whose VM copy devvm makes from
// what it parsed, by its path relative to the home folder: one of an agent's Files of JSON or
// TOML, or a JSON, JSONC or TOML file in a WholeFolder agent's Folder. False for any other
// file, which goes as it is or is withheld (Withheld).
func SettingsFormat(rel string) (Format, bool) {
	for _, d := range Defs {
		if f, ok := d.file(rel); ok {
			return f.Format, f.Format != AsIs
		}
		if d.WholeFolder && path.Dir(rel) == d.Folder {
			switch path.Ext(rel) {
			case ".json", ".jsonc":
				return JSON, true
			case ".toml":
				return TOML, true
			}
		}
	}
	return AsIs, false
}

// NameTables are every agent's Names and NamedValues, each path split into its keys, which
// agentlogin.Redact applies to every agent's settings files alike.
func NameTables() (names, values [][]string) {
	split := func(l []string) [][]string {
		var out [][]string
		for _, p := range l {
			out = append(out, strings.Split(p, "."))
		}
		return out
	}
	for _, d := range Defs {
		names = append(names, split(d.Names)...)
		values = append(values, split(d.NamedValues)...)
	}
	return names, values
}

// Withheld reports whether a VM doesn't get an agent's file, by its path relative to the home
// folder, as devvm can't take secrets out of it: a file of a WholeFolder agent's Folder that's
// neither a settings file (SettingsFormat) nor Markdown, such as a .env.
func Withheld(rel string) bool {
	if _, ok := SettingsFormat(rel); ok {
		return false
	}
	for _, d := range Defs {
		if d.WholeFolder && path.Dir(rel) == d.Folder && path.Ext(rel) != ".md" {
			return true
		}
	}
	return false
}

// History are every agent's history paths, in Defs' order: what devvm rm exports.
func History() []string {
	var out []string
	for _, d := range Defs {
		out = append(out, d.History...)
	}
	return out
}

// LoginFiles are every agent's login files that aren't part of its history, in Defs' order:
// what a VM made again never takes from the old VM's home, as the store gives it its own.
// Claude Code's .claude.json is history too, for its per-folder state, so it isn't one.
func LoginFiles() []string {
	var out []string
	for _, d := range Defs {
		for _, l := range d.Logins {
			if !slices.Contains(d.History, l.Rel) {
				out = append(out, l.Rel)
			}
		}
	}
	return out
}

// HistoryExcludes are what an export of every agent's history leaves out and an import never
// writes, in Defs' order: each agent's HistoryExcludes, then its HistoryOwn.
func HistoryExcludes() []string {
	var out []string
	for _, d := range Defs {
		out = append(out, d.HistoryExcludes...)
		out = append(out, d.HistoryOwn...)
	}
	return out
}

// HistoryLogins are every agent's HistoryExcludes alone, in Defs' order, without its
// HistoryOwn: what a VM made again leaves out of the old VM's history, as it's the same
// machine.
func HistoryLogins() []string {
	var out []string
	for _, d := range Defs {
		out = append(out, d.HistoryExcludes...)
	}
	return out
}

// GuestEnv is the guest environment of every agent's, in Defs' order: what every VM's shells
// get, whichever agents it has, as a VM can gain one at any launch.
func GuestEnv() []Env {
	var out []Env
	for _, d := range Defs {
		out = append(out, d.Env...)
	}
	return out
}
