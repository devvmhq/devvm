package agentlogin

// Cursor's CLI (checked against its Linux package 2026.11.11-b912975, read, not run) keeps its
// login in $XDG_CONFIG_HOME/cursor/auth.json (~/.config/cursor/auth.json) on Linux, only its
// user's to read: {"accessToken", "refreshToken", "apiKey"?, "bedrockCredentials"?}. On macOS it
// keeps it in the Keychain, which devvm agent import doesn't read.
//
// cursor-agent login (NO_OPEN_BROWSER=1 prints the address rather than opening a browser) shows
// cursor.com/loginDeepControl with a PKCE challenge and a uuid, which you open in a browser on any
// machine, and polls GET api2.cursor.sh/auth/poll?uuid=&verifier= until it answers 200 with
// accessToken and refreshToken (404 while the login is pending), and selectedTeamId. The VM's
// proxy captures that poll's success (proxy.Capture's GET): the store keeps the tokens, and the
// VM gets a placeholder JWT for its access token, which the CLI reads only exp from, and managed
// for its refresh token. Its requests carry the token as Authorization: Bearer to api2.cursor.sh,
// its agent hosts (agentUrl, from api2's server config: *.api5.cursor.sh) included.
//
// The CLI never refreshes a login with its refresh token (only an API key exchanges for one, at
// /auth/exchange_user_api_key), so neither does devvm: once the access token expires, its lookup
// fails and says to log in again.

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// Where Cursor's CLI logs in and sends its login.
const (
	// cursorAPI is its API's host, which serves its login's poll beside an API that streams, so
	// the proxy guards only cursorAuth's paths there (proxy.Capture's Guard).
	cursorAPI  = "api2.cursor.sh"
	cursorAuth = "/auth/"
	cursorPoll = cursorAuth + "poll"
	// cursorLogin is the name of its login's binding.
	cursorLogin = agents.Cursor
)

// cursorRoutes are where its CLI sends its login, as the package's own code calls each method,
// and nowhere else, so a request to any other of the hosts' RPCs, such as DashboardService's that
// make API keys, carries only the VM's placeholder: each route's hosts, and by service the methods
// it calls there (Connect's /package.Service/Method), checked against its 2026.11.11-b912975
// package (read, not run). Its API (api2), with its traces; its agent's hosts, which api2's server
// config names (agentUrl and agentnUrl, only checked to parse; Cursor names api5.cursor.sh's hosts,
// which the package doesn't); its indexing's host, repo42.
//
// Left out, though the CLI calls them, and refused by the proxy (proxy's cursorPersists): the
// methods that change the account in ways that outlast the VM and act outside it, as GitHub's keys
// and webhooks do. Installing, updating and removing plugins and marketplaces; applying, deleting,
// testing and changing automations; setting and deleting cloud agents' environments; starting a
// cloud agent; and checking Bedrock keys, which the store doesn't keep. Left out too, and refused
// whole (proxy's cursorStore): AgentStoreService, as the agent store isn't the VM's alone. The
// account's CLIs on every machine and its cloud agents mount its user store, the cloud agents load
// the skills in it, and a team's store is every member's; and its methods take a token of its own
// (MintAgentStoreToken's), which a VM could send out. The CLI keeps its sessions without it.
var cursorRoutes = []struct {
	hosts []string
	rpcs  map[string][]string
	paths []string
}{
	{[]string{cursorAPI}, map[string][]string{
		"aiserver.v1.ServerConfigService": {"GetServerConfig"},
		"aiserver.v1.AiService": {
			"AvailableModels", "GetUsableModels", "GetDefaultModelForCli", "NameAgent", "ReportCommitAiAnalytics",
			"EvaluatePromptHook",
		},
		"aiserver.v1.AutomationsService": {
			"ValidateAutomationSpec", "ListAutomations", "GetAutomation", "ListAutomationRuns", "GetAutomationRun",
		},
		"aiserver.v1.AnalyticsService": {"TrackEvents", "SubmitLogs", "IngestConversation", "BootstrapStatsig"},
		"aiserver.v1.DashboardService": {
			"GetMe", "GetTeams", "GetPlanInfo", "GetUserOrganizations", "GetUserPrivacyMode", "GetCurrentPeriodUsage",
			"GetHardLimit", "GetMonthlyBillingCycle", "GetAggregatedUsageEvents", "GetCliDownloadUrl",
			"GetDeploymentIntegrationAvailability", "GetManagedSkills", "GetGlobalCommands", "GetTeamCommands", "GetTeamHooks",
			"GetTeamAdminSettingsOrEmptyIfNotInTeam", "GetTeamReposOrEmptyIfNotInTeam", "GetEffectiveUserPlugins",
			"ResolvePluginsByRef", "ListMarketplaces", "ListMarketplacePlugins", "SubmitFeedback", "SubmitFeedbackAnon",
		},
		"aiserver.v1.BackgroundComposerService": {
			"ListBackgroundComposers", "GetBackgroundComposerConversation", "GetBackgroundComposerStatus",
			"GetBackgroundComposerUserSettings", "ListPrivateWorkers", "ListEnvironments", "ListPersonalEnvironments",
			"ListTeamEnvironments", "GetEnvironment",
		},
	}, []string{"/v1/traces"}},
	// Its agent's stream, and what goes up with it over HTTP/1.1, which takes it to api2.
	{[]string{cursorAPI, "api5.cursor.sh", "*.api5.cursor.sh"}, map[string][]string{
		"agent.v1.AgentService": {"Run", "RunSSE"}, "aiserver.v1.BidiService": {"BidiAppend"},
	}, nil},
	{[]string{"repo42.cursor.sh"}, map[string][]string{
		"aiserver.v1.RepositoryService": {"FastRepoInitHandshakeV2", "FastUpdateFileV2", "FastRepoSyncComplete", "SyncMerkleSubtreeV2", "SemSearchFast"},
	}, nil},
}

