package agentlogin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// Cursor's login: only its poll (a GET's query, as a form, with its uuid and verifier) is a request
// its capture reads, which goes on as it is. Its success replaces the store's login with its two
// tokens, and the VM gets placeholders: a JWT that lasts until farFuture for its access token,
// managed for its refresh token, and the team it chose. A reply without both tokens saves nothing.
func TestCursorCapture(t *testing.T) {
	f := File{Kind: agents.Cursor, Path: filepath.Join(t.TempDir(), "auth.json"), Salt: salt}
	poll := "uuid=u-1&verifier=v-1"
	out, answer, err := f.CaptureRequest(formType, []byte(poll))
	if err != nil || string(out) != poll || answer != nil {
		t.Fatalf("CaptureRequest(poll) = %q, %q, %v", out, answer, err)
	}
	for _, req := range []string{"uuid=u-1", "verifier=v-1", "grant_type=authorization_code&code=c", "grant_type=refresh_token&refresh_token=" + managed} {
		if _, _, err := f.CaptureRequest(formType, []byte(req)); !errors.Is(err, errGrant) {
			t.Errorf("CaptureRequest(%s) = %v", req, err)
		}
	}
	if _, err := f.Capture(formType, []byte(poll), []byte(`{"accessToken":"at-only"}`)); err == nil {
		t.Error("a reply without a refresh token was saved")
	}
	if _, err := f.Capture(formType, []byte(poll), []byte(`{"xaiAccessToken":"x","xaiRefreshToken":"y"}`)); err == nil {
		t.Error("an xAI sign-in's reply was saved")
	}
	if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused reply wrote the store's login: %v", err)
	}
	at := jwtWith(time.Now().Add(time.Hour), map[string]any{"sub": "auth0|1"})
	vm, err := f.Capture(formType, []byte(poll), []byte(`{"accessToken":"`+at+`","refreshToken":"rt-real","selectedTeamId":7}`))
	must(t, err)
	var got map[string]any
	must(t, json.Unmarshal(vm, &got))
	ph := cursorPlaceholder(at, salt)
	if got["accessToken"] != ph || got["refreshToken"] != managed || got["selectedTeamId"] != float64(7) ||
		strings.Contains(string(vm), "rt-real") || strings.Contains(string(vm), at) {
		t.Errorf("the VM got %s", vm)
	}
	// The placeholder lasts until farFuture, and names the account as the token does (sub), as the
	// CLI names the repos it indexes by it.
	if exp, ok := jwtExp(ph); !ok || exp.Unix() != farFuture || str(jwtClaims(ph), "sub") != "auth0|1" || !strings.Contains(str(jwtClaims(ph), "devvm"), salt) {
		t.Errorf("the placeholder's claims = %v", jwtClaims(ph))
	}
	saved, err := readJSON(f.Path)
	must(t, err)
	if len(saved) != 2 || saved["accessToken"] != at || saved["refreshToken"] != "rt-real" {
		t.Errorf("the store's login = %v", saved)
	}
	// A binding for each method the CLI calls, exactly, on its hosts alone, and each one a method the
	// proxy lets through there (proxy.Mints): never a whole service, nor /auth/.
	bs, err := f.Bindings()
	must(t, err)
	var paths []string
	for _, b := range bs {
		if b.Placeholder != ph || b.Header != "Authorization" || b.Name != cursorLogin || len(b.Hosts) == 0 {
			t.Errorf("binding %+v", b)
		}
		paths = append(paths, strings.Join(b.Hosts, ",")+b.Path)
		if strings.HasSuffix(b.Path, "/") || strings.Contains(b.Path, "/auth") || strings.Count(b.Path, "/") != 2 {
			t.Errorf("a binding for %s", b.Path)
		}
		for _, h := range b.Hosts {
			host := strings.Replace(h, "*", "agent", 1)
			r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: b.Path}, Header: http.Header{}}
			if why := proxy.Mints(host, r); why != "" {
				t.Errorf("the CLI's %s on %s is refused: %s", b.Path, host, why)
			}
		}
	}
	for _, want := range []string{
		"api2.cursor.sh/aiserver.v1.AiService/GetUsableModels", "api2.cursor.sh/aiserver.v1.DashboardService/GetMe",
		"api2.cursor.sh/v1/traces",
		"api2.cursor.sh,api5.cursor.sh,*.api5.cursor.sh/agent.v1.AgentService/Run",
		"repo42.cursor.sh/aiserver.v1.RepositoryService/SemSearchFast",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("no binding for %s in %q", want, paths)
		}
	}
	// Never a method that makes a credential, nor one that changes the account in a way that
	// outlasts the VM, nor the agent store's, which the CLI calls on the host but never gets the
	// login for in a VM; and the proxy refuses each.
	for _, never := range []string{
		"/aiserver.v1.DashboardService/CreateUserApiKey", "/aiserver.v1.AiService/MintRealtimeVoiceSecret",
		"/aiserver.v1.DashboardService/InstallUserPlugin", "/aiserver.v1.DashboardService/UninstallUserPlugin",
		"/aiserver.v1.DashboardService/UpdateUserPluginInstall", "/aiserver.v1.DashboardService/RegisterMarketplaceAndPlugins",
		"/aiserver.v1.DashboardService/RefreshMarketplace", "/aiserver.v1.DashboardService/RemoveMarketplace",
		"/aiserver.v1.AutomationsService/ApplyAutomationSpec", "/aiserver.v1.AutomationsService/DeleteAutomation",
		"/aiserver.v1.AutomationsService/UpdateAutomationAuthoringMode", "/aiserver.v1.AutomationsService/TestAutomation",
		"/aiserver.v1.BackgroundComposerService/StartBackgroundComposerFromSnapshot",
		"/aiserver.v1.BackgroundComposerService/SetPersonalEnvironmentJson", "/aiserver.v1.BackgroundComposerService/SetTeamEnvironmentJson",
		"/aiserver.v1.BackgroundComposerService/DeletePersonalEnvironmentJson", "/aiserver.v1.BackgroundComposerService/DeleteTeamEnvironment",
		"/aiserver.v1.AiService/TestBedrockCredentials",
		"/aiserver.v1.AgentStoreService/MintAgentStoreToken", "/aiserver.v1.AgentStoreService/PresignAgentStoreReads",
		"/aiserver.v1.AgentStoreService/ListAgentStoreDirectory", "/aiserver.v1.AgentStoreService/CheckAgentStoreVersions",
		"/aiserver.v1.AgentStoreService/RecordAgentStoreListingVersionAudit",
	} {
		if slices.ContainsFunc(paths, func(p string) bool { return strings.HasSuffix(p, never) }) {
			t.Errorf("a binding for %s", never)
		}
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: never}, Header: http.Header{}}
		if proxy.Mints(cursorAPI, r) == "" {
			t.Errorf("the proxy lets %s through", never)
		}
	}
	if v, err := f.Get(t.Context(), cursorLogin); err != nil || v != at {
		t.Errorf("Get = %q, %v", v, err)
	}
	if _, err := (cursorFormat{}).managedReply(nil, salt); err == nil {
		t.Error("a managed reply for a login Cursor's CLI never refreshes")
	}
}

// An expired Cursor login isn't put in: Cursor's CLI has no refresh of it, so the lookup says to log
// in again (RefreshError); a missing access token or another binding's name fails too.
func TestCursorGet(t *testing.T) {
	p := writeFile(t, map[string]any{"accessToken": jwtWith(time.Now().Add(-time.Minute), nil), "refreshToken": "rt"})
	f := File{Kind: agents.Cursor, Path: p}
	var re *RefreshError
	if _, err := f.Get(t.Context(), cursorLogin); !errors.As(err, &re) || re.Agent != agents.Cursor {
		t.Errorf("Get(expired) = %v", err)
	}
	if _, err := f.Get(t.Context(), "other"); err == nil {
		t.Error("Get(other) found a login")
	}
	empty := File{Kind: agents.Cursor, Path: writeFile(t, map[string]any{"apiKey": "k"})}
	if _, err := empty.Get(t.Context(), cursorLogin); err == nil {
		t.Error("Get without an access token found one")
	}
	if bs, _ := empty.Bindings(); len(bs) != 0 {
		t.Errorf("bindings without an access token = %+v", bs)
	}
}
