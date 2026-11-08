// Package agents is the coding agents devvm supports, one Def each, in Defs: every list of the
// agents, and every fact devvm keeps of one (its shared tool, its settings, logins and history,
// the environment its VMs get, how devvm agent login logs it in), comes from there. What one
// agent alone does (Claude Code's OAuth login, opencode's database, pi's settings filter) is
// code of its own elsewhere, which names the agent by its constant here. Each agent's shared
// tool is also its line in assets/shared-tools/mise.toml's [tool_alias] and its policy,
// assets/shared-tools/<Name>.policy; its login files' formats are internal/agentlogin's; and
// its row in the docs' Supported agents table names it (TestDefsComplete checks them all).
// Adding an agent is a Def here and those files; Contributing's "Adding a coding agent" lists
// them.
package agents

import (
	"path"
	"slices"
)

// The agents' names: what special-cased code elsewhere names one by.
const (
	Claude   = "claude"
	Codex    = "codex"
	Opencode = "opencode"
	Pi       = "pi"
)

// Folders of the agents' below the home folder that code of one agent's own reads.
const (
	// ClaudeJSON is Claude Code's .claude.json, its settings, account state and MCP servers,
	// which devvm merges into the VM's own rather than copying it.
	ClaudeJSON = ".claude.json"
	// ClaudePlugins is Claude Code's plugins folder, whose JSON files name their paths.
	ClaudePlugins = ".claude/plugins"
	// OpencodeConfig is opencode's config folder: its settings, every entry but node_modules
	// and lock files (WholeFolder).
	OpencodeConfig = ".config/opencode"
	// OpencodeData is opencode's data folder, which holds its login file and its database.
	OpencodeData = ".local/share/opencode"
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
	// ProviderKeys: a key for each provider whose hosts devvm knows, named with --provider
	// (agentlogin.KeyProviders).
	ProviderKeys
)

// Env is a variable of the guest environment that every VM's shells get.
type Env struct{ Name, Value string }

// Def is one coding agent.
type Def struct {
	// Name is its shared tool's name, as --agents and devvm tools name it, its command in the
	// VM, and its folder's name in devvm's stores.
	Name string
	// Display is the agent's name as people know it: Claude Code.
	Display string
	// Folder is the folder of the home folder its settings are in, which devvm agent import
	// copies them from, following no link out of it.
	Folder string
	// Dirs are its settings folders, relative to the home folder, mounted read-only from the
	// store and linked into the VM's home.
	Dirs []string
	// Files are its settings files, copied in on every launch.
	Files []File
	// WholeFolder: its settings are every entry of Folder, read as they are now: each folder
	// but node_modules is mounted, and each file but lock files is copied, a JSON, JSONC or
	// TOML one as a settings file, Markdown as it is, and any other withheld, as devvm can't
	// take secrets out of it.
	WholeFolder bool
	// Logins are its login files, the one devvm agent import copies from the home folder
	// first.
	Logins []Login
	// History is what devvm rm exports of it from a VM, relative to the home folder, and
	// HistoryExcludes what of that an export leaves out: its logins.
	History, HistoryExcludes []string
	// Env is the guest environment its VMs' shells get: its self-updates and telemetry off.
	Env []Env
	// Secret is the secret (secrets.Known) every VM with the agent uses: Claude Code's token.
	Secret string
	// GlibcOnly: its releases are built for glibc alone, so a VM on a musl base (Alpine)
	// can't have it.
	GlibcOnly bool
	// OwnLogin: devvm agent login runs the agent's own login, as it is (Claude Code's).
	OwnLogin bool
	// ChatGPT: devvm agent login logs it in to ChatGPT, in a browser on this machine or with
	// a code (--device-code).
	ChatGPT bool
	// Keys are the API keys devvm agent login saves for it.
	Keys Keys
	// PassKeys: config.toml's [pass-keys] takes it, so the providers it lists for the agent
	// go to its VMs as they are, and the logins devvm neither binds nor passes are withheld.
	PassKeys bool
}

// Defs are the agents, in the order every list of them has.
var Defs = []Def{
	{
		Name: Claude, Display: "Claude Code", Folder: ".claude",
		Dirs:            []string{".claude/skills", ".claude/agents", ".claude/commands", ".claude/output-styles", ClaudePlugins},
		Files:           []File{{".claude/settings.json", JSON}, {".claude/CLAUDE.md", AsIs}},
		Logins:          []Login{{Claude, ClaudeJSON}, {ClaudeOAuth, ".claude/.credentials.json"}},
		History:         []string{".claude", ClaudeJSON},
		HistoryExcludes: []string{".claude/.credentials.json"},
		Env:             []Env{{"DISABLE_AUTOUPDATER", "1"}, {"DISABLE_TELEMETRY", "1"}, {"DISABLE_ERROR_REPORTING", "1"}},
		Secret:          Claude,
		OwnLogin:        true,
	},
	{
		Name: Codex, Display: "Codex", Folder: ".codex",
		Dirs:            []string{".codex/prompts", ".codex/skills"},
		Files:           []File{{".codex/config.toml", TOML}, {".codex/AGENTS.md", AsIs}},
		Logins:          []Login{{Codex, ".codex/auth.json"}},
		History:         []string{".codex"},
		HistoryExcludes: []string{".codex/auth.json"},
		ChatGPT:         true, Keys: OneKey,
	},
	{
		Name: Opencode, Display: "opencode", Folder: OpencodeConfig, WholeFolder: true,
		Logins:  []Login{{Opencode, OpencodeData + "/auth.json"}},
		History: []string{OpencodeData, ".local/state/opencode"},
		HistoryExcludes: []string{
			OpencodeData + "/auth.json", OpencodeData + "/account.json", OpencodeData + "/mcp-auth.json", OpencodeData + "/bin",
		},
		Env:     []Env{{"OPENCODE_DISABLE_AUTOUPDATE", "1"}},
		ChatGPT: true, Keys: ProviderKeys, PassKeys: true,
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
		Logins:    []Login{{Pi, PiFolder + "/auth.json"}},
		History:   []string{PiFolder + "/sessions"},
		Env:       []Env{{"PI_SKIP_VERSION_CHECK", "1"}, {"PI_TELEMETRY", "0"}},
		GlibcOnly: true, Keys: ProviderKeys, PassKeys: true,
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

// LoginAgents are the agents devvm agent login logs in: those with a login of their own, a
// ChatGPT login or API keys.
func LoginAgents() []string {
	return Select(func(d Def) bool { return d.OwnLogin || d.ChatGPT || d.Keys != NoKeys })
}

// KeysOnly reports whether devvm agent login saves only API keys for the
// agent: it has neither a login of its own nor a ChatGPT login (pi).
func (d Def) KeysOnly() bool { return !d.OwnLogin && !d.ChatGPT && d.Keys != NoKeys }

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

// HistoryExcludes are what an export of every agent's history leaves out, in Defs' order.
func HistoryExcludes() []string {
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
