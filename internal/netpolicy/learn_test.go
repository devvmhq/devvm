package netpolicy

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
)

// The network choices are the tiers with learn after internet; learn
// reads as the internet tier, learning, and a value that's no choice is
// refused. Each has what it reaches.
func TestChoices(t *testing.T) {
	if got := strings.Join(Choices(), " "); got != "open internet learn allowlist none" {
		t.Errorf("Choices() = %s", got)
	}
	for _, tc := range []struct {
		s           string
		tier        Tier
		learn, okay bool
	}{
		{"learn", TierInternet, true, true},
		{"internet", TierInternet, false, true},
		{"none", TierNone, false, true},
		{"maybe", "maybe", false, false},
	} {
		if tier, learn, ok := Choice(tc.s); tier != tc.tier || learn != tc.learn || ok != tc.okay {
			t.Errorf("Choice(%s) = %s %v %v", tc.s, tier, learn, ok)
		}
	}
	for _, c := range Choices() {
		if ChoiceDesc(c).ID() == "" {
			t.Errorf("%s has no description", c)
		}
	}
	if got := ChoiceDesc(Learn).ID(); got != "net_choice_learn" {
		t.Errorf("learn's description is %s", got)
	}
}

// Learning shows after internet alone, in devvm ls as in devvm network.
func TestShown(t *testing.T) {
	for _, tc := range []struct {
		listed   string
		learning bool
		want     string
	}{
		{"internet", true, "internet (learning)"},
		{"internet", false, "internet"},
		{CheckUnenforced, true, CheckUnenforced},
		{"allowlist", true, "allowlist"},
	} {
		if got := Shown(tc.listed, tc.learning); got != tc.want {
			t.Errorf("Shown(%s, %v) = %s, want %s", tc.listed, tc.learning, got, tc.want)
		}
	}
}

// A VM learns while its section says since when, on the internet tier:
// any tier set after ends it, and the section without learning is as it
// was, so a VM that never learned keeps its vm.json.
func TestLearningState(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := newVM(t, d, "devvm-l-00000000000000", "base")
	since := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		tier  Tier
		learn Learning
		on    bool
	}{
		{TierInternet, Learning{Since: since}, true},
		{TierAllowlist, Learning{}, false},
		{TierAllowlist, Learning{Since: since}, false}, // only internet learns
		{TierInternet, Learning{}, false},
	} {
		change(t, st, func(st *state.VM) error { return SetTier(st, tc.tier, tc.learn) })
		if lr := LearningOf(st); lr.On() != tc.on || tc.on && !lr.Since.Equal(since) {
			t.Errorf("%s with %+v: learning %+v", tc.tier, tc.learn, lr)
		}
	}
	if b, _ := os.ReadFile(d.VM(st.Name()).State()); strings.Contains(string(b), "learning") {
		t.Errorf("a VM that doesn't learn has learning in its state:\n%s", b)
	}
}

// Addresses, punycode names, paste and drop sites, file storage, tunnels,
// webhook catchers, free hosting and chat endpoints are risky, a
// subdomain of one too, with OAST's, S3's and API Gateway's patterns;
// other hosts aren't. No host is in two of the lists.
func TestRiskOf(t *testing.T) {
	for host, want := range map[string]Risk{
		"203.0.113.5":                             RiskAddress,
		"[2001:db8::1]":                           RiskAddress,
		"xn--pypi-7ra.org":                        RiskLookalike,
		"pastebin.com":                            RiskPaste,
		"PasteBin.com.":                           RiskPaste,
		"gist.githubusercontent.com":              RiskPaste,
		"gist.github.com":                         RiskPaste,
		"justpaste.it":                            RiskPaste,
		"files.catbox.moe":                        RiskStorage,
		"bucket.s3.amazonaws.com":                 RiskStorage,
		"bucket.s3.us-east-1.amazonaws.com":       RiskStorage,
		"s3-us-west-2.amazonaws.com":              RiskStorage,
		"bucket.s3.cn-north-1.amazonaws.com.cn":   RiskStorage,
		"acct.z13.web.core.windows.net":           RiskStorage,
		"x.sharepoint.com":                        RiskStorage,
		"abc.ngrok-free.app":                      RiskTunnel,
		"x.trycloudflare.com":                     RiskTunnel,
		"abc.oast.fun":                            RiskWebhook,
		"x.burpcollaborator.net":                  RiskWebhook,
		"app.vercel.app":                          RiskHosting,
		"d1.cloudfront.net":                       RiskHosting,
		"abc.lambda-url.us-east-1.on.aws":         RiskHosting,
		"abc.execute-api.us-east-1.amazonaws.com": RiskHosting,
		"discord.com":                             RiskChat,
		"hooks.slack.com":                         RiskChat,
		"api.telegram.org":                        RiskChat,
		"ec2.us-east-1.amazonaws.com":             "",
		"registry.npmjs.org":                      "",
		"notpastebin.com":                         "",
		"slack.com":                               "",
		"com":                                     "",
	} {
		if got := RiskOf(host); got != want {
			t.Errorf("RiskOf(%s) = %q, want %q", host, got, want)
		}
	}
	seen := map[string]Risk{}
	for why, hosts := range riskyHosts() {
		for _, h := range hosts {
			if was, ok := seen[h]; ok {
				t.Errorf("%s is %s and %s", h, was, why)
			}
			seen[h] = why
		}
	}
}

