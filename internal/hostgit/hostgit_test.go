package hostgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/stubtest"
)

// git runs plain git in dir, failing the test if it fails, and returns
// its output, trimmed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := stubtest.Git(t.Context(), append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo is a new repo with a commit, git given a config and identity of
// its own.
func repo(t *testing.T) string {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	return dir
}

// The tests' git works on their own repos, never on the one that a git
// running the tests names in its environment, as a hook's or git rebase
// --exec's does.
func TestRepoNotTheRunners(t *testing.T) {
	decoy := t.TempDir()
	if out, err := stubtest.Git(t.Context(), "init", "-q", decoy).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	config := filepath.Join(decoy, ".git", "config")
	was, err := os.ReadFile(config) //nolint:gosec // the test's own
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.bare'='true'")
	dir := repo(t)
	git(t, dir, "config", "core.fsmonitor", "/nowhere")
	if now, err := os.ReadFile(config); err != nil || string(now) != string(was) { //nolint:gosec // the test's own
		t.Errorf("the decoy's config changed:\n%s", now)
	}
	if got := git(t, dir, "config", "--file", filepath.Join(dir, ".git", "config"), "core.fsmonitor"); got != "/nowhere" {
		t.Errorf("the test's repo: core.fsmonitor %q", got)
	}
}

// marker is a program that touches a file and passes its input through,
// and that file, which it hasn't touched yet.
func marker(t *testing.T) (prog, ran string) {
	t.Helper()
	d := t.TempDir()
	ran, prog = filepath.Join(d, "ran"), filepath.Join(d, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+ran+"'\ncat\n"), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	return prog, ran
}

// ranOnce reports whether the marker's file is there, and removes it.
func ranOnce(ran string) bool { return os.Remove(ran) == nil }

// Command's git never asks on the terminal and never runs the repo's
// fsmonitor, which a repo's config can point at any program.
func TestCommandNoFsmonitor(t *testing.T) {
	dir := repo(t)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	cmd := Command(t.Context(), "status")
	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") || slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=1") {
		t.Errorf("env %q", cmd.Env)
	}
	prog, ran := marker(t)
	git(t, dir, "config", "core.fsmonitor", prog)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "f")
	if !ranOnce(ran) {
		t.Fatal("plain git didn't run the fsmonitor")
	}
	if out, err := Output(t.Context(), "-C", dir, "status", "--porcelain"); err != nil || string(out) != "A  f\n" {
		t.Fatalf("status %q, %v", out, err)
	}
	if ranOnce(ran) {
		t.Error("ran the repo's fsmonitor")
	}
}

