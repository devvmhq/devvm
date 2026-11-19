package vm

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// sorted is ss, sorted.
func sorted(ss ...string) []string {
	slices.Sort(ss)
	return ss
}

// A marketplace's clone whose git config holds a credential, in a URL's user info or an http
// extraHeader, is copied as it is, with a warning naming it, the store's file and the keys,
// never the credential, a URL subsection that holds one shown as <url>; so is one whose
// known_marketplaces.json source holds one. A config git can't read is named as unchecked, as
// the VM reads the file itself. A clone with an ssh user, or none, gets none. The import and the
// save both warn.
func TestMarketplaceSecrets(t *testing.T) {
	configs := map[string]string{ //nolint:gosec // G101: test fixtures, not credentials
		"tok":     "[remote \"origin\"]\n\turl = https://x-access-token:ghp_abc@github.com/o/r.git\n",
		"user":    "[remote \"origin\"]\n\turl = https://ghp_abc@github.com/o/r.git\n",
		"header":  "[http \"https://github.com/\"]\n\textraHeader = Authorization: Basic eDp5\n",
		"hdrurl":  "[http \"https://x:ghp_abc@github.com/\"]\n\textraHeader = Authorization: Basic eDp5\n",
		"push":    "[remote \"origin\"]\n\turl = git@github.com:o/r.git\n\tpushurl = https://u:p@example.com/r.git\n",
		"instead": "[url \"https://x:ghp_abc@github.com/\"]\n\tinsteadOf = https://github.com/\n",
		"helper":  "[remote \"origin\"]\n\turl = persistent-https::https://tok@example.com/r\n",
		"proxy":   "[http]\n\tproxy = http://u:pw@proxy:8080\n",
		"include": "[include]\n\tpath = creds\n",
		"missing": "[include]\n\tpath = gone\n\tpath = ~/.gitconfig-x\n[remote \"origin\"]\n\turl = https://tok@example.com/r\n",
		"broken":  "[remote \"origin\"\n\turl = https://u:p@h/r\n",
		"clean":   "[remote \"origin\"]\n\turl = git@github.com:o/r.git\n[remote \"up\"]\n\turl = https://github.com/o/r.git\n",
	}
	extra := map[string]string{ //nolint:gosec // G101: test fixtures, not credentials
		"include/.git/creds":            "[remote \"origin\"]\n\turl = https://tok@example.com/r\n",
		"clean/.git/modules/sub/config": "[remote \"origin\"]\n\turl = https://tok@example.com/sub\n",
	}
	known := `{"tok": {"source": {"source": "git", "url": "https://ghp_abc@github.com/o/r.git"}}, ` +
		`"clean": {"source": {"source": "github", "repo": "o/r"}}}`
	want := func(store string) []string {
		file := func(n, f string) string { return filepath.Join(store, ".claude/plugins/marketplaces", n, f) }
		return sorted(
			"marketplace_secret file="+file("clean", ".git/modules/sub/config")+" name=clean why=remote.origin.url",
			"marketplace_secret file="+file("header", ".git/config")+" name=header why=http.https://github.com/.extraheader",
			"marketplace_secret file="+file("hdrurl", ".git/config")+" name=hdrurl why=http.<url>.extraheader",
			"marketplace_secret file="+file("helper", ".git/config")+" name=helper why=remote.origin.url",
			"marketplace_secret file="+file("include", ".git/config")+" name=include why=remote.origin.url",
			"marketplace_secret file="+file("instead", ".git/config")+" name=instead why=url.<url>.insteadof",
			"marketplace_secret file="+file("missing", ".git/config")+" name=missing why=remote.origin.url",
			"marketplace_secret file="+file("proxy", ".git/config")+" name=proxy why=http.proxy",
			"marketplace_secret file="+file("push", ".git/config")+" name=push why=remote.origin.pushurl",
			"marketplace_secret file="+file("tok", ".git/config")+" name=tok why=remote.origin.url",
			"marketplace_secret file="+file("user", ".git/config")+" name=user why=remote.origin.url",
			"marketplace_secret file="+filepath.Join(store, ".claude/plugins/known_marketplaces.json")+" name=tok why=source.url",
		)
	}
	got := func(said []string) (secrets []string, unchecked string) {
		for _, s := range said {
			switch {
			case strings.HasPrefix(s, "marketplace_secret "):
				secrets = append(secrets, s)
			case strings.HasPrefix(s, "marketplace_unchecked "):
				unchecked = s
			}
			if strings.Contains(s, "ghp_abc") || strings.Contains(s, ":pw@") {
				t.Errorf("a warning shows the credential: %s", s)
			}
		}
		slices.Sort(secrets)
		return secrets, unchecked
	}

	im := newImporter(t)
	files := map[string]string{".claude/plugins/known_marketplaces.json": known}
	for name, c := range configs {
		files[".claude/plugins/marketplaces/"+name+"/.git/config"] = c
	}
	for rel, c := range extra {
		files[".claude/plugins/marketplaces/"+rel] = c
	}
	write(t, im.Dirs.Home, files)
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := agentStore(im.Dirs, "claude")
	secrets, unchecked := got(im.got)
	if !slices.Equal(secrets, want(store)) {
		t.Errorf("import warned\n%q, want\n%q", secrets, want(store))
	}
	if !strings.HasPrefix(unchecked, "marketplace_unchecked file="+filepath.Join(store, ".claude/plugins/marketplaces/broken/.git/config")+" name=broken why=") {
		t.Errorf("unchecked = %q", unchecked)
	}
	if got := readFile(t, filepath.Join(store, ".claude/plugins/marketplaces/tok/.git/config")); got != configs["tok"] {
		t.Errorf("tok's config = %q", got)
	}

	es := []te{
		{name: ".claude/plugins/"},
		{name: ".claude/plugins/marketplaces/"},
		{name: ".claude/plugins/marketplaces/tok/"},
		{name: ".claude/plugins/marketplaces/tok/.git/"},
		{name: ".claude/plugins/marketplaces/tok/.git/config", data: configs["tok"]},
	}
	st := newSaveTest(t, saveTar(t, true, es...), "claude")
	p, err := st.plan("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.applyAll(t.Context(), []agentPlan{p}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	saved, _ := got(st.got)
	tok := "marketplace_secret file=" + filepath.Join(st.store("claude"), ".claude/plugins/marketplaces/tok/.git/config") + " name=tok why=remote.origin.url"
	if !slices.Equal(saved, []string{tok}) {
		t.Errorf("save warned %q", saved)
	}
}
