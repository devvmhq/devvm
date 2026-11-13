package proxy

// Cursor's CLI: the proxy puts its login's token in for the RPCs the CLI calls on its API,
// api2.cursor.sh, its agent's hosts, api5.cursor.sh's, and its indexing's, repo42.cursor.sh
// (agentlogin's cursor format), which serve them, many of them streamed, beside its login's poll
// at api2's /auth/poll, the one request below /auth/ it captures. So on those hosts it refuses
// what would hand the VM a token or make a credential with the login (cursorMints): any other
// request below /auth/, on any of them, an RPC that makes a credential, and a path it can't read as
// Cursor's hosts do (oddPath). And it reads every reply it can without breaking a stream
// (cursorReply), refusing one that holds a token's shape.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// cursorAPI is Cursor's API's host, where its login polls.
const cursorAPI = "api2.cursor.sh"

// cursorHost reports whether host is one the proxy puts Cursor's login in for: its API's, its
// agent's or its indexing's.
func cursorHost(host string) bool {
	return host == cursorAPI || host == "api5.cursor.sh" || strings.HasSuffix(host, ".api5.cursor.sh") || host == "repo42.cursor.sh"
}

// cursorMinting are the RPCs of Cursor's that make, show or exchange a credential with the login,
// by their paths, which every method of Connect's goes to, as the CLI's 2026.11.11-b912975
// package defines them (read, not run): its CLI calls none of them. The login's bindings go to
// none of DashboardService's but GetMe and GetUserPrivacyMode either.
var cursorMinting = []string{
	"/aiserver.v1.AiService/MintRealtimeVoiceSecret",
	"/aiserver.v1.AutomationsService/GetSentryAuthUrl",
	"/aiserver.v1.BackgroundComposerService/GetGithubAccessTokenForRepos",
	"/aiserver.v1.BackgroundComposerService/RedeemSlackSignInLink",
	"/aiserver.v1.BackgroundComposerService/CompleteSlackSignIn",
	"/aiserver.v1.DashboardService/ApproveOAuthAuthorizationRequest",
	"/aiserver.v1.DashboardService/CreateGrokSignInLink",
	"/aiserver.v1.DashboardService/CreateSupportImpersonationSessionInternal",
	"/aiserver.v1.DashboardService/CompleteMcpOAuth",
	"/aiserver.v1.DashboardService/HandOffMcpOAuthCode",
}

// cursorCredential matches a method's name that says it makes, shows or hands over a credential, a
// sign-in or an invitation, which the proxy refuses on every Cursor host, as the package defines
// more of them than cursorMinting names (DashboardService's API keys, service tokens and OAuth
// applications, BackgroundComposerService's secrets and keyring) and Cursor can add more.
var cursorCredential = regexp.MustCompile(`(?i)(key|secret|token|oauth|signin|login|session|auth|cookie|password|jwt|credential|impersonat|scim|invite|link)`)

// cursorStore matches Cursor's agent store's service, aiserver.v1.AgentStoreService, and, as
// defence in depth, any service of any package or version whose name starts with AgentStore, in
// any case: every method of each the proxy refuses, as the store isn't the VM's alone. The account's user store is mounted read-write by its CLIs on every
// machine and by its cloud agents, which load the skills in it as instructions; a team's store by
// every member's; and the store can share a path with the team or the public. Its methods take a
// token of its own (MintAgentStoreToken's), which a VM could send out and use from elsewhere until
// it expires, for whatever Cursor lets that token do, so the VM gets neither the token nor the
// store. The CLI keeps its sessions in a local store.db without it.
var cursorStore = regexp.MustCompile(`(?i)^/[^/]*\.agentstore[^/]*service/`)