// Command's git runs none of the repo's hooks: reference-transaction (any
// ref update: update-ref, fetch, remote remove) and pre-push, from .git/hooks
// or a core.hooksPath.
func TestCommandNoHooks(t *testing.T) {
	dir := repo(t)
	prog, ran := marker(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	for _, h := range []string{"reference-transaction", "pre-push"} {
		if err := os.Symlink(prog, filepath.Join(hooks, h)); err != nil {
			t.Fatal(err)
		}
	}
	bare := t.TempDir()
	git(t, bare, "init", "-q", "--bare")
	git(t, dir, "update-ref", "refs/heads/plain", "HEAD")
	if !ranOnce(ran) {
		t.Fatal("plain git didn't run reference-transaction")
	}
	git(t, dir, "push", "-q", bare, "main")
	if !ranOnce(ran) {
		t.Fatal("plain git didn't run pre-push")
	}
	for _, hp := range []string{"", hooks} {
		if hp != "" {
			git(t, dir, "config", "core.hooksPath", hp)
		}
		if out, err := Command(t.Context(), "-C", dir, "update-ref", "refs/heads/hardened", "HEAD").CombinedOutput(); err != nil {
			t.Fatalf("update-ref: %v\n%s", err, out)
		}
		if ranOnce(ran) {
			t.Errorf("hooksPath %q: ran reference-transaction", hp)
		}
		// pre-push as git runs it for a push, without one.
		_ = Command(t.Context(), "-C", dir, "hook", "run", "pre-push").Run()
		if ranOnce(ran) {
			t.Errorf("hooksPath %q: ran pre-push", hp)
		}
	}
	// Over ssh only: a push to a path or a file:// URL fails.
	for _, url := range []string{bare, "file://" + bare, "ext::sh -c true"} {
		if err := Command(t.Context(), "-C", dir, "push", "-q", url, "main").Run(); err == nil {
			t.Errorf("pushed to %s", url)
		}
	}
	if ranOnce(ran) {
		t.Error("a refused push ran a hook")
	}
}

// Command's git runs none of the hooks a repo's config names
// (hook.NAME.command and event), which git 2.54 and newer run.
func TestCommandNoConfigHooks(t *testing.T) {
	dir := repo(t)
	prog, ran := marker(t)
	git(t, dir, "config", "hook.mine.command", prog)
	git(t, dir, "config", "hook.mine.event", "reference-transaction")
	git(t, dir, "config", "--add", "hook.mine.event", "pre-push")
	git(t, dir, "config", "hook.other.thing.command", prog)
	git(t, dir, "config", "hook.other.thing.event", "pre-push")
	git(t, dir, "update-ref", "refs/heads/plain", "HEAD")
	if !ranOnce(ran) {
		t.Skip("this git runs no hooks a config names")
	}
	if out, err := Command(t.Context(), "-C", dir, "update-ref", "refs/heads/hardened", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	_ = Command(t.Context(), "-C", dir, "hook", "run", "pre-push").Run()
	if ranOnce(ran) {
		t.Error("ran a hook the repo's config names")
	}
}

// Command's git reaches only ssh, even when the repo's config allows
// another protocol by its own key (protocol.ext.allow) and rewrites the
// VM's URL to it.
func TestCommandSSHOnly(t *testing.T) {
	dir := repo(t)
	_, ran := marker(t)
	git(t, dir, "config", "protocol.ext.allow", "always")
	git(t, dir, "config", "protocol.file.allow", "always")
	git(t, dir, "config", "url.ext::sh -c touch% "+ran+"% #.insteadOf", "vm:")
	git(t, dir, "config", "url.ext::sh -c touch% "+ran+"% #.pushInsteadOf", "vm:")
	for _, args := range [][]string{{"push", "-q", "vm:/x", "HEAD"}, {"fetch", "-q", "vm:/x"}, {"fetch", "-q", t.TempDir()}} {
		out, err := Command(t.Context(), append([]string{"-C", dir}, args...)...).CombinedOutput()
		if ranOnce(ran) {
			t.Errorf("git %v ran the repo's ext:: command", args)
		}
		if err == nil {
			t.Errorf("git %v: reached a protocol that isn't ssh\n%s", args, out)
		}
	}
}

// Command's git fetches nothing for a repo that says it's a partial
// clone, so neither its core.sshCommand nor ssh runs for an object it
// lacks.
func TestCommandNoLazyFetch(t *testing.T) {
	dir := repo(t)
	_, ran := marker(t)
	// An ssh that says it ran and fails, reading nothing.
	prog := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+ran+"'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.partialClone", "origin"},
		{"remote.origin.url", "ssh://example.invalid/r"},
		{"remote.origin.promisor", "true"},
		{"core.sshCommand", prog},
	} {
		git(t, dir, "config", kv[0], kv[1])
	}
	missing := "0123456789abcdef0123456789abcdef01234567"
	_ = stubtest.Git(t.Context(), "-C", dir, "cat-file", "-e", missing).Run()
	if !ranOnce(ran) {
		t.Fatal("plain git didn't fetch lazily with the repo's ssh")
	}
	if err := Command(t.Context(), "-C", dir, "cat-file", "-e", missing).Run(); err == nil {
		t.Error("found a missing object")
	}
	if ranOnce(ran) {
		t.Error("ran the repo's core.sshCommand")
	}
}

