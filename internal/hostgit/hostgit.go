// Package hostgit runs git on the host for devvm, hardened against what
// a project folder's repo can make it do. A VM can write the repo's .git
// when it mounts the folder, or a nested repo through a parent folder's
// mount, and a repo can come from anywhere, so every git command devvm
// runs on the host goes through Command. Its git runs none of the
// programs a repo's config names that devvm's commands would reach: no
// fsmonitor (core.fsmonitor), no hooks (core.hooksPath, and the hooks a
// config names, hook.*), no filters (filter.*, for git status), no
// alternates' refs command, and over no transport but ssh
// (protocol.allow). It ignores the git variables of the environment
// devvm runs in (GIT_DIR, GIT_CONFIG_* and the rest), which would point
// it at another repo or config, finds no program in a relative PATH
// folder, never asks on the terminal (GIT_TERMINAL_PROMPT=0), fetches
// no object lazily and reaches no host over ssh unless its caller names
// the ssh (GIT_SSH_COMMAND), starts no gc or maintenance, runs no
// signing program (gpg.*program), and sees no replace refs or grafts.
package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// HookEvents are the events git runs hooks for (githooks(5)), each turned
// off for the hooks a config names for it (hook.EVENT.enabled), and the
// names git runs in a hooks folder.
func HookEvents() []string {
	return []string{
		"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
		"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
		"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
		"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
		"fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist", "p4-pre-submit",
		"post-index-change",
	}
}

// hardened is what every host git command runs with, as config pairs: no
// signature is checked or made, so no log or reflog command runs the
// signing program a repo's config names (gpg.*program), even with
// log.showSignature on.
func hardened() [][2]string {
	kv := [][2]string{
		{"core.fsmonitor", "false"},
		{"core.hooksPath", os.DevNull},
		{"core.alternateRefsCommand", "true"},
		{"protocol.allow", "never"},
		{"protocol.ssh.allow", "always"},
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
		{"log.showSignature", "false"},
		{"gpg.program", "false"},
		{"gpg.ssh.program", "false"},
		{"gpg.x509.program", "false"},
		// command's GIT_GRAFT_FILE would make git warn on every walk.
		{"advice.graftFileDeprecated", "false"},
	}
	for _, h := range HookEvents() {
		kv = append(kv, [2]string{"hook." + h + ".enabled", "false"})
	}
	return kv
}

// Command returns git with args, to run on the host, hardened. When args
// start with -C DIR, the hooks and filter drivers DIR's repo config
// names are turned off too. A caller that adds to its environment
// appends to cmd.Env.
func Command(ctx context.Context, args ...string) *exec.Cmd {
	kv := hardened()
	if len(args) > 1 && args[0] == "-C" {
		kv = append(kv, repoOff(ctx, args[1])...)
	}
	return command(ctx, kv, args)
}

// Output runs git with args on the host, hardened, and returns its
// standard output.
func Output(ctx context.Context, args ...string) ([]byte, error) {
	return Command(ctx, args...).Output()
}

// DiffFiles returns git diff-files in dir with args, hardened, taking no
// optional locks (so it writes no index, and no post-index-change hook
// has anything to run for) and entering no submodule or embedded repo,
// whose own config could name filters that git would run there.
func DiffFiles(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return Command(ctx, append([]string{"-C", dir, "--no-optional-locks", "diff-files", "--ignore-submodules=all"}, args...)...)
}

// command is git with args and the config pairs kv, given in the
// environment (GIT_CONFIG_COUNT), where a key holds any character. It
// reaches ssh alone (GIT_ALLOW_PROTOCOL), whatever protocol.NAME.allow a
// repo's config sets, which would beat protocol.allow. It fetches no
// missing object of a repo that says it's a partial clone
// (GIT_NO_LAZY_FETCH), and its ssh is false, which reaches nothing, in
// place of a repo's core.sshCommand, unless the caller sets
// GIT_SSH_COMMAND after it; that ssh is ssh's own (GIT_SSH_VARIANT),
// whatever the repo's ssh.variant says. It sees a repo's objects as they
// are, not as its replace refs (GIT_NO_REPLACE_OBJECTS) or a grafts file
// (GIT_GRAFT_FILE) would show them, so what it walks, packs or looks up
// is the repo's real history.
func command(ctx context.Context, kv [][2]string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // devvm's own arguments
	env := append(environ(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=ssh",
		"GIT_SSH_COMMAND=false", "GIT_SSH_VARIANT=ssh", "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE="+os.DevNull,
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(kv)))
	for i, p := range kv {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}
	cmd.Env = env
	return cmd
}

