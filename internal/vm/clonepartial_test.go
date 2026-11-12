package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// partialRepo is a partial clone, with git clone filter, of an origin of
// two commits whose first holds a file the second removes, so the clone
// lacks that file's blob, which it returns, and its origin, a bare repo
// that allows filters. With sparse, it's a sparse clone too, of a's
// folder alone, so its index names blobs it never fetched.
func partialRepo(t *testing.T, filter string, sparse bool) (dir, missing string) {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	gitIn(t, root, "init", "-q", "-b", "main", src)
	writeIn(t, src, "old", strings.Repeat("old\n", 100))
	writeIn(t, src, "a/f", "a\n")
	writeIn(t, src, "b/f", "b\n")
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "one")
	missing = gitIn(t, src, "rev-parse", "HEAD:old")
	gitIn(t, src, "rm", "-q", "old")
	gitIn(t, src, "commit", "-q", "-m", "two")
	bare := filepath.Join(root, "origin.git")
	gitIn(t, root, "clone", "-q", "--bare", src, bare)
	gitIn(t, bare, "config", "uploadpack.allowFilter", "true")
	dir = filepath.Join(root, "host")
	args := []string{"clone", "-q", "--filter=" + filter}
	if sparse {
		args = append(args, "--sparse")
	}
	gitIn(t, root, append(args, "file://"+bare, dir)...)
	if sparse {
		gitIn(t, dir, "sparse-checkout", "set", "a")
	}
	cmd := stubtest.Git(t.Context(), "-C", dir, "cat-file", "-e", missing)
	cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	if cmd.Run() == nil {
		t.Fatal("the partial clone has the blob it should lack")
	}
	return dir, missing
}