// cursorPaths are a route's paths: each RPC's, in order, and its other paths.
func cursorPaths(rpcs map[string][]string, paths []string) []string {
	var out []string
	for _, svc := range slices.Sorted(maps.Keys(rpcs)) {
		for _, m := range rpcs[svc] {
			out = append(out, "/"+svc+"/"+m)
		}
	}
	return append(out, paths...)
}

// cursorKeep is what the VM's copy of auth.json keeps: its tokens, as placeholders.
var cursorKeep = keep{bound: []string{"accessToken", "refreshToken"}}

// cursorPlaceholder is the VM's in place of Cursor's access token at, a JWT: one the CLI reads as
// lasting until farFuture, so it never thinks the login expired, with at's sub, its account's ID,
// which the CLI names the repos it indexes by.
func cursorPlaceholder(at, salt string) string {
	c := map[string]any{"exp": farFuture, "devvm": salted("placeholder", salt)}
	if sub := str(jwtClaims(at), "sub"); sub != "" {
		c["sub"] = sub
	}
	return fakeJWT(c)
}

type cursorFormat struct{}

func (cursorFormat) kind() string { return agents.Cursor }

// bindings: one for each route's RPCs and paths, all of one name and placeholder.
func (cursorFormat) bindings(m map[string]any, salt string) []proxy.Binding {
	at := str(m, "accessToken")
	if at == "" {
		return nil
	}
	var bs []proxy.Binding
	for _, r := range cursorRoutes {
		for _, p := range cursorPaths(r.rpcs, r.paths) {
			bs = append(bs, login(agents.Cursor, cursorLogin, provider{hosts: r.hosts, path: p, headers: []string{"Authorization"}}, cursorPlaceholder(at, salt))...)
		}
	}
	return bs
}

// vmCopy: its tokens as placeholders, Unknown the fields cursorKeep withholds (an API key,
// Bedrock's credentials).
func (cursorFormat) vmCopy(m map[string]any, f File) (map[string]any, VMFile) {
	c, unknown := cursorKeep.apply(m)
	if at := str(m, "accessToken"); at != "" {
		c["accessToken"], c["refreshToken"] = cursorPlaceholder(at, f.Salt), managed
	}
	return c, VMFile{Unknown: unknown}
}

// errCursorExpired: Cursor's login expired, and nothing refreshes it.
var errCursorExpired = errors.New("the login expired, and Cursor's CLI has no refresh of a login")

// get: the access token, read again, while it lasts.
func (cursorFormat) get(_ context.Context, f File, name string) (string, error) {
	if name != cursorLogin {
		return "", errors.New("no Cursor login " + name)
	}
	m, err := readJSON(f.Path)
	if err != nil {
		return "", err
	}
	at := str(m, "accessToken")
	if at == "" {
		return "", errors.New("the Cursor login in devvm's agent store has no access token")
	}
	if exp, ok := jwtExp(at); ok && time.Until(exp) <= 0 {
		return "", &RefreshError{Agent: agents.Cursor, Err: errCursorExpired}
	}
	return at, nil
}

func (cursorFormat) endpoints() []endpoint {
	return []endpoint{{
		host: cursorAPI, path: cursorPoll, method: "GET", guard: cursorAuth,
		tokenKeys: []string{"accessToken", "refreshToken", "xaiAccessToken", "xaiRefreshToken", "sessionToken", "authToken", "apiKey", "jwt", "token"},
	}}
}

func (cursorFormat) userAgents() []string { return nil }

// poll: the poll's query, its login's uuid and its PKCE verifier.
func (cursorFormat) poll(t tokenRequest) bool {
	return t.form && t.fields.Get("uuid") != "" && t.fields.Get("verifier") != ""
}

// capture: the poll's success replaces the store's login with its tokens. A reply without both
// (an xAI sign-in's, which names its tokens otherwise) is refused, and nothing is saved.
func (cursorFormat) capture(_ map[string]any, _ tokenRequest, r map[string]any, salt string) (map[string]any, map[string]any, error) {
	at, rt := str(r, "accessToken"), str(r, "refreshToken")
	if at == "" || rt == "" {
		return nil, nil, errNoLogin
	}
	vm := map[string]any{"accessToken": cursorPlaceholder(at, salt), "refreshToken": managed}
	if id, ok := r["selectedTeamId"]; ok && plainField(id) {
		vm["selectedTeamId"] = id
	}
	return map[string]any{"accessToken": at, "refreshToken": rt}, vm, nil
}

// managedReply: none, as Cursor's CLI never refreshes a login.
func (cursorFormat) managedReply(map[string]any, string) ([]byte, error) {
	return nil, errors.New("a login that Cursor's CLI never refreshes")
}
