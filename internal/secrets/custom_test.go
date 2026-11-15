package secrets

import (
	"os"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
)

// A secret of yours has a name of its own: lower case, digits and dashes,
// never devvm's or a coding agent's, alone or before a dash, as the
// agents' logins' bindings are named.
func TestCheckName(t *testing.T) {
	for name, want := range map[string]string{
		"stripe": "", "openrouter-key": "", "a": "", "s3": "", strings.Repeat("a", nameMax): "",
		"":                             "bad_secret_name given= most=40",
		"Stripe":                       "bad_secret_name given=Stripe most=40",
		"-x":                           "bad_secret_name given=-x most=40",
		"x-":                           "bad_secret_name given=x- most=40",
		"a_b":                          "bad_secret_name given=a_b most=40",
		"../../etc/passwd":             "bad_secret_name given=../../etc/passwd most=40",
		"a.b":                          "bad_secret_name given=a.b most=40",
		strings.Repeat("a", nameMax+1): "bad_secret_name given=" + strings.Repeat("a", nameMax+1) + " most=40",
		"github":                       "secret_name_taken given=github",
		"claude":                       "secret_name_taken given=claude",
		"proxy":                        "secret_name_taken given=proxy",
		"codex":                        "secret_name_taken given=codex",
		"codex-key":                    "secret_name_taken given=codex-key",
		"pi-openai":                    "secret_name_taken given=pi-openai",
		"opencode-anthropic":           "secret_name_taken given=opencode-anthropic",
		"claude-mcp-x":                 "secret_name_taken given=claude-mcp-x",
		"pip":                          "",
	} {
		if got := msg.NamesOf(CheckName(name)); got != want {
			t.Errorf("CheckName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A secret of yours goes to exact host names on 443 alone: no wildcard,
// address, internal name, single label or other port.
func TestCheckHost(t *testing.T) {
	for given, want := range map[string]string{
		"api.stripe.com": "api.stripe.com", "API.Stripe.COM": "api.stripe.com", "api.stripe.com.": "api.stripe.com",
		"api.stripe.com:443": "api.stripe.com", "openrouter.ai": "openrouter.ai", "a-b.example.co.uk": "a-b.example.co.uk",
		"*.stripe.com": "", "*": "", "**": "", "api.*.com": "", "com": "", "localhost": "", "api.localhost": "",
		"printer.local": "", "db.internal": "", "devvm.proxy": "", "db.devvm.internal": "", "x.home.arpa": "",
		"10.0.0.1": "", "127.0.0.1": "", "1.1.1.1": "", "[::1]": "", "::1": "", "[::1]:443": "", "api.stripe.com:80": "",
		"api.stripe.com:8443": "", "api.stripe.com:": "", "": "", "api..com": "", "api.stripe.123": "", "a b.com": "",
		"api.stripe.com/x": "", "!api.stripe.com": "", "api.stripe.com#x": "", "ex%41mple.com": "",
		"api.test": "", "www.example": "", "www.example.com": "www.example.com",
		"x.alt": "", "web.service.consul": "", "db.default.svc": "", "app.docker": "", "box.tail1234.ts.net": "",
		"10.0.0.1.nip.io": "", "nip.io": "", "1-2-3-4.sslip.io": "", "app.localtest.me": "", "lvh.me": "",
		"net.ts.net.example.com": "net.ts.net.example.com",
	} {
		got, err := CheckHost(given)
		switch {
		case want == "" && msg.NamesOf(err) != "bad_secret_host given="+given:
			t.Errorf("CheckHost(%q) = %q, %v: want refused", given, got, msg.NamesOf(err))
		case want != "" && (got != want || err != nil):
			t.Errorf("CheckHost(%q) = %q, %v, want %q", given, got, msg.NamesOf(err), want)
		}
	}
}

// A secret of yours goes in a header that carries a credential: a token,
// in its canonical form, never Host, Cookie, the framing, the proxy's or a
// hop-by-hop one.
func TestCheckHeader(t *testing.T) {
	for given, want := range map[string]string{
		"Authorization": "Authorization", "authorization": "Authorization", "x-api-key": "X-Api-Key",
		"X-Goog-Api-Key": "X-Goog-Api-Key", "api_key": "Api_key",
		"Host": "", "host": "", "Cookie": "", "Content-Length": "", "Transfer-Encoding": "", "Connection": "",
		"Keep-Alive": "", "TE": "", "Trailer": "", "Upgrade": "", "Proxy-Authorization": "", "proxy-connection": "",
		"Forwarded": "", "X-Forwarded-For": "", "x-forwarded-host": "", "X-Forwarded-Proto": "",
		"Origin": "", "Referer": "", "user-agent": "", "X-Request-Id": "", "X-Correlation-ID": "", "traceparent": "",
		"Via": "", "Expect": "", "HTTP2-Settings": "", "Content-Type": "",
		"": "", "X Api": "", "X-Api:": "", "X-Api\r\nHost": "", "Ünicode": "", strings.Repeat("X", headerMax+1): "",
	} {
		got, err := CheckHeader(given)
		switch {
		case want == "" && !strings.HasPrefix(msg.NamesOf(err), "bad_secret_header given="):
			t.Errorf("CheckHeader(%q) = %q, %v: want refused", given, got, msg.NamesOf(err))
		case want != "" && (got != want || err != nil):
			t.Errorf("CheckHeader(%q) = %q, %v, want %q", given, got, msg.NamesOf(err), want)
		}
	}
}

// A secret of yours' variable is a shell variable that neither devvm nor
// the VM's shell sets.
func TestCheckEnv(t *testing.T) {
	for env, ok := range map[string]bool{
		"STRIPE_API_KEY": true, "OPENROUTER_API_KEY": true, "openai_key": true, "_X": true,
		"": false, "1X": false, "A-B": false, "A B": false, "A=B": false, "$X": false,
		"PATH": false, "HOME": false, "path": false, "LANG": false, "IFS": false, "BASH_ENV": false, "ENV": false,
		"LD_PRELOAD": false, "DYLD_INSERT_LIBRARIES": false, "LC_ALL": false, "SSH_AUTH_SOCK": false,
		"DEVVM_PROXY": false, "HTTPS_PROXY": false, "https_proxy": false, "ALL_PROXY": false, "NODE_USE_ENV_PROXY": false,
		"GH_TOKEN": false, "CLAUDE_CODE_OAUTH_TOKEN": false, "SSL_CERT_FILE": false, "NODE_EXTRA_CA_CERTS": false,
		"DISABLE_AUTOUPDATER": false, "NODE_OPTIONS": false, "NPM_CONFIG_PREFIX": false, "npm_config_cache": false, "BROWSER": false,
		"GIT_SSH_COMMAND": false, "GIT_SSL_CAINFO": false, "PYTHONPATH": false, "PYTHONSTARTUP": false, "PERL5LIB": false,
		"RUBYOPT": false, "NODE_PATH": false, "GCONV_PATH": false, "EDITOR": false, "VISUAL": false, "PAGER": false,
		"LESSOPEN": false, "CDPATH": false, "HISTFILE": false, "PS0": false, "SUDO_ASKPASS": false, "HOSTALIASES": false,
		"RES_OPTIONS": false, "LOCALDOMAIN": false, "SSL_CERT_DIR": false, "CURL_CA_BUNDLE": false, "MISE_LOCKED": false,
		"DISPLAY": false, "DO_NOT_TRACK": false, "NEXT_TELEMETRY_DISABLED": false, "DOTNET_CLI_TELEMETRY_OPTOUT": false,
		"GITHUB_TOKEN": true, "GITLAB_TOKEN": true,
	} {
		err := CheckEnv(env)
		if (err == nil) != ok || (!ok && msg.NamesOf(err) != "bad_secret_env given="+env) {
			t.Errorf("CheckEnv(%q) = %v, want ok %v", env, msg.NamesOf(err), ok)
		}
	}
}

// A placeholder's start is a short word of letters, digits, '_', '-' and
// '.'.
func TestCheckPrefix(t *testing.T) {
	for prefix, ok := range map[string]bool{
		"sk_": true, "sk-": true, "sk_live_": true, "ghp_": true, "xoxb-": true, "v1.": true, strings.Repeat("a", prefixMax): true,
		"": false, "s k": false, "sk:": false, "devvm-proxy-managed-": false, "x.devvm-placeholder": false, `sk"`: false, "sk$": false, "sk\n": false, strings.Repeat("a", prefixMax+1): false,
	} {
		if err := CheckPrefix(prefix); (err == nil) != ok {
			t.Errorf("CheckPrefix(%q) = %v, want ok %v", prefix, msg.NamesOf(err), ok)
		}
	}
}

// Define: each host once, Authorization by default, and a secret needs its
// hosts and its variable.
func TestDefine(t *testing.T) {
	sec, err := Define("stripe", Def{Hosts: []string{"api.stripe.com", "API.stripe.com", "files.stripe.com:443"}, Env: "STRIPE_API_KEY", Prefix: "sk_"})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if strings.Join(sec.Hosts, ",") != "api.stripe.com,files.stripe.com" || sec.Header != DefaultHeader || !sec.Custom || sec.HostOnly {
		t.Errorf("Define = %+v", sec)
	}
	many := make([]string, hostsMax+1)
	for i := range many {
		many[i] = "h" + strings.Repeat("x", i) + ".example.com"
	}
	host := []string{"api.stripe.com"}
	for _, tc := range []struct {
		d   Def
		err string
	}{
		{Def{Env: "X_KEY"}, "secret_needs_host secret=stripe"},
		{Def{Hosts: host}, "secret_needs_env secret=stripe"},
		{Def{Hosts: []string{"*.stripe.com"}, Env: "X_KEY"}, "bad_secret_host given=*.stripe.com"},
		{Def{Hosts: host, Header: "Cookie", Env: "X_KEY"}, "bad_secret_header given=Cookie"},
		{Def{Hosts: host, Env: "PATH"}, "bad_secret_env given=PATH"},
		{Def{Hosts: many, Env: "X_KEY"}, "secret_too_many_hosts most=16 secret=stripe"},
		{Def{Hosts: host, Env: "X_KEY", Prefix: "sk "}, "bad_secret_prefix given=sk  most=32"},
		{Def{Hosts: host, Env: "X_KEY", Scheme: "Basic"}, "bad_secret_scheme given=Basic"},
		{Def{Hosts: host, Env: "X_KEY", Path: "api/"}, "bad_request_path given=api/"},
		{Def{Hosts: host, Env: "X_KEY", Path: "/a/../b/"}, "bad_request_path given=/a/../b/"},
		{Def{Hosts: host, Env: "X_KEY", Path: "/v1/*/charges", Methods: "get,HEAD"}, ""},
		{Def{Hosts: host, Env: "X_KEY", Path: "/"}, ""},
		{Def{Hosts: host, Env: "X_KEY", Methods: "GET,CONNECT"}, "bad_methods given=GET,CONNECT methods=" + strings.Join(proxy.Methods(), ", ")},
		{Def{Hosts: host, Env: "X_KEY", Header: "X-Api-Key", Scheme: "ApiKey"}, "secret_scheme_header secret=stripe"},
		{Def{Hosts: host, Env: "X_KEY", Header: "X-Api-Key", BasicUser: true}, "secret_scheme_header secret=stripe"},
		{Def{Hosts: host, Env: "X_KEY", Scheme: "ApiKey", Path: "/v1/", BasicUser: true}, ""},
	} {
		if _, err := Define("stripe", tc.d); msg.NamesOf(err) != tc.err {
			t.Errorf("Define(%+v) = %v, want %s", tc.d, msg.NamesOf(err), tc.err)
		}
	}
	// A path of / is every path, so it's none: a bindings file of version 1 that a proxy from
	// before patterns reads, whose prefixes are longer than /.
	if every, err := Define("stripe", Def{Hosts: host, Env: "X_KEY", Path: "/"}); err != nil || every.Path != "" {
		t.Errorf("a path of /: %q, %v", every.Path, msg.NamesOf(err))
	}
}

// devvm secret set's flags over a saved secret: only what's given
// changes, none takes a scheme, a path or methods away, a secret saved
// without --method goes in for every method, and the ID stays.
func TestChangeApply(t *testing.T) {
	saved, err := Define("stripe", Def{Hosts: []string{"api.stripe.com"}, Env: "STRIPE_API_KEY", Prefix: "sk_", Scheme: "ApiKey", Path: "/v1/", Methods: "get", BasicUser: true})
	if err != nil {
		t.Fatal(err)
	}
	saved.ID = newID()
	for _, tc := range []struct {
		c    Change
		want func(Secret) bool
	}{
		{Change{}, func(s Secret) bool {
			return s.Scheme == "ApiKey" && s.Path == "/v1/" && s.Methods == "GET" && s.BasicUser && s.Prefix == "sk_"
		}},
		{Change{Def: Def{Hosts: []string{"files.stripe.com"}}}, func(s Secret) bool { return s.Hosts[0] == "files.stripe.com" && s.Env == "STRIPE_API_KEY" }},
		{Change{Def: Def{Scheme: None, Path: None}}, func(s Secret) bool { return s.Scheme == "" && s.Path == "" && s.Methods == "GET" && s.BasicUser }},
		{Change{Def: Def{Methods: "HEAD,GET"}}, func(s Secret) bool { return s.Methods == "GET,HEAD" }},
		{Change{Def: Def{Methods: None}}, func(s Secret) bool { return s.Methods == "" && s.Path == "/v1/" }},
		{Change{BasicUserSet: true}, func(s Secret) bool { return !s.BasicUser }},
	} {
		got, err := tc.c.Apply("stripe", &saved)
		if err != nil || !tc.want(got) || got.ID != saved.ID {
			t.Errorf("Apply(%+v) = %+v, %v", tc.c, got, msg.NamesOf(err))
		}
	}
	if (Change{BasicUserSet: true}).Empty() || !(Change{}).Empty() {
		t.Error("Empty")
	}
}

// Every secret's placeholder has the one shape (agentlogin.Placeholder):
// the known ones' as they've always been, yours with its prefix first and
// its name, each the VM's own by its salt, and never a value.
func TestPlaceholders(t *testing.T) {
	salt := strings.Repeat("ab", 16)
	claude, _ := Lookup(ClaudeToken)
	github, _ := Lookup("github")
	stripe, err := Define("stripe", Def{Hosts: []string{"api.stripe.com"}, Env: "STRIPE_API_KEY", Prefix: "sk_"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Define("openrouter", Def{Hosts: []string{"openrouter.ai"}, Env: "OPENROUTER_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	for sec, want := range map[*Secret]string{
		&claude: "sk-ant-oat01-devvm-proxy-managed-" + salt,
		&github: "devvm-proxy-managed-github-" + salt,
		&stripe: "sk_devvm-proxy-managed-secret-stripe-" + salt,
		&plain:  "devvm-proxy-managed-secret-openrouter-" + salt,
	} {
		ph := sec.Placeholder(salt)
		if ph != want || !agentlogin.HasPlaceholder([]byte(ph)) {
			t.Errorf("%s's placeholder = %q, want %q", sec.Name, ph, want)
		}
		if _, err := Token(sec.Name, redact.NewSecret(ph)); msg.NamesOf(err) != "token_is_placeholder" {
			t.Errorf("%s's placeholder taken as its value: %v", sec.Name, msg.NamesOf(err))
		}
	}
	if stripe.Placeholder(salt) == stripe.Placeholder(strings.Repeat("cd", 16)) {
		t.Error("two VMs' placeholders are the same")
	}
}

// A list devvm can't read, others may write or that's a symbolic link, or
// that holds a secret Define refuses or one without an ID, fails what
// reads it, naming the file: never a secret that skipped the checks.
func TestCustomsUnreadable(t *testing.T) {
	s, _ := testStore(t, false, nil)
	y := YoursOf(s)
	path := s.Dirs.CustomSecrets()
	id := `"id":"` + strings.Repeat("ab", 16) + `",`
	good := `{"version":1,"secrets":[{"name":"x",` + id + `"hosts":["api.example.com"],"header":"Authorization","env":"X_KEY"}]}`
	for _, tc := range []struct {
		data string
		mode os.FileMode
		want string
	}{
		{"not json", 0o600, "custom_secrets_unreadable path=" + path},
		{`{"version":2,"secrets":[]}`, 0o600, "custom_secrets_unreadable path=" + path},
		{good, 0o620, "custom_secrets_unreadable path=" + path},
		{`{"version":1,"secrets":[{"name":"x",` + id + `"hosts":["*.example.com"],"header":"Authorization","env":"X_KEY"}]}`, 0o600, "custom_secret_refused path=" + path + " secret=x"},
		{`{"version":1,"secrets":[{"name":"github",` + id + `"hosts":["evil.example.com"],"header":"Authorization","env":"X_KEY"}]}`, 0o600, "custom_secret_refused path=" + path + " secret=github"},
		{`{"version":1,"secrets":[{"name":"x","hosts":["api.example.com"],"header":"Authorization","env":"X_KEY"}]}`, 0o600, "custom_secret_refused path=" + path + " secret=x"},
		{`{"version":1,"secrets":[{"name":"x",` + id + `"hosts":["api.example.com"],"header":"Authorization","env":"X_KEY","scheme":"Basic"}]}`, 0o600, "custom_secret_refused path=" + path + " secret=x"},
		// A path saved as a prefix whose * was itself isn't read as a wider pattern.
		{`{"version":1,"secrets":[{"name":"x",` + id + `"hosts":["api.example.com"],"header":"Authorization","env":"X_KEY","path":"/v*/"}]}`, 0o600, "custom_secret_refused path=" + path + " secret=x"},
	} {
		if err := os.MkdirAll(s.Dirs.Data, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(tc.data), tc.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := y.All(); !strings.HasPrefix(msg.NamesOf(err), tc.want) {
			t.Errorf("%s, mode %o: %v, want %s", tc.data, tc.mode, msg.NamesOf(err), tc.want)
		}
	}
	// A symbolic link to a good list is refused too.
	target := path + ".target"
	if err := os.WriteFile(target, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := y.All(); !strings.HasPrefix(msg.NamesOf(err), "custom_secrets_unreadable path="+path) {
		t.Errorf("a symbolic link: %v", msg.NamesOf(err))
	}
	// A known secret's name never reads the list, so a VM that uses none of
	// yours launches as it did.
	if _, ok, err := y.Lookup("github"); !ok || err != nil {
		t.Errorf("Lookup(github) = %v, %v", ok, err)
	}
	if all, err := y.Using([]string{"github", "claude"}); err != nil || len(all) != len(Known) {
		t.Errorf("Using(known) = %d, %v", len(all), err)
	}
	if _, err := y.Using([]string{"github", "stripe"}); !strings.HasPrefix(msg.NamesOf(err), "custom_secrets_unreadable ") { // the link's
		t.Errorf("Using(stripe) = %v", msg.NamesOf(err))
	}
}