// An older git's partial clone names its promisor with
// extensions.partialClone: the VM's clone gets remote.<name>.promisor.
func TestReadConfigPartialExtension(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/x/y.git")
	gitIn(t, dir, "config", "core.repositoryformatversion", "1")
	gitIn(t, dir, "config", "extensions.partialClone", "origin")
	c, err := readConfig(t.Context(), dir, branchRefs("main"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.pairs, "remote.origin.promisor") || c.pairs[slices.Index(c.pairs, "remote.origin.promisor")+1] != "true" {
		t.Errorf("pairs %q", c.pairs)
	}
}

// The host and port a promisor remote's URL reaches, local for one on
// this machine, and no host for one it can't read; and the remotes the
// settings make promisors.
func TestPromisorHosts(t *testing.T) {
	for _, c := range []struct {
		url   string
		host  string
		port  int
		local bool
	}{
		{"https://github.com/x/y.git", "github.com", 443, false},
		{"http://git.example.com:8080/r", "git.example.com", 8080, false},
		{"ssh://git@host.example/r", "host.example", 22, false},
		{"git@github.com:x/y.git", "github.com", 22, false},
		{"git@Host.example:path@x", "host.example", 22, false},
		{"[::1]:repo", "::1", 22, false},
		{"git@[fd00::1]:repo", "fd00::1", 22, false},
		{"https://h.example:bad/r", "", 0, false},
		{"git://h.example/r", "", 0, false},
		{"file:///srv/r.git", "", 0, true},
		{"/srv/r.git", "", 0, true},
		{"./a:b", "", 0, true},
	} {
		if host, port, local := urlHostPort(c.url); host != c.host || port != c.port || local != c.local {
			t.Errorf("urlHostPort(%q) = %q, %d, %v", c.url, host, port, local)
		}
	}
	got := promisors(seedConfig{pairs: []string{
		"remote.origin.url", "https://a/r", "remote.origin.promisor", "true",
		"remote.up.url", "https://b/r", "remote.up.promisor", "false",
		"remote.nourl.promisor", "true",
		"remote.last.url", "https://c/r", "remote.last.promisor", "true", "remote.last.promisor", "false",
		"branch.x.url", "https://d/r", "branch.x.promisor", "true",
	}})
	if len(got) != 1 || got["origin"] != "https://a/r" {
		t.Errorf("promisors %v", got)
	}
}

// The repo's promisors as git reads them: one value git can't read as a
// boolean drops only its own remote, and a remote's last value wins.
func TestHostPromisors(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	appendConfig(t, dir, "[remote \"bad\"]\n\tpromisor = maybe\n[remote \"yes\"]\n\tpromisor\n[remote \"off\"]\n\tpromisor = true\n\tpromisor = false\n[remote \"on\"]\n\tpromisor = false\n\tpromisor = 1\n")
	if got := hostPromisors(t.Context(), dir); !slices.Equal(got, []string{"on", "yes"}) {
		t.Errorf("promisors %q", got)
	}
	if !partialClone(t.Context(), dir) {
		t.Error("not taken for a partial clone")
	}
}

// A partial clone's seed names what keeps the VM from what it lacks: the
// refs left out, the promisor remotes that stay, one on this machine,
// and, by tier, a promisor's host: on none any, with how to raise it; on
// the allowlist one it doesn't allow; on internet one a deny rule blocks
// or a private address; on open none. Several promisors in name order.
// Nothing for a repo that isn't partial.
func TestPartialWarnings(t *testing.T) {
	data := func(url string, partial bool) seedData {
		return seedData{partial: partial, config: seedConfig{pairs: []string{"remote.origin.url", url, "remote.origin.promisor", "true"}}}
	}
	r := reach{
		allows: func(host string, port int) bool { return host == "github.com" && port == 443 },
		denies: func(host string, _ int) bool { return host == "denied.example" },
	}
	two := seedData{partial: true, config: seedConfig{pairs: []string{
		"remote.zed.url", "file:///z", "remote.zed.promisor", "true",
		"remote.alpha.url", "file:///a", "remote.alpha.promisor", "true",
	}}}
	kept := seedData{partial: true, refsMissing: []string{"refs/tags/t"}, promisorsKept: []string{"origin"}}
	for _, c := range []struct {
		data seedData
		tier netpolicy.Tier
		want string
	}{
		{data("https://git.example.com/x.git", true), netpolicy.TierAllowlist, "clone_partial_blocked dir=/d host=git.example.com remote=origin"},
		{data("https://github.com/x.git", true), netpolicy.TierAllowlist, ""},
		{data("https://github.com/x.git", true), netpolicy.TierNone, "clone_partial_blocked_none dir=/d host=github.com remote=origin"},
		{data("https://git.example.com/x.git", true), netpolicy.TierInternet, ""},
		{data("https://denied.example/x.git", true), netpolicy.TierInternet, "clone_partial_denied dir=/d host=denied.example remote=origin"},
		{data("https://10.0.0.5/x.git", true), netpolicy.TierInternet, "clone_partial_denied dir=/d host=10.0.0.5 remote=origin"},
		{data("https://git.example.com/x.git", true), netpolicy.TierOpen, ""},
		{data("https://h.example:bad/x.git", true), netpolicy.TierNone, ""},
		{data("file:///srv/x.git", true), netpolicy.TierOpen, "clone_partial_unreachable dir=/d remote=origin url=file:///srv/x.git"},
		{data("https://git.example.com/x.git", false), netpolicy.TierNone, ""},
		{two, netpolicy.TierOpen, "clone_partial_unreachable dir=/d remote=alpha url=file:///a\nclone_partial_unreachable dir=/d remote=zed url=file:///z"},
		{kept, netpolicy.TierOpen, `clone_partial_refs_kept count=1 dir=/d refs="refs/tags/t"` + "\n" + `clone_partial_promisor_kept count=1 dir=/d remotes="origin"`},
		{data("git@github.com:x/y.git", true), netpolicy.TierAllowlist, "clone_partial_blocked dir=/d host=github.com:22 remote=origin"},
		{data("ssh://git@[fd00::1]:2222/x", true), netpolicy.TierNone, "clone_partial_blocked_none dir=/d host=[fd00::1]:2222 remote=origin"},
	} {
		var got []string
		for _, m := range partialWarnings("/d", c.data, c.tier, r) {
			got = append(got, m.Names())
		}
		if strings.Join(got, "\n") != c.want {
			t.Errorf("%s %v: %q, want %q", c.tier, c.data.config.pairs, got, c.want)
		}
	}
	// With no policies to read, nothing is judged against them: the
	// allowlist names nothing, internet only a private address.
	for _, c := range []struct {
		data seedData
		tier netpolicy.Tier
		want string
	}{
		{data("https://git.example.com/x.git", true), netpolicy.TierAllowlist, ""},
		{data("https://denied.example/x.git", true), netpolicy.TierInternet, ""},
		{data("https://10.0.0.5/x.git", true), netpolicy.TierInternet, "clone_partial_denied dir=/d host=10.0.0.5 remote=origin"},
	} {
		var got []string
		for _, m := range partialWarnings("/d", c.data, c.tier, reach{}) {
			got = append(got, m.Names())
		}
		if strings.Join(got, "\n") != c.want {
			t.Errorf("no policies, %s %v: %q, want %q", c.tier, c.data.config.pairs, got, c.want)
		}
	}
}

// A promisor is sent as git reads it: yes, 1 or no value at all is true.
func TestReadConfigPromisorBool(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	for _, r := range []string{"a", "b", "c"} {
		gitIn(t, dir, "remote", "add", r, "https://example.com/"+r)
	}
	gitIn(t, dir, "config", "remote.a.promisor", "yes")
	gitIn(t, dir, "config", "remote.b.promisor", "1")
	appendConfig(t, dir, "[remote \"c\"]\n\tpromisor\n")
	c, err := readConfig(t.Context(), dir, branchRefs("main"))
	if err != nil {
		t.Fatal(err)
	}
	if got := promisors(c); len(got) != 3 {
		t.Errorf("promisors %v, pairs %q", got, c.pairs)
	}
}

// A partial clone whose promisor remote doesn't go, as its URL is of a
// kind that stays, names it; one that's a VM's remote too. A promisor
// that isn't origin goes by its own name.
func TestSeedPartialPromisors(t *testing.T) {
	dir, _ := partialRepo(t, "blob:none", false)
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "remote", "set-url", "origin", "ext::sh -c x")
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(data.promisorsKept, []string{"origin"}) {
		t.Errorf("promisors kept %q", data.promisorsKept)
	}
	gitIn(t, dir, "remote", "set-url", "origin", "devvm-web-0123456789abcd:/p")
	if data, err = readSeedState(t.Context(), s); err != nil || !slices.Equal(data.promisorsKept, []string{"origin"}) {
		t.Errorf("a VM's remote: promisors kept %q, %v", data.promisorsKept, err)
	}
	gitIn(t, dir, "remote", "rename", "origin", "up")
	gitIn(t, dir, "remote", "set-url", "up", "https://example.com/x.git")
	if data, err = readSeedState(t.Context(), s); err != nil || data.promisorsKept != nil || promisors(data.config)["up"] == "" {
		t.Errorf("up: promisors kept %q, sent %v, %v", data.promisorsKept, promisors(data.config), err)
	}
}