// Command's git runs none of the signing programs a repo's config names,
// which a log of a signed commit runs with log.showSignature on.
func TestCommandNoSignaturePrograms(t *testing.T) {
	dir := repo(t)
	prog, ran := marker(t)
	tree := git(t, dir, "rev-parse", "HEAD^{tree}")
	signed := "tree " + tree + "\nauthor t <t@t> 1 +0000\ncommitter t <t@t> 1 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n x\n -----END PGP SIGNATURE-----\n\nsigned\n"
	cmd := stubtest.Git(t.Context(), "-C", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(signed)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(out))
	for _, k := range []string{"gpg.program", "gpg.ssh.program", "gpg.x509.program"} {
		git(t, dir, "config", k, prog)
	}
	git(t, dir, "config", "log.showSignature", "true")
	git(t, dir, "log", "-1", sha)
	if !ranOnce(ran) {
		t.Fatal("plain git didn't run gpg.program")
	}
	if out, err := Output(t.Context(), "-C", dir, "log", "-1", "--format=%s", sha); err != nil || strings.TrimSpace(string(out)) != "signed" {
		t.Fatalf("log %q, %v", out, err)
	}
	if ranOnce(ran) {
		t.Error("ran the repo's gpg.program")
	}
}

// Command's git sees a repo's real objects and history: not a replace
// ref's object in place of the one it replaces, nor the parents a grafts
// file gives a commit.
func TestCommandRealHistory(t *testing.T) {
	dir := repo(t)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "two")
	head := git(t, dir, "rev-parse", "HEAD")
	one := git(t, dir, "rev-parse", "HEAD~")
	git(t, dir, "replace", one, head)
	git(t, dir, "config", "advice.graftFileDeprecated", "false")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "grafts"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "log", "--format=%s", "-1", one); got != "two" {
		t.Fatalf("plain git: replace ref unused: %q", got)
	}
	if got := git(t, dir, "rev-list", "--count", head); got != "1" {
		t.Fatalf("plain git: grafts unused: %q", got)
	}
	if out, _ := Output(t.Context(), "-C", dir, "log", "--format=%s", "-1", one); strings.TrimSpace(string(out)) != "one" {
		t.Errorf("log of the replaced commit: %q", out)
	}
	if out, _ := Output(t.Context(), "-C", dir, "rev-list", "--count", head); strings.TrimSpace(string(out)) != "2" {
		t.Errorf("rev-list --count with grafts: %q", out)
	}
	// And no hint about grafts, in a repo with none either.
	plain := repo(t)
	if out, err := Command(t.Context(), "-C", plain, "log", "--format=%s").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "one" {
		t.Errorf("log: %q, %v", out, err)
	}
}

// Command's git ignores the git variables of devvm's environment, such
// as a GIT_DIR naming another repo.
func TestCommandIgnoresGitEnv(t *testing.T) {
	dir, other := repo(t), repo(t)
	resolved := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	vars := []string{"GIT_DIR=" + filepath.Join(other, ".git"), "GIT_WORK_TREE=" + other}
	plain := stubtest.Git(t.Context(), "-C", dir, "rev-parse", "--absolute-git-dir")
	plain.Env = append(plain.Env, vars...) // stubtest.Git's own environment leaves them out
	if out, err := plain.Output(); err != nil || resolved(strings.TrimSpace(string(out))) != resolved(filepath.Join(other, ".git")) {
		t.Fatalf("plain git: %s, %v", out, err)
	}
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	out, err := Output(t.Context(), "-C", dir, "rev-parse", "--absolute-git-dir")
	if got := strings.TrimSpace(string(out)); err != nil || resolved(got) != resolved(filepath.Join(dir, ".git")) {
		t.Errorf("rev-parse %q, %v", got, err)
	}
	// And relative PATH folders, which git would look in from the repo.
	if env := environ([]string{"GIT_DIR=x", "GIT_CONFIG_COUNT=1", "HOME=/h", "PATH=/p:.:node_modules/.bin::/q"}); !slices.Equal(env, []string{"HOME=/h", "PATH=/p:/q"}) {
		t.Errorf("environ = %q", env)
	}
}

