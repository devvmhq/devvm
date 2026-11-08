package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// The ports the agents' ChatGPT logins in a browser come back to: Codex
// and opencode listen on the first, and Codex on the second when the
// first is taken.
const (
	loginPort         = 1455
	loginFallbackPort = 1457
)

// AgentLoginOptions is how devvm agent login logs an agent in.
type AgentLoginOptions struct {
	Agent string // one of agents.LoginAgents
	// Device logs in to ChatGPT with a code to type into a browser on any
	// machine, so nothing is forwarded.
	Device bool
	// SetupToken gets Claude Code a long-lived token with claude
	// setup-token, which devvm saves as the claude secret, instead of its
	// subscription's login.
	SetupToken bool
	// Key is an API key to save in devvm's store instead, for an agent
	// with a key for each provider Provider's (agentlogin.KeyProviders),
	// which the VM gets as placeholders: the agent's own login never runs.
	// An agent with neither a login of its own nor a ChatGPT login (pi)
	// has no other login here.
	Key, Provider string
}

// AgentLogin is devvm agent login: an agent's login in the folder's VM,
// saved in devvm's agent store, so this machine never needs the agent.
type AgentLogin struct{ *Env }

// Run gets the folder's VM ready as devvm host does (ready), then logs
// o.Agent in there. A ChatGPT login runs the agent's own login in the VM,
// whose proxy saves it in the store as it reaches the token endpoint
// (netpolicy.Capturer): in a browser on this machine, which comes back to
// the agent's server in the VM through a forward of loginPort (and
// loginFallbackPort when it can) that lasts only while the login runs,
// interrupted too; or with a code (Device), nothing forwarded. An API
// key goes into the store from here, and the VM gets its copy of the
// login and its proxy the bindings at once. Claude Code's login, or its
// claude setup-token, shows an address to open in a browser on any
// machine, whose code you paste back, so nothing is forwarded (claude).
// An agent with API keys alone (pi) saves one. Run says what the store
// saved, and fails when the login saved nothing.
func (e AgentLogin) Run(ctx context.Context, o AgentLoginOptions) error {
	if d, _ := agents.Get(o.Agent); d.KeysOnly() && o.Key == "" {
		return msg.AgentLoginKeyOnly(o.Agent)
	}
	t, conf, err := readyConf(ctx, e.launcher)
	if err != nil {
		return err
	}
	vm := t.f.Name
	if !slices.Contains(image.AgentsOf(tools.Names(t.st)), o.Agent) {
		return msg.AgentLoginNoAgent(o.Agent, vm)
	}
	store := e.Proxy.Secrets
	if o.Agent == agents.Claude {
		return e.claude(ctx, t, conf, o)
	}
	if o.Key != "" {
		return e.saveKey(ctx, t, store, o)
	}
	before := store.LoginSum(o.Agent)
	if err := e.login(ctx, t, o); err != nil {
		return err
	}
	return e.saved(store, o.Agent, vm, before)
}

// saved says what the store saved of the agent's login, which was
// before (secrets.Store.LoginSum) as the login started: a new ChatGPT
// login, with its account's email, or nothing, which fails.
func (e AgentLogin) saved(store *secrets.Store, agent, vm, before string) error {
	email, ok, err := store.ChatGPT(agent)
	if err != nil {
		return err
	}
	if !ok || store.LoginSum(agent) == before {
		return msg.AgentLoginNotSaved(agent, vm)
	}
	if email == "" {
		e.Info(msg.AgentLoginSaved(agent, vm))
	} else {
		e.Info(msg.AgentLoginSavedFor(agent, email, vm))
	}
	return nil
}

// claude logs Claude Code in in the VM, its login's code pasted on this
// terminal: the VM's proxy saves its subscription's login in the store,
// or claude setup-token's token as the claude secret, and takes the other
// away. Then the VM is set up again, so it gets the login devvm keeps now,
// its token in the environment or its OAuth login's file, never both, and
// Run says which it saved.
func (e AgentLogin) claude(ctx context.Context, t *target, conf backend.Confinement, o AgentLoginOptions) error {
	vm, store := t.f.Name, e.Proxy.Secrets
	oauth, token, err := secrets.ClaudeOf(store).Sums(ctx)
	if err != nil {
		return err
	}
	e.Info(msg.AgentLoginClaudePaste())
	if err := e.login(ctx, t, o); err != nil {
		return err
	}
	oauth2, token2, err := secrets.ClaudeOf(store).Sums(ctx)
	if err != nil {
		return err
	}
	if o.SetupToken && (token2 == "" || token2 == token) || !o.SetupToken && (oauth2 == "" || oauth2 == oauth) {
		return msg.AgentLoginNotSaved(o.Agent, vm)
	}
	if err := e.launcher.syncer.sync(ctx, t.b, t.st, t.f.Slug(), tools.Files(t.st), conf); err != nil {
		return err
	}
	if o.SetupToken {
		e.Info(msg.ClaudeSetupTokenSaved(vm))
	} else {
		e.Info(msg.ClaudeLoginSaved(vm))
	}
	if secrets.ClaudeOf(store).Env() != "" {
		e.Warn(msg.ClaudeEnvTokenWins())
	}
	return nil
}