// A ref whose object a partial clone lacks is left out of the seed, and
// named, and the rest goes.
func TestSeedPartialMissingRef(t *testing.T) {
	dir, missing := partialRepo(t, "blob:none", false)
	writeIn(t, dir, ".git/refs/tags/blob", missing+"\n")
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(data.refsMissing, []string{"refs/tags/blob"}) {
		t.Errorf("refs missing %q", data.refsMissing)
	}
	vm := seedOK(t, dir)
	if _, err := plainGit(t, vm, "rev-parse", "-q", "--verify", "refs/tags/blob"); err == nil {
		t.Error("the VM has the tag")
	}
	// A repo that isn't partial with such a ref is broken: named.
	broken := safetyRepo(t, map[string]string{"a": "a\n"})
	writeIn(t, broken, ".git/refs/tags/blob", missing+"\n")
	if s, err = planSeed(t.Context(), ".", broken); err != nil {
		t.Fatal(err)
	}
	_, err = readSeedState(t.Context(), s)
	var m msg.M
	if !errors.As(err, &m) || m.Names() != "clone_ref_no_object ref=refs/tags/blob" {
		t.Errorf("not partial: %v", err)
	}
}

// pack-objects that ends before reading rev-list's list (here, an option
// it refuses) ends the pack at once: rev-list, whose list is more than
// a pipe holds, gets an error on its next write and ends too, rather
// than wait for a reader this process still held.
func TestPackPartialPackDiesEarly(t *testing.T) {
	dir, _ := partialRepo(t, "blob:none", false)
	var in strings.Builder
	in.WriteString("commit refs/heads/many\ncommitter t <t@t> 0 +0000\ndata 4\nmany\nfrom refs/heads/main\n")
	for i := range 5000 {
		body := strconv.Itoa(i)
		fmt.Fprintf(&in, "M 100644 inline many/%d\ndata %d\n%s\n", i, len(body), body)
	}
	cmd := stubtest.Git(t.Context(), "-C", dir, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var stderr strings.Builder
		_, err := packOutput(t, dir, data, &stderr, func(c *exec.Cmd) { c.Args = append(c.Args, "--no-such-option") })
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("no error")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the pack hung")
	}
}