// A host is tickable only on port 443, and only under one of the
// reviewed registries' and vendors' names; every host from the review's
// attacks isn't, whatever the VM's rules and secrets. No name on the list
// is, or is under, a domain where anyone gets a subdomain or a page, and
// no name on it is risky.
func TestTickable(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"registry.npmjs.org", 443, true},
		{"Registry.NPMJS.org.", 443, true},
		{"files.pythonhosted.org", 443, true},
		{"static.crates.io", 443, true},
		{"proxy.golang.org", 443, true},
		{"api.anthropic.com", 443, true},
		{"registry.npmjs.org", 80, false},
		{"registry.npmjs.org", 8443, false},
		{"evilregistry.npmjs.org.example", 443, false},
		// The review's: hosts that share a vendor's domain with anyone.
		{"docs.google.com", 443, false},
		{"rekor.sigstore.dev", 443, false},
		{"paste.ubuntu.com", 443, false},
		{"evil.cloudapp.azure.com", 443, false},
		{"ec2-1-2-3-4.compute-1.amazonaws.com", 443, false},
		{"bedrock-runtime.us-east-1.amazonaws.com", 443, false},
		{"aspmx.l.google.com", 25, false},
		{"aspmx.l.google.com", 443, false},
		{"evil.github.io", 443, false},
		{"mydocs.github.io", 443, false},
		{"x.ltd.uk", 443, false},
		{"evil.co.uk", 443, false},
		{"play.golang.org", 443, false},
		{"upload.pypi.org", 443, false},
		{"claude.ai", 443, false},
		{"chatgpt.com", 443, false},
	} {
		if got := Tickable(c.host, c.port); got != c.want {
			t.Errorf("Tickable(%s, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	shared := []string{
		"google.com", "googleapis.com", "amazonaws.com", "azure.com", "windows.net", "cloudapp.net", "github.io", "gitlab.io",
		"githubusercontent.com", "github.com", "ubuntu.com", "debian.org", "alpinelinux.org", "sigstore.dev", "pypi.org", "golang.org",
		"rust-lang.org", "fly.dev", "onrender.com", "hf.space", "ts.net", "appspot.com", "web.app", "firebaseapp.com", "run.app",
		"blogspot.com", "microsoft.com", "openai.com", "anthropic.com", "claude.ai", "docker.com", "vercel.app", "netlify.app",
		"herokuapp.com", "pages.dev", "workers.dev", "cloudfront.net",
	}
	for name, why := range tickable() {
		if why == "" {
			t.Errorf("%s has no reason", name)
		}
		for _, s := range shared {
			if name == s || strings.HasSuffix(s, "."+name) {
				t.Errorf("%s is on the list, which takes in %s, where anyone can get a host or a page", name, s)
			}
		}
		if r := RiskOf(name); r != "" {
			t.Errorf("%s is on the list but risky: %s", name, r)
		}
	}
}

// writeLog writes lines as a log's file at path.
func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// at is an event's line at a time, of a kind, to host:port, by agent.
func at(when time.Time, kind, host string, port int, agent string) string {
	return `{"time":"` + when.UTC().Format(time.RFC3339) + `","event":"` + kind + `","host":"` + host +
		`","port":` + strconv.Itoa(port) + `,"agent":"` + agent + `"}`
}

// none judges no host allowed or denied, and knows no domain.
func none() Judge {
	no := func(string, int) bool { return false }
	return Judge{Allows: no, Denies: no}
}

// Learned is what the VM reached since it started learning that the
// judge neither allows nor denies: one row per exact rule, counted, with
// the tool that reached it most, under a known domain first and the risky
// last. Blocks, links, cache requests, what came before, internal
// addresses, allowed and denied hosts are left out, and a log that lost
// events since is partial. A log that isn't there has none.
func TestLearned(t *testing.T) {
	dir := t.TempDir()
	l := &Log{Path: filepath.Join(dir, "network.jsonl")}
	since := time.Date(2026, 11, 7, 10, 0, 0, 500, time.UTC)
	got, err := l.Learned(since, none())
	if err != nil || len(got.Hosts) != 0 || got.Partial {
		t.Fatalf("no log: %+v, %v", got, err)
	}
	s := func(n int) time.Time { return since.Add(time.Duration(n) * time.Second) }
	writeLog(t, l.Old(),
		at(s(-60), "tunnel", "before.test", 443, ""),
		at(s(-60), learnEvent, "", 0, ""), // an earlier learning's
		at(s(0), learnEvent, "", 0, ""),
		at(s(0), "tunnel", "registry.npmjs.org", 443, "npm/10.9.2 node/v24"),
	)
	writeLog(t, l.Path,
		at(s(1), "intercept", "registry.npmjs.org", 443, "npm/10.9.2 node/v24"),
		at(s(2), "tunnel", "registry.npmjs.org", 443, "curl/8.5.0"),
		at(s(3), "forward", "example.com", 8080, ""),
		at(s(4), "tunnel", "pastebin.com", 443, "curl/8.5.0"),
		at(s(5), "tunnel", "93.184.215.14", 443, ""),
		at(s(6), "tunnel", "10.0.0.5", 22, ""),
		at(s(7), "tunnel", "github.com", 443, ""),
		at(s(8), "blocked", "blocked.test", 443, ""),
		at(s(9), "link", "db", 5432, ""),
		at(s(10), "nix-cache", "cache.nixos.org", 443, ""),
		at(s(11), "tunnel", "bad host", 443, ""),
		at(s(12), "tunnel", "denied.test", 443, ""),
		at(s(13), "forward", "registry.npmjs.org", 80, "npm/10.9.2"),
		at(s(14), "tunnel", "cdn.example.com", 443, string(rune(0x3164))+"npm/1"),
		`not json`,
	)
	j := Judge{
		Allows: func(host string, port int) bool { return host == "github.com" && port == 443 },
		Denies: func(host string, _ int) bool { return host == "denied.test" },
	}
	got, err = l.Learned(since, j)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, h := range got.Hosts {
		rows = append(rows, strings.Join([]string{h.Rule, strconv.Itoa(h.Count), h.Agent, string(h.Risk), strconv.FormatBool(h.Ticked()), h.Last}, " "))
	}
	want := []string{
		"registry.npmjs.org:443 3 npm/10.9.2  true " + s(2).Format(time.RFC3339),
		"cdn.example.com:443 1   false " + s(14).Format(time.RFC3339),
		"example.com:8080 1   false " + s(3).Format(time.RFC3339),
		"registry.npmjs.org:80 1 npm/10.9.2  false " + s(13).Format(time.RFC3339),
		"93.184.215.14:443 1  address false " + s(5).Format(time.RFC3339),
		"pastebin.com:443 1 curl/8.5.0 paste false " + s(4).Format(time.RFC3339),
	}
	if !slices.Equal(rows, want) || got.Partial || !got.Since.Equal(since) {
		t.Errorf("Learned:\n%s\nwant\n%s\npartial %v", strings.Join(rows, "\n"), strings.Join(want, "\n"), got.Partial)
	}
	// The older file is gone with the mark: what came before is too, and
	// an earlier learning's mark doesn't count.
	writeLog(t, l.Old(), at(s(-60), learnEvent, "", 0, ""), at(s(1), "tunnel", "registry.npmjs.org", 443, ""))
	if got, err := l.Learned(since, j); err != nil || !got.Partial {
		t.Errorf("rotated past since: partial %v, %v", got.Partial, err)
	}
}

// devvm's mark of a VM starting to learn is written through the proxy's
// writer, and devvm network log shows nothing of it, nor its --json.
func TestMarkLearning(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "logs", "network.jsonl")}
	since := time.Now().Add(-time.Second)
	if err := l.MarkLearning("devvm-l-00000000000000"); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Learned(since, none()); err != nil || got.Partial {
		t.Errorf("marked: partial %v, %v", got.Partial, err)
	}
	if es, err := l.Events(10); err != nil || len(es) != 0 {
		t.Errorf("Events = %+v, %v", es, err)
	}
	b, _ := os.ReadFile(l.Path)
	if _, ok := FormatEvent(strings.TrimSpace(string(b))); ok || !strings.Contains(string(b), `"event":"learn"`) {
		t.Errorf("the mark %q is shown, or isn't one", b)
	}
}