// login runs the agent's own login in the VM, on this terminal, with the
// forward a ChatGPT login in a browser needs while it runs.
func (e AgentLogin) login(ctx context.Context, t *target, o AgentLoginOptions) error {
	vm := t.f.Name
	if !o.Device && agents.Of(o.Agent).ChatGPT {
		reach, err := vmssh.Checked(ctx, e.Dirs, e.Registry.Review, t.b, vm, vmssh.PinKeys)
		if err != nil {
			return err
		}
		fw, err := reach.SSH.ForwardLoopback(ctx, loginPort, loginFallbackPort)
		if errors.Is(err, vmssh.ErrPortTaken) {
			return msg.AgentLoginPortTaken(o.Agent, strconv.Itoa(loginPort))
		} else if err != nil {
			return err
		}
		// An interrupt ends the forward too: the run's cleanups run then.
		remove := e.AddCleanup(func(context.Context) { fw.Close() })
		defer func() { fw.Close(); remove() }()
		e.Info(msg.AgentLoginForward(o.Agent, strconv.Itoa(loginPort), vm))
	}
	argv := append([]string{"bash", "-lc", loginScript, "devvm"}, loginArgv(o)...)
	// Interrupted, the run ends the login in the VM too, here or, past
	// the run's grace for this command, in its cleanups.
	remove := e.AddCleanup(func(ctx context.Context) {
		if e.Interrupted() {
			e.endLogin(ctx, t)
		}
	})
	err := t.exec(ctx, argv, backend.ExecOpts{Stdin: os.Stdin, Stdout: e.Stdout(), Stderr: e.Stderr()})
	if ctx.Err() != nil {
		e.endLogin(ctx, t)
		return ctx.Err()
	}
	remove()
	if err != nil {
		return msg.AgentLoginExited(o.Agent, vm)
	}
	return nil
}

// loginScript runs the agent's login in a login shell (commandScript's
// PATH and variables), its pid kept in loginPidFile, below the VM's home,
// for endLogin.
const (
	loginPidFile = ".cache/devvm-login.pid"
	loginScript  = `mkdir -p "$HOME/.cache" && echo $$ > "$HOME/` + loginPidFile + `" && exec "$@"`
	endScript    = `f="$HOME/` + loginPidFile + `"; p=$(cat "$f" 2>/dev/null) && kill "$p" 2>/dev/null; rm -f "$f"`
)

// endLogin ends the agent's login in the VM once devvm's run is
// interrupted: without a terminal, the end of its ssh session doesn't end
// it, and its server would wait on, for the VM's port forwarder to forward
// on. It has a few seconds, past the run's end.
func (e AgentLogin) endLogin(ctx context.Context, t *target) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	log := e.vmLog()
	_ = t.exec(ctx, []string{"sh", "-c", endScript}, backend.ExecOpts{Stdout: log, Stderr: log})
}

// loginArgv is the agent's own login: Claude Code's subscription login, or
// its claude setup-token, without a CLAUDE_CODE_OAUTH_TOKEN of the VM's
// environment, which Claude Code would use instead; Codex's ChatGPT
// login, kept in its auth.json, which the VM's proxy fills with
// placeholders, never a keyring; opencode's Codex login to OpenAI, in a
// browser or with a code (its methods chatgpt-browser and
// chatgpt-headless), whose token endpoint the proxy captures: opencode's
// Sign in with ChatGPT gets its tokens from another endpoint, whose
// replies the proxy refuses.
func loginArgv(o AgentLoginOptions) []string {
	if o.Agent == agents.Claude {
		argv := []string{"env", "-u", "CLAUDE_CODE_OAUTH_TOKEN", agents.Claude}
		if o.SetupToken {
			return append(argv, "setup-token")
		}
		return append(argv, "auth", "login", "--claudeai")
	}
	if o.Agent == agents.Codex {
		argv := []string{agents.Codex, "-c", `cli_auth_credentials_store="file"`, "login"}
		if o.Device {
			argv = append(argv, "--device-auth")
		}
		return argv
	}
	method := "chatgpt-browser"
	if o.Device {
		method = "chatgpt-headless"
	}
	return []string{agents.Opencode, "auth", "login", "openai", "--method", method}
}