// A failing git rev-list fails the pack, cut short, which index-pack
// refuses, never a pack short of objects that says nothing: here a loose
// blob, which no promisor has, is gone.
func TestPackPartialListFails(t *testing.T) {
	dir, _ := partialRepo(t, "blob:none", false)
	writeIn(t, dir, "new", "new\n")
	gitIn(t, dir, "add", "new")
	gitIn(t, dir, "commit", "-q", "-m", "three")
	blob := gitIn(t, dir, "rev-parse", "HEAD:new")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	p, err := packOutput(t, dir, data, &stderr)
	if err == nil {
		t.Fatalf("no error; stderr %s", stderr.String())
	}
	other := t.TempDir()
	gitIn(t, other, "init", "-q")
	cmd := stubtest.Git(t.Context(), "-C", other, "index-pack", "--stdin")
	cmd.Stdin = strings.NewReader(string(p))
	if cmd.Run() == nil {
		t.Error("index-pack took the pack")
	}
}

// A partial clone that's shallow too, and a linked worktree of a partial
// clone, are copied.
func TestSeedPartialShallowWorktree(t *testing.T) {
	dir, _ := partialRepo(t, "blob:none", false)
	bare := gitIn(t, dir, "remote", "get-url", "origin")
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitIn(t, filepath.Dir(shallow), "clone", "-q", "--filter=blob:none", "--depth=1", bare, shallow)
	vm := seedOK(t, shallow)
	if got := gitIn(t, vm, "log", "--format=%s"); got != "two" {
		t.Errorf("shallow: log %q", got)
	}
	if got := gitIn(t, vm, "config", "remote.origin.promisor"); got != "true" {
		t.Errorf("shallow: promisor %q", got)
	}
	wt := filepath.Join(filepath.Dir(dir), "wt")
	gitIn(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
	vm = seedOK(t, wt)
	if got := gitIn(t, vm, "config", "remote.origin.promisor"); got != "true" {
		t.Errorf("worktree: promisor %q", got)
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
		t.Errorf("worktree: status:\n%s", got)
	}
}

// fileExists is whether there's a file at p.
func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A partial clone, blob:none or tree:0, sparse or not, is copied: the
// pack holds what the repo has, the VM's is a promisor pack, and its git
// fetches what it lacks from the repo's origin when it needs it.
func TestSeedPartial(t *testing.T) {
	for _, c := range []struct {
		filter string
		sparse bool
	}{
		{"blob:none", false},
		{"tree:0", false},
		{"blob:none", true},
	} {
		t.Run(strings.ReplaceAll(c.filter, ":", "-")+map[bool]string{true: "-sparse"}[c.sparse], func(t *testing.T) {
			dir, missing := partialRepo(t, c.filter, c.sparse)
			vm := seedOK(t, dir)
			if got := gitIn(t, vm, "config", "remote.origin.promisor"); got != "true" {
				t.Errorf("remote.origin.promisor %q", got)
			}
			packs, _ := filepath.Glob(filepath.Join(vm, ".git", "objects", "pack", "*.promisor"))
			if len(packs) == 0 {
				t.Error("no promisor pack in the VM")
			}
			if got := gitIn(t, vm, "log", "--format=%s", "-2"); got != "two\none" {
				t.Errorf("log %q", got)
			}
			if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
				t.Errorf("the VM's status:\n%s", got)
			}
			if _, err := plainGit(t, vm, "cat-file", "-e", "HEAD:b/f"); err != nil {
				t.Error("b/f isn't in the VM's history")
			}
			if c.sparse && fileExists(filepath.Join(vm, "b", "f")) {
				t.Error("b/f, outside the sparse checkout, is in the VM's working tree")
			}
			// Fetched from origin, as the VM's git does when it needs it.
			if got := gitIn(t, vm, "cat-file", "-s", missing); got != "400" {
				t.Errorf("the missing blob, fetched: %q", got)
			}
		})
	}
}