// The proxy's Learned judges with the VM's allowlist tier's own rules,
// read as its proxy reads them: devvm's for every VM, its shared tools',
// and the user's files, so github.com, registry.npmjs.org for a VM with
// node, a host the VM's own file allows and one it denies are left out.
// Only a reviewed vendor's host starts ticked, such as PyPI's files for a
// VM without python: never a host beside one devvm's rules, yours or the
// VM's own file allow, such as another of GitHub's or of mine.test, which
// a lock before added. A VM that doesn't learn has none.
func TestProxyLearned(t *testing.T) {
	d, st := sandbox(t, "base", "node")
	p := &Proxy{Dirs: d, Assets: assets.FS}
	if got, err := p.Learned(st); err != nil || len(got.Hosts) != 0 {
		t.Fatalf("not learning: %+v, %v", got, err)
	}
	since := time.Now().Add(-time.Minute).UTC()
	change(t, st, func(st *state.VM) error { return SetTier(st, TierInternet, Learning{Since: since}) })
	writeLog(t, d.VM(st.Name()).Policy(), "mine.test:443", "!denied.test")
	writeLog(t, d.Policy(), "mydocs.github.io")
	writeLog(t, d.VM(st.Name()).EventLog(),
		at(since.Add(time.Second), "tunnel", "github.com", 443, ""),
		at(since.Add(time.Second), "tunnel", "registry.npmjs.org", 443, ""),
		at(since.Add(time.Second), "tunnel", "mine.test", 443, ""),
		at(since.Add(time.Second), "tunnel", "denied.test", 443, ""),
		at(since.Add(time.Second), "tunnel", "new.test", 443, ""),
		at(since.Add(time.Second), "tunnel", "uploads.github.com", 443, ""),
		at(since.Add(time.Second), "tunnel", "x.mine.test", 443, ""),
		at(since.Add(time.Second), "tunnel", "evil.github.io", 443, ""),
		at(since.Add(time.Second), "tunnel", "files.pythonhosted.org", 443, ""),
	)
	got, err := p.Learned(st)
	var rules []string
	for _, h := range got.Hosts {
		rules = append(rules, h.Rule+" "+strconv.FormatBool(h.Ticked()))
	}
	want := []string{
		"files.pythonhosted.org:443 true", "evil.github.io:443 false", "new.test:443 false", "uploads.github.com:443 false", "x.mine.test:443 false",
	}
	if err != nil || !slices.Equal(rules, want) {
		t.Errorf("Learned = %q, %v; want %q", rules, err, want)
	}
}

// A User-Agent's first word is the tool it names, when it's short and of
// a tool name's characters alone; anything else names none. The tool
// counted most wins, the first by name of a tie.
func TestAgentWord(t *testing.T) {
	long := strings.Repeat("x", agentMax+1)
	for ua, want := range map[string]string{
		"": "", "  ": "", "npm/10 node/24": "npm/10", "curl/8.5.0": "curl/8.5.0", "Go-http-client/1.1": "Go-http-client/1.1",
		long: "", long[:agentMax]: long[:agentMax], "a\x1b[2Jb": "", "x;y": "",
		// Zero-width, full-width, braille blank and Hangul filler: never shown.
		string(rune(0x200b)) + "npm/1": "", string([]rune{0xff4e, 0xff50, 0xff4d}) + "/1": "", string(rune(0x2800)): "", string(rune(0x3164)): "",
	} {
		if got := agentWord(ua); got != want {
			t.Errorf("agentWord(%q) = %q, want %q", ua, got, want)
		}
	}
	if got := mostOf(map[string]int{"b": 2, "a": 2, "c": 1}); got != "a" {
		t.Errorf("mostOf = %s", got)
	}
	if got := mostOf(nil); got != "" {
		t.Errorf("mostOf(nil) = %s", got)
	}
}
