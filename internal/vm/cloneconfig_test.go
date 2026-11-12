package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/hostgit"
)

// cleanURL takes credentials out of an http(s) URL (a token can be the
// user's name alone, or its query), and a password out of an ssh one,
// whose user it keeps; a scp-like URL goes as it is, its user what's
// before an @ ahead of its first colon (past a bracketed host), but one
// whose path holds an @ and then a colon (user:token@host:path, which
// git reads as host user) stays; file:// URLs and paths go as they are;
// anything else stays.
func TestCleanURL(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"https://github.com/x/y.git", "https://github.com/x/y.git", true},
		{"https://ghp_TOKEN@github.com/x/y.git", "https://github.com/x/y.git", true},
		{"https://u:ghp_TOKEN@github.com/x/y.git", "https://github.com/x/y.git", true},
		{"https://github.com/x/y.git?token=ghp_TOKEN#frag", "https://github.com/x/y.git", true},
		{"HTTP://u:p@example.com:8080/r", "http://example.com:8080/r", true},
		{"ssh://git@github.com/x/y.git", "ssh://git@github.com/x/y.git", true},
		{"ssh://git:secret@github.com:22/x/y.git?a=b", "ssh://git@github.com:22/x/y.git", true},
		{"git+ssh://me@host/r", "git+ssh://me@host/r", true},
		{"git@github.com:x/y.git", "git@github.com:x/y.git", true},
		{"me:ghp_TOKEN@github.com:x/y.git", "", false},
		{"github.com:x/y.git", "github.com:x/y.git", true},
		{"host:repo@v2.git", "host:repo@v2.git", true},
		{"git@host:a@b", "git@host:a@b", true},
		{"[::1]:x/y.git", "[::1]:x/y.git", true},
		{"git@[::1]:x/y.git", "git@[::1]:x/y.git", true},
		{"file:///srv/repo.git", "file:///srv/repo.git", true},
		{"/srv/repo.git", "/srv/repo.git", true},
		{"../other", "../other", true},
		{"./a:b@c:d", "./a:b@c:d", true},
		{"git://example.com/r", "", false},
		{"ext::sh -c touch% /tmp/x", "", false},
		{"fd::3", "", false},
		{"", "", false},
	} {
		if got, ok := cleanURL(c.in); got != c.want || ok != c.ok {
			t.Errorf("cleanURL(%q) = %q, %v", c.in, got, ok)
		}
	}
}

// settingsRepo is a repo with remotes of every kind cleanURL sees, an
// insteadOf and a pushInsteadOf, a push URL that reaches a devvm VM,
// upstreams, one of a branch the seed doesn't send, a submodule's
// settings, sparse checkout's switches, one with no value, its own
// identity, settings that never go, and hooks: a pre-commit, a link, one
// too big, one that's a folder, and a file no hook is named.
func settingsRepo(t *testing.T) string {
	t.Helper()
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	for _, kv := range [][2]string{
		{"remote.origin.url", "gh:x/y.git"},
		{"url.https://u:ghp_TOKEN@github.com/.insteadOf", "gh:"},
		{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"remote.mirror.url", "ssh://git:pw@host/r"},
		{"remote.mirror.pushurl", "https://u:ghp_PUSH@example.com/r"},
		{"remote.pi.url", "https://example.com/pi"},
		{"url.ssh://git@example.com/.pushInsteadOf", "https://example.com/"},
		{"remote.vmpush.url", "https://example.com/v"},
		{"remote.vmpush.pushurl", "devvm-web-0123456789abcd:/p"},
		{"remote.odd.url", "ext::sh -c x"},
		{"remote.odd.fetch", "+refs/heads/*:refs/remotes/odd/*"},
		{"branch.local.remote", "."},
		{"branch.gone.remote", "odd"},
		{"remote.devvm.url", "devvm-web-0123456789abcd:/p"},
		{"branch.main.remote", "origin"},
		{"branch.main.merge", "refs/heads/main"},
		{"branch.other.remote", "devvm"},
		{"branch.secret-name.remote", "origin"},
		{"branch.secret-name.merge", "refs/heads/secret-name"},
		{"submodule.lib.url", "https://u:ghp_SUB@github.com/x/lib.git"},
		{"submodule.lib.update", "!touch /tmp/x"},
		{"submodule.lib.active", "true"},
		{"core.sparseCheckout", "false"},
		{"user.name", "Repo Person"},
		{"user.email", "repo@example.com"},
		{"credential.helper", "store"},
		{"http.extraHeader", "Authorization: Bearer ghp_HEADER"},
		{"core.sshCommand", "ssh -i key"},
		{"alias.st", "status"},
		{"filter.crypt.clean", "crypt"},
	} {
		gitIn(t, dir, "config", "--add", kv[0], kv[1])
	}
	appendConfig(t, dir, "[core]\n\tsparseCheckoutCone\n")
	hooks := filepath.Join(dir, ".git", "hooks")
	writeIn(t, dir, ".git/hooks/pre-commit", "#!/bin/sh\necho ran > hook-ran\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o700); err != nil { //nolint:gosec // a hook runs
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}
	writeIn(t, dir, ".git/hooks/post-merge", strings.Repeat("#", hookMax+1))
	writeIn(t, dir, ".git/hooks/pre-rebase/x", "x")
	writeIn(t, dir, ".git/hooks/not-a-hook", "x")
	return dir
}