// repoOff is the config pairs that turn off every hook and filter driver
// dir's repo config names: hook.NAME.enabled, and filter.NAME's clean,
// smudge and process emptied and required false, as git status would run
// a filter on a file whose stat changed. One it can't list is none git
// sees either.
func repoOff(ctx context.Context, dir string) [][2]string {
	out, _ := command(ctx, hardened(), []string{"-C", dir, "config", "--name-only", "--get-regexp", `^(filter|hook)\.`}).Output()
	var kv [][2]string
	seen := map[string]bool{}
	for _, key := range strings.Split(string(out), "\n") {
		section, name, ok := split(key)
		if !ok || seen[section+"."+name] {
			continue
		}
		seen[section+"."+name] = true
		switch section {
		case "hook":
			kv = append(kv, [2]string{"hook." + name + ".enabled", "false"})
		case "filter":
			for _, k := range []string{"clean", "smudge", "process"} {
				kv = append(kv, [2]string{"filter." + name + "." + k, ""})
			}
			kv = append(kv, [2]string{"filter." + name + ".required", "false"})
		}
	}
	return kv
}

// split splits a SECTION.NAME.KEY key into its section and NAME, which
// may hold dots.
func split(key string) (section, name string, ok bool) {
	section, rest, found := strings.Cut(key, ".")
	i := strings.LastIndex(rest, ".")
	if !found || i <= 0 {
		return "", "", false
	}
	return section, rest[:i], true
}

// UserFiles are the user's own config files that git reads when the
// environment devvm runs in names them, which Command's git ignores with
// the rest of GIT_*: GIT_CONFIG_GLOBAL's in place of ~/.gitconfig, and
// GIT_CONFIG_SYSTEM's in place of the system's, or no system config
// (nosystem) with GIT_CONFIG_NOSYSTEM. A path that isn't absolute, or a
// file that isn't there, counts as unset ("").
type UserFiles struct {
	Global, System string
	NoSystem       bool
}

// UserFilesOf is the user's config files that getenv (os.Getenv) names.
func UserFilesOf(getenv func(string) string) UserFiles {
	file := func(name string) string {
		p := getenv(name)
		if fi, err := os.Stat(p); p == "" || !filepath.IsAbs(p) || err != nil || !fi.Mode().IsRegular() {
			return ""
		}
		return p
	}
	no, _ := strconv.ParseBool(getenv("GIT_CONFIG_NOSYSTEM"))
	return UserFiles{Global: file("GIT_CONFIG_GLOBAL"), System: file("GIT_CONFIG_SYSTEM"), NoSystem: no}
}

// UserConfig returns key's value in the user's global config, else in the
// system's, as git reads them for the user (UserFilesOf the environment):
// a file the environment names is read by name (--file), so no other
// GIT_* reaches git; and whether either has it. It reads only.
func UserConfig(ctx context.Context, key string) (string, bool) {
	f := UserFilesOf(os.Getenv)
	global, system := []string{"--global"}, []string{"--system"}
	if f.Global != "" {
		global = []string{"--file", f.Global}
	}
	if f.System != "" {
		system = []string{"--file", f.System}
	}
	scopes := [][]string{global}
	if !f.NoSystem {
		scopes = append(scopes, system)
	}
	for _, s := range scopes {
		out, err := Output(ctx, append(append([]string{"config"}, s...), "--get", key)...)
		if err == nil {
			return strings.TrimRight(string(out), "\n"), true
		}
	}
	return "", false
}

// environ is env without git's own variables (GIT_*), which would point
// git at another repo, worktree, index, object store or config, or at
// other programs, whatever devvm runs in (a hook, a rebase's exec); and
// with only absolute folders in PATH, as git looks programs up (ssh among
// them) from the repo's folder.
func environ(env []string) []string {
	var out []string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_"):
		case strings.HasPrefix(kv, "PATH="):
			var abs []string
			for _, d := range filepath.SplitList(strings.TrimPrefix(kv, "PATH=")) {
				if filepath.IsAbs(d) {
					abs = append(abs, d)
				}
			}
			out = append(out, "PATH="+strings.Join(abs, string(os.PathListSeparator)))
		default:
			out = append(out, kv)
		}
	}
	return out
}