// vmLoginScript writes its input to the login file at $1, below the VM's
// home, whole, only the VM's user's to read.
const vmLoginScript = `f="$HOME/$1" && mkdir -p "${f%/*}" && umask 077 && cat > "$f.devvm" && mv -f "$f.devvm" "$f"`

// saveKey saves o's API key in the store, then gives the running VM its
// copy of the login, with placeholders, and its proxy the bindings.
func (e AgentLogin) saveKey(ctx context.Context, t *target, store *secrets.Store, o AgentLoginOptions) error {
	vm := t.f.Name
	if err := store.SaveKey(ctx, o.Agent, o.Provider, o.Key); err != nil {
		return err
	}
	provider := o.Provider
	if provider == "" {
		provider = "openai"
	}
	salt, err := netpolicy.Salt(ctx, t.st)
	if err != nil {
		return err
	}
	if err := store.WriteFiles(ctx, t.st, salt); err != nil {
		return err
	}
	data, err := store.VMLogin(o.Agent, salt)
	if err != nil {
		return msg.LoginCopyFailed(o.Agent, err.Error(), vm).Because(err)
	}
	log := e.vmLog()
	argv := []string{"sh", "-c", vmLoginScript, "sh", secrets.LoginRel(o.Agent)}
	if err := t.exec(ctx, argv, backend.ExecOpts{Stdin: bytes.NewReader(data), Stdout: log, Stderr: log}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.AgentKeyCopyFailed(o.Agent, vm))
	}
	if o.Agent == agents.Opencode {
		if err := e.importLogins(ctx, t, data); err != nil {
			return err
		}
	}
	e.Info(msg.AgentKeySaved(o.Agent, provider, vm))
	return nil
}

// opencodeScript is devvm's guest script that gives opencode 2 in the VM
// the store's opencode logins that changed, run by every launch's setup
// too (guest/sync.sh).
const opencodeScript = "opencode.sh"

// importOutMax is the most of opencodeScript's output devvm agent login
// reads: a few lines a provider.
const importOutMax = 1 << 20

// importLogins gives the VM's opencode, which keeps its logins in its
// database, the logins of vmCopy, the VM's copy of the store's
// (agentlogin.OpencodeImport), with opencodeScript in a login shell
// (opencode on PATH), the logins on its input. devvm reads at most
// importOutMax bytes of what it prints, and stops it past that.
func (e AgentLogin) importLogins(ctx context.Context, t *target, vmCopy []byte) error {
	data, err := agentlogin.OpencodeImport(vmCopy)
	if err != nil {
		return msg.LoginCopyFailed(agents.Opencode, err.Error(), t.f.Name).Because(err)
	}
	script, err := fs.ReadFile(e.Assets, "guest/"+opencodeScript)
	if err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	log := e.vmLog()
	out := &bounded{max: importOutMax, cancel: cancel}
	argv := []string{"bash", "-lc", string(script), opencodeScript}
	err = t.exec(run, argv, backend.ExecOpts{Stdin: bytes.NewReader(data), Stdout: io.MultiWriter(out, log), Stderr: log})
	switch {
	case out.over:
		return msg.OpencodeImportTooMuch(t.f.Name)
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	if failed := opencodeRefused(out.String()); len(failed) > 0 {
		return backend.Logged(msg.OpencodeImportFailed(strings.Join(failed, ", "), t.f.Name))
	}
	return backend.Logged(msg.AgentKeyCopyFailed(agents.Opencode, t.f.Name))
}

// opencodeRefused is the providers opencodeScript's output names as
// refused by opencode's import, its "devvm-opencode-failed PROVIDER" lines.
func opencodeRefused(out string) []string {
	var failed []string
	for line := range strings.Lines(out) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "devvm-opencode-failed "); ok && p != "" {
			failed = append(failed, msg.Printable(p))
		}
	}
	return failed
}