// Host git runs none of the repo's filter drivers on a file whose stat
// changed, whatever its name or kind, a required one included, and finds
// the file unchanged.
func TestNoFilters(t *testing.T) {
	for _, c := range []struct{ name, kind, required string }{
		{"x", "clean", ""}, {"a.b", "process", ""}, {"lfs", "clean", "true"}, {"a=b", "clean", ""}, {"Up", "clean", ""},
	} {
		t.Run(c.name+"."+c.kind, func(t *testing.T) {
			dir := repo(t)
			prog, ran := marker(t)
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("f filter="+c.name+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", ".")
			git(t, dir, "commit", "-q", "-m", "two")
			git(t, dir, "config", "filter."+c.name+"."+c.kind, prog)
			if c.required != "" {
				git(t, dir, "config", "filter."+c.name+".required", c.required)
			}
			touch := func(n int64) {
				at := time.Unix(1_600_000_000+n, 0) // a new mtime: git hashes it again
				if err := os.Chtimes(filepath.Join(dir, "f"), at, at); err != nil {
					t.Fatal(err)
				}
			}
			touch(1)
			_ = stubtest.Git(t.Context(), "-C", dir, "status", "--porcelain").Run()
			if !ranOnce(ran) {
				t.Fatal("plain git status didn't run the filter")
			}
			touch(2)
			// A refresh hashes the file again, as git status does.
			if out, err := Output(t.Context(), "-C", dir, "update-index", "-q", "--refresh"); err != nil || len(out) != 0 {
				t.Errorf("update-index --refresh %q, %v", out, err)
			}
			if ranOnce(ran) {
				t.Error("ran the repo's filter")
			}
		})
	}
}

// DiffFiles doesn't enter a submodule, whose own config names filters of
// its own.
func TestDiffFilesNoSubmodules(t *testing.T) {
	dir, sub := repo(t), repo(t)
	prog, ran := marker(t)
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("f filter=s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "two")
	git(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "s")
	git(t, dir, "commit", "-q", "-m", "sub")
	git(t, filepath.Join(dir, "s"), "config", "filter.s.clean", prog)
	at := time.Unix(1_600_000_001, 0)
	touch := func() {
		if err := os.Chtimes(filepath.Join(dir, "s", "f"), at, at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
	}
	touch()
	_ = stubtest.Git(t.Context(), "-C", dir, "status", "--porcelain").Run()
	if !ranOnce(ran) {
		t.Fatal("plain git status didn't run the submodule's filter")
	}
	touch()
	if out, err := DiffFiles(t.Context(), dir, "--name-only").Output(); err != nil || len(out) != 0 {
		t.Errorf("diff-files %q, %v", out, err)
	}
	if ranOnce(ran) {
		t.Error("ran the submodule's filter")
	}
}

// The user's config files the environment names: only an absolute path to
// a file that's there, else unset; GIT_CONFIG_NOSYSTEM as git reads it.
func TestUserFilesOf(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "gitconfig")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for env, want := range map[[3]string]UserFiles{
		{f, f, "1"}:                        {Global: f, System: f, NoSystem: true},
		{"gitconfig", d, "no"}:             {},
		{filepath.Join(d, "none"), "", ""}: {},
	} {
		got := UserFilesOf(func(k string) string {
			return map[string]string{"GIT_CONFIG_GLOBAL": env[0], "GIT_CONFIG_SYSTEM": env[1], "GIT_CONFIG_NOSYSTEM": env[2]}[k]
		})
		if got != want {
			t.Errorf("%q: %+v, want %+v", env, got, want)
		}
	}
}

// UserConfig reads the global file GIT_CONFIG_GLOBAL names, then the
// system's, which GIT_CONFIG_NOSYSTEM leaves out; else ~/.gitconfig.
func TestUserConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	write := func(name, data string) string {
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(".gitconfig", "[e2e]\n\tk = home\n")
	global := write("global", "[e2e]\n\tk = global\n")
	system := write("system", "[e2e]\n\ts = system\n")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if v, ok := UserConfig(t.Context(), "e2e.k"); !ok || v != "home" {
		t.Errorf("default: %q %v", v, ok)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if v, _ := UserConfig(t.Context(), "e2e.k"); v != "global" {
		t.Errorf("GIT_CONFIG_GLOBAL: %q", v)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	if _, ok := UserConfig(t.Context(), "e2e.s"); ok {
		t.Error("read the system's with GIT_CONFIG_NOSYSTEM")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	if v, _ := UserConfig(t.Context(), "e2e.s"); v != "system" {
		t.Errorf("GIT_CONFIG_SYSTEM: %q", v)
	}
}