// cursorPersists are the methods of Cursor's that change the account in ways that outlast the VM
// and act outside it, as GitHub's keys and webhooks do (githubMinting), which the CLI calls but its
// login never goes to in a VM: plugins and marketplaces installed, updated or removed; cloud
// agents' environments set or deleted, and a cloud agent started; Bedrock keys checked, which the
// store doesn't keep. In the services where the account's own state is, DashboardService,
// AutomationsService and BackgroundComposerService, any method whose name starts with a verb that
// changes something is refused too (cursorChanges), as the package defines many more.
var cursorPersists = []string{
	"/aiserver.v1.DashboardService/InstallUserPlugin",
	"/aiserver.v1.DashboardService/UninstallUserPlugin",
	"/aiserver.v1.DashboardService/UpdateUserPluginInstall",
	"/aiserver.v1.DashboardService/RegisterMarketplaceAndPlugins",
	"/aiserver.v1.DashboardService/RefreshMarketplace",
	"/aiserver.v1.DashboardService/RemoveMarketplace",
	"/aiserver.v1.AutomationsService/ApplyAutomationSpec",
	"/aiserver.v1.AutomationsService/DeleteAutomation",
	"/aiserver.v1.AutomationsService/UpdateAutomationAuthoringMode",
	"/aiserver.v1.AutomationsService/TestAutomation",
	"/aiserver.v1.BackgroundComposerService/StartBackgroundComposerFromSnapshot",
	"/aiserver.v1.BackgroundComposerService/SetPersonalEnvironmentJson",
	"/aiserver.v1.BackgroundComposerService/SetTeamEnvironmentJson",
	"/aiserver.v1.BackgroundComposerService/DeletePersonalEnvironmentJson",
	"/aiserver.v1.BackgroundComposerService/DeleteTeamEnvironment",
	"/aiserver.v1.AiService/TestBedrockCredentials",
}

// cursorAccount are the services where the account's and its teams' own state is.
var cursorAccount = []string{"/aiserver.v1.DashboardService/", "/aiserver.v1.AutomationsService/", "/aiserver.v1.BackgroundComposerService/"}

// cursorChanges matches a method's name that starts with a verb that changes something.
var cursorChanges = regexp.MustCompile(`(?i)^(apply|create|update|upsert|set|delete|remove|enable|disable|install|uninstall|register|add|invite|start|launch|trigger|run|test|pause|resume|rotate|revoke|refresh|toggle|archive|restore|move|rename|edit|modify|change|approve|accept|join|leave|transfer|grant|connect|disconnect|unlink|link|share|publish|import|sync|redeem|complete|schedule|cancel|stop|kill|send|post|put|patch|save|store|upload|attach|detach|assign|unassign|promote|demote|reset|clear|purge|migrate|claim|mark)`)

// cursorAccountChange reports whether p, a method's clean path, changes the account (cursorPersists,
// or a changing verb in cursorAccount's services).
func cursorAccountChange(p string) bool {
	lower := strings.ToLower(p)
	return slices.ContainsFunc(cursorPersists, func(m string) bool { return strings.EqualFold(p, m) }) ||
		slices.ContainsFunc(cursorAccount, func(s string) bool { return strings.HasPrefix(lower, strings.ToLower(s)) }) &&
			cursorChanges.MatchString(p[strings.LastIndex(p, "/")+1:])
}

// cursorMints is what a request to a Cursor host would get or make with the login the proxy puts
// in, or "": a login's tokens, below /auth/ but for the poll it captures (GET api2's
// /auth/poll), a credential, from one of cursorMinting's RPCs or any whose name says so
// (cursorCredential), the agent store (cursorStore), a part of a reply, or anything at a path it
// can't read as the host does.
func cursorMints(host string, r *http.Request) string {
	method, u := r.Method, r.URL
	p := cleanPath(u.Path)
	lower := strings.ToLower(p)
	switch {
	case oddPath(u):
		return "a request at a path devvm can't read as Cursor's hosts do"
	case r.Header.Get("Range") != "":
		return "a part of a reply, which devvm can't read whole"
	case strings.HasPrefix(lower+"/", "/auth/") && (host != cursorAPI || method != http.MethodGet || lower != "/auth/poll"):
		return "a Cursor login's tokens"
	case slices.ContainsFunc(cursorMinting, func(m string) bool { return strings.EqualFold(p, m) }),
		cursorCredential.MatchString(p[strings.LastIndex(p, "/")+1:]):
		return "a Cursor credential"
	case cursorStore.MatchString(p):
		return "Cursor's agent store, which other machines', cloud and team agents share"
	case cursorAccountChange(p):
		return "a change to your Cursor account that outlasts the VM (plugins, automations, cloud agents)"
	}
	return ""
}

// cursorUnencoded drops what of a request to a Cursor host asks for a compressed reply, as the
// proxy reads every reply's body as it passes (cursorReply): HTTP's Accept-Encoding, which Go's
// transport sets again for gzip alone and undoes, and Connect's and gRPC's for their messages, as
// every client of theirs takes identity.
func cursorUnencoded(h http.Header) {
	for _, k := range []string{"Accept-Encoding", "Connect-Accept-Encoding", "Grpc-Accept-Encoding"} {
		h.Del(k)
	}
}