// appendConfig adds text to the end of the repo at dir's .git/config, as
// git config can't write it, such as a key with no value.
func appendConfig(t *testing.T, dir, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the test's own repo
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// configPairs are c's pairs as key=value, sorted.
func configPairs(c seedConfig) []string {
	var got []string
	for i := 0; i+1 < len(c.pairs); i += 2 {
		got = append(got, c.pairs[i]+"="+c.pairs[i+1])
	}
	slices.Sort(got)
	return got
}

// branchRefs are the seed's refs for branches, each named.
func branchRefs(names ...string) []seedRef {
	var refs []seedRef
	for _, n := range names {
		refs = append(refs, seedRef{name: "refs/heads/" + n})
	}
	return refs
}

// The settings that go: the remotes' URLs resolved and cleaned, a push
// URL where git pushes elsewhere, pushInsteadOf's too, their fetch
// specs, the upstreams of the branches the seed sends, the submodule's
// URL and switches, sparse checkout's switches, one with no value as
// true, and the repo's identity; not another VM's remote, nor a push URL
// that reaches a VM (the remote named), nor a branch's upstream on it,
// nor a branch the seed doesn't send, a URL of a kind that doesn't go
// (named), nor that remote's other settings or a branch's upstream on
// it, an update that runs a command, nor anything outside the allowlist;
// an upstream in the repo itself (".") goes. The hooks named as git's,
// regular and in bounds; the link, the folder and the big one named.
func TestReadConfig(t *testing.T) {
	dir := settingsRepo(t)
	c, err := readConfig(t.Context(), dir, branchRefs("main", "local", "gone", "other"))
	if err != nil {
		t.Fatal(err)
	}
	got := configPairs(c)
	want := []string{
		"remote.origin.url=https://github.com/x/y.git",
		"remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
		"remote.mirror.url=ssh://git@host/r",
		"remote.mirror.pushurl=https://example.com/r",
		"remote.pi.url=https://example.com/pi",
		"remote.pi.pushurl=ssh://git@example.com/pi",
		"remote.vmpush.url=https://example.com/v",
		"branch.main.remote=origin",
		"branch.main.merge=refs/heads/main",
		"branch.local.remote=.",
		"submodule.lib.url=https://github.com/x/lib.git",
		"submodule.lib.active=true",
		"core.sparsecheckout=false",
		"core.sparsecheckoutcone=true",
		"user.name=Repo Person",
		"user.email=repo@example.com",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("pairs:\n%s", strings.Join(got, "\n"))
	}
	all := strings.Join(got, "\n")
	for _, secret := range []string{"ghp_", "pw@", "Bearer", "store", "ssh -i", "devvm-web", "crypt", "!touch", "secret-name"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q went", secret)
		}
	}
	if !slices.Equal(c.urlsKept, []string{"odd"}) || !slices.Equal(c.pushKept, []string{"vmpush"}) || c.urlsUnread != nil {
		t.Errorf("urls kept %q, push kept %q, unread %q", c.urlsKept, c.pushKept, c.urlsUnread)
	}
	var hooks []string
	for _, h := range c.hooks {
		hooks = append(hooks, h.name)
	}
	if !slices.Equal(hooks, []string{"pre-commit"}) || !c.hooks[0].exec {
		t.Errorf("hooks %q", hooks)
	}
	slices.Sort(c.hooksKept)
	if !slices.Equal(c.hooksKept, []string{"post-merge", "pre-push", "pre-rebase"}) || c.hooksDirKept != "" {
		t.Errorf("hooks kept %q, folder kept %q", c.hooksKept, c.hooksDirKept)
	}
}