// cursorReply checks a reply from a Cursor host outside the guard of its login's poll, whatever its
// status, length or type, streams included: no part of a body (206, which the proxy's refusal of
// Range already rules out), no encoding the proxy can't read (its HTTP Content-Encoding, which Go's
// transport undoes for gzip, or its Connect or gRPC messages' but gzip), no token in a header, a
// redirect's Location included, and a body that the proxy reads as it passes, cut off at the first
// token in it: a Connect or gRPC stream message by message, each decompressed when it comes
// compressed (frameScanner), and any other body as its bytes come (jwtScanner).
func (s *server) cursorReply(resp *http.Response) error {
	refuse := func(why string) error {
		_ = resp.Body.Close()
		return fmt.Errorf("%w: %s from a host of Cursor's", errCaptureRefused, why)
	}
	h := resp.Header
	h.Del("Set-Cookie") // the CLI keeps no cookies, and a cookie could hold a session
	kind, isJSON := streamKind(h.Get("Content-Type"))
	connectEnc, grpcEnc := h.Get("Connect-Content-Encoding"), h.Get("Grpc-Encoding")
	// A stream's messages may be gzip, in its own protocol's header alone.
	gz := kind == "connect" && strings.EqualFold(connectEnc, "gzip") && identity(grpcEnc) ||
		kind == "grpc" && strings.EqualFold(grpcEnc, "gzip") && identity(connectEnc)
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		return refuse("a part of a reply")
	case encoded(resp) || kind == "unreadable" || !gz && (!identity(connectEnc) || !identity(grpcEnc)):
		return refuse("an encoded reply")
	case tokenIn(h):
		return refuse("a token in a header")
	}
	if kind != "" {
		resp.Body = &frameScanner{r: resp.Body, grpc: kind == "grpc", gzip: gz, json: isJSON, trailer: &resp.Trailer}
		return nil
	}
	resp.Body = &jwtScanner{r: resp.Body, trailer: &resp.Trailer}
	return nil
}

// tokenIn reports whether any of h's values, or names, holds a JWT's start (jwtStart).
func tokenIn(h http.Header) bool {
	for k, vs := range h {
		if jwtStart.MatchString(k) || slices.ContainsFunc(vs, jwtStart.MatchString) {
			return true
		}
	}
	return false
}

// identity reports whether a message encoding is none: unset, or identity.
func identity(enc string) bool { return enc == "" || strings.EqualFold(enc, "identity") }

// jwtStart is a JWT's start, a header and the start of its claims, which the scanner finds before
// any of the claims or the signature that make it a token pass: a base64url part of a JSON object
// ({" is eyJ), a dot, and another one's start.
var jwtStart = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ`)

// jwtWindow is how much of what a body has passed the scanner keeps, so a JWT's start split between
// two reads is found: more than any JWT header's.
const jwtWindow = 4 << 10

// jwtScanner is a reply's body that passes as it comes, without buffering, and fails at the read
// that would complete a JWT's start (jwtStart), passing none of that read: ReverseProxy then ends
// the reply there, so a token's claims and signature never reach the VM.
type jwtScanner struct {
	r       io.ReadCloser
	trailer *http.Header // the reply's trailers, HTTP/2's and gRPC's, filled in once the body ends
	tail    []byte       // the last jwtWindow bytes passed
	err     error
}

// Read fails at the read that completes a JWT's start, and at the body's end when its trailers hold
// one (tokenIn), so ReverseProxy, which copies them after the body, never sends them.
func (j *jwtScanner) Read(p []byte) (int, error) {
	if j.err != nil {
		return 0, j.err
	}
	n, err := j.r.Read(p)
	if n > 0 {
		j.tail = append(j.tail, p[:n]...)
		if jwtStart.Match(j.tail) {
			j.err = fmt.Errorf("%w: a token from a host of Cursor's", errCaptureRefused)
			return 0, j.err
		}
		if over := len(j.tail) - jwtWindow; over > 0 {
			j.tail = append(j.tail[:0], j.tail[over:]...)
		}
	}
	if errors.Is(err, io.EOF) && j.trailer != nil && tokenIn(*j.trailer) {
		j.err = fmt.Errorf("%w: a token in a trailer from a host of Cursor's", errCaptureRefused)
		return n, j.err
	}
	return n, err
}

func (j *jwtScanner) Close() error { return j.r.Close() }