// A remote whose URLs git can't give doesn't go, and is named.
func TestReadConfigUnread(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	appendConfig(t, dir, "[remote \"w\"]\n\turl\n")
	c, err := readConfig(t.Context(), dir, branchRefs("main"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.urlsUnread, []string{"w"}) || slices.ContainsFunc(c.pairs, func(p string) bool { return strings.HasPrefix(p, "remote.w") }) {
		t.Errorf("unread %q, pairs %q", c.urlsUnread, c.pairs)
	}
}

// In the VM: the settings set, the pre-commit hook, which runs; a try
// again changes nothing the VM set since.
func TestSeedConfig(t *testing.T) {
	dir := settingsRepo(t)
	vm := seedOK(t, dir)
	if got := gitIn(t, vm, "remote", "get-url", "origin"); got != "https://github.com/x/y.git" {
		t.Errorf("origin %q", got)
	}
	if got := gitIn(t, vm, "config", "user.name"); got != "Repo Person" {
		t.Errorf("user.name %q", got)
	}
	local := gitIn(t, vm, "config", "--local", "--list")
	for _, never := range []string{"ghp_", "credential", "extraheader", "sshcommand", "alias", "filter", "insteadof", "remote.devvm", "remote.odd"} {
		if strings.Contains(strings.ToLower(local), strings.ToLower(never)) {
			t.Errorf("%q in the VM's config:\n%s", never, local)
		}
	}
	gitIn(t, vm, "commit", "-q", "--allow-empty", "-m", "hooked")
	if _, err := os.Stat(filepath.Join(vm, "hook-ran")); err != nil {
		t.Error("the pre-commit hook didn't run in the VM")
	}
	if _, err := os.Lstat(filepath.Join(vm, ".git", "hooks", "pre-push")); err == nil {
		t.Error("the linked hook is in the VM")
	}
}

// The core.hooksPath git uses here, the last one: inside the repo, the
// setting alone goes, no hook file; .git/hooks is the hooks folder, whose
// hooks go; one outside the repo, through ~ or %(prefix), or that an
// included file or your global config sets, is named, and no hook goes.
func TestReadConfigHooksPath(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	writeIn(t, dir, ".githooks/pre-commit", "#!/bin/sh\n")
	writeIn(t, dir, ".git/hooks/commit-msg", "#!/bin/sh\n")
	for _, c := range []struct {
		values      []string
		sent, kept  string
		folderHooks bool
	}{
		{[]string{".githooks"}, ".githooks", "", false},
		{[]string{"./.githooks/"}, "./.githooks/", "", false},
		{[]string{".git/hooks"}, "", "", true},
		{[]string{"/etc/hooks"}, "", "/etc/hooks", false},
		{[]string{"~/hooks"}, "", "~/hooks", false},
		{[]string{"../out"}, "", "../out", false},
		{[]string{"%(prefix)/hooks"}, "", "%(prefix)/hooks", false},
		{[]string{".githooks", "/etc/hooks"}, "", "/etc/hooks", false},
		{[]string{"/etc/hooks", ".githooks"}, ".githooks", "", false},
	} {
		_, _ = plainGit(t, dir, "config", "--unset-all", "core.hooksPath")
		for _, v := range c.values {
			gitIn(t, dir, "config", "--add", "core.hooksPath", v)
		}
		got, err := readConfig(t.Context(), dir, branchRefs("main"))
		if err != nil {
			t.Fatal(err)
		}
		sent := ""
		if i := slices.Index(got.pairs, "core.hookspath"); i >= 0 {
			sent = got.pairs[i+1]
		}
		if sent != c.sent || got.hooksPathKept != c.kept || (len(got.hooks) == 1 && got.hooks[0].name == "commit-msg") != c.folderHooks || len(got.hooks) > 1 {
			t.Errorf("%q: sent %q, kept %q, hooks %v", c.values, sent, got.hooksPathKept, got.hooks)
		}
	}
	_, _ = plainGit(t, dir, "config", "--unset-all", "core.hooksPath")
	writeIn(t, dir, ".git/inc", "[core]\n\thooksPath = .githooks\n")
	gitIn(t, dir, "config", "include.path", "inc")
	if c, err := readConfig(t.Context(), dir, branchRefs("main")); err != nil || c.hooksPathKept != ".githooks" || len(c.hooks) != 0 || slices.Contains(c.pairs, "core.hookspath") {
		t.Errorf("an included core.hooksPath: %+v, %v", c, err)
	}
	// The repo's own, then an include after it, which wins, as in git.
	gitIn(t, dir, "config", "--unset", "include.path")
	gitIn(t, dir, "config", "core.hooksPath", ".githooks")
	writeIn(t, dir, ".git/inc", "[core]\n\thooksPath = /etc/later\n")
	gitIn(t, dir, "config", "include.path", "inc")
	if c, err := readConfig(t.Context(), dir, branchRefs("main")); err != nil || c.hooksPathKept != "/etc/later" || slices.Contains(c.pairs, "core.hookspath") {
		t.Errorf("an include after the repo's own: %+v, %v", c, err)
	}
	_, _ = plainGit(t, dir, "config", "--unset-all", "core.hooksPath")
	gitIn(t, dir, "config", "--unset", "include.path")
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeIn(t, filepath.Dir(global), "gitconfig", "[core]\n\thooksPath = /opt/hooks\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if c, err := readConfig(t.Context(), dir, branchRefs("main")); err != nil || c.hooksPathKept != "/opt/hooks" || len(c.hooks) != 0 {
		t.Errorf("a global core.hooksPath: %+v, %v", c, err)
	}
}

// In a linked worktree, whose .git is a file, a relative .git/hooks
// names no folder, so git there runs no hook: none goes, nor the setting.
func TestReadConfigHooksPathWorktree(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	writeIn(t, dir, ".git/hooks/commit-msg", "#!/bin/sh\n")
	wt := filepath.Join(filepath.Dir(dir), "wt")
	gitIn(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
	gitIn(t, dir, "config", "core.hooksPath", ".git/hooks")
	c, err := readConfig(t.Context(), wt, branchRefs("wt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.hooks) != 0 || c.hooksPathKept != "" || slices.Contains(c.pairs, "core.hookspath") {
		t.Errorf("hooks %v, kept %q, pairs %q", c.hooks, c.hooksPathKept, c.pairs)
	}
	if c, err := readConfig(t.Context(), dir, branchRefs("main")); err != nil || len(c.hooks) != 1 {
		t.Errorf("the main worktree: hooks %v, %v", c.hooks, err)
	}
}

// The settings go in once: a try again with the marker there keeps a key
// the VM changed since; without the marker, they go in again.
func TestSeedConfigRetry(t *testing.T) {
	dir := settingsRepo(t)
	vm := seedOK(t, dir)
	if _, err := os.Stat(filepath.Join(vm, ".git", seedConfigMarker)); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	gitIn(t, vm, "config", "user.name", "VM Person")
	again := func() {
		t.Helper()
		s, err := planSeed(t.Context(), ".", dir)
		if err != nil {
			t.Fatal(err)
		}
		data, err := readSeedState(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		var tb bytes.Buffer
		if err := data.writeTar(&tb); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vm, ".git", seedMarker), []byte("pending\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runSeedScript(t, seedStage, &tb, vm); err != nil {
			t.Fatalf("stage: %v", err)
		}
		pack := hostgit.Command(t.Context(), "-C", dir, "pack-objects", "--revs", "--stdout", "-q")
		pack.Stdin = strings.NewReader(data.tips())
		p, err := pack.Output()
		if err != nil {
			t.Fatalf("pack: %v", err)
		}
		if out, err := runSeedScript(t, seedApply, bytes.NewReader(p), vm); err != nil {
			t.Fatalf("apply: %v (%q)", err, out)
		}
	}
	again()
	if got := gitIn(t, vm, "config", "user.name"); got != "VM Person" {
		t.Errorf("with the marker: user.name %q", got)
	}
	if err := os.Remove(filepath.Join(vm, ".git", seedConfigMarker)); err != nil {
		t.Fatal(err)
	}
	again()
	if got := gitIn(t, vm, "config", "--get-all", "user.name"); got != "Repo Person" {
		t.Errorf("without the marker: user.name %q", got)
	}
}

// A hooks folder that's missing or a link sends no hook, and is named.
func TestReadConfigHooksFolder(t *testing.T) {
	for _, how := range []string{"missing", "link"} {
		t.Run(how, func(t *testing.T) {
			dir := safetyRepo(t, map[string]string{"a": "a\n"})
			hooks := filepath.Join(dir, ".git", "hooks")
			if err := os.RemoveAll(hooks); err != nil {
				t.Fatal(err)
			}
			if how == "link" {
				other := t.TempDir()
				writeIn(t, other, "pre-commit", "#!/bin/sh\n")
				if err := os.Symlink(other, hooks); err != nil {
					t.Fatal(err)
				}
			}
			c, err := readConfig(t.Context(), dir, branchRefs("main"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(c.hooksDirKept, filepath.Join(".git", "hooks")) || len(c.hooks) != 0 {
				t.Errorf("folder kept %q, hooks %v", c.hooksDirKept, c.hooks)
			}
		})
	}
}

// husky's layouts, v8's core.hooksPath .husky, whose _ folder git
// ignores, and v9's .husky/_, which git ignores: the VM's clone gets the
// setting, the tracked files, and neither the ignored folder nor the git
// dir's hooks, which git here doesn't run.
func TestSeedConfigHusky(t *testing.T) {
	for _, c := range []struct{ name, hooksPath, ignored string }{
		{"v8", ".husky", ".husky/_/husky.sh"},
		{"v9", ".husky/_", ".husky/_/pre-commit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := safetyRepo(t, map[string]string{".husky/pre-commit": "npx lint-staged\n"})
			writeIn(t, dir, ".husky/_/.gitignore", "*\n")
			writeIn(t, dir, c.ignored, "#!/bin/sh\n")
			writeIn(t, dir, ".git/hooks/pre-commit", "#!/bin/sh\nexit 1\n")
			gitIn(t, dir, "config", "core.hooksPath", c.hooksPath)
			vm := seedOK(t, dir)
			if got := gitIn(t, vm, "config", "core.hooksPath"); got != c.hooksPath {
				t.Errorf("core.hooksPath %q", got)
			}
			for _, p := range []string{".husky/_", ".git/hooks/pre-commit"} {
				if _, err := os.Lstat(filepath.Join(vm, p)); err == nil {
					t.Errorf("%s is in the VM", p)
				}
			}
			if _, err := os.Stat(filepath.Join(vm, ".husky", "pre-commit")); err != nil {
				t.Errorf("the tracked .husky/pre-commit: %v", err)
			}
		})
	}
}

// A sparse checkout, whose switches git sparse-checkout writes to the
// worktree's config.worktree, is one in the VM too, in the main worktree
// and in a linked one.
func TestSeedConfigSparse(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "main", true: "linked"}[linked], func(t *testing.T) {
			dir := safetyRepo(t, map[string]string{"a/f": "a\n", "b/f": "b\n"})
			if linked {
				wt := filepath.Join(filepath.Dir(dir), "wt")
				gitIn(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
				dir = wt
			}
			gitIn(t, dir, "sparse-checkout", "set", "a")
			vm := seedOK(t, dir)
			if got := gitIn(t, vm, "config", "--bool", "core.sparseCheckout"); got != "true" {
				t.Errorf("core.sparseCheckout %q", got)
			}
			if got := gitIn(t, vm, "sparse-checkout", "list"); got != "a" {
				t.Errorf("sparse-checkout list %q", got)
			}
			if _, err := os.Lstat(filepath.Join(vm, "b", "f")); err == nil {
				t.Error("b/f, outside the sparse checkout, is in the VM")
			}
			if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
				t.Errorf("status:\n%s", got)
			}
		})
	}
}

// The seed's git runs no hook: with a core.hooksPath whose
// reference-transaction hook fails, as the VM's own config could have,
// the seed still sets the refs.
func TestSeedRunsNoHook(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	hooks := t.TempDir()
	writeIn(t, hooks, "reference-transaction", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "reference-transaction"), 0o700); err != nil { //nolint:gosec // a hook runs
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeIn(t, filepath.Dir(global), "gitconfig", "[core]\n\thooksPath = "+hooks+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	vm := seedOK(t, dir)
	if got := gitIn(t, vm, "rev-parse", "HEAD"); got != gitIn(t, dir, "rev-parse", "HEAD") {
		t.Errorf("HEAD %q", got)
	}
}
