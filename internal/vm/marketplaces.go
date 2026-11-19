package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/safefile"
)

// A Claude Code marketplace is a git clone, which devvm agent import and save copy whole into
// the store's plugins folder, its git config included, and every VM with Claude Code mounts that
// folder. A clone of a private marketplace can keep a credential there, in a URL's user info or
// an http.extraHeader, and known_marketplaces.json can keep one in a marketplace's source URL,
// so devvm warns of each one it copies that has one, naming it, and copies it as it is.

// marketplacesDir is where Claude Code keeps its marketplaces' clones, relative to the home
// folder or the agent's store, and marketplacesFile its record of them.
const (
	marketplacesDir  = agents.ClaudePlugins + "/marketplaces"
	marketplacesFile = agents.ClaudePlugins + "/known_marketplaces.json"
)

// includeMax is how many included files deep the check follows a clone's config.
const includeMax = 10

// warnMarketplaceSecrets warns of each credential among the files written, by their paths
// relative to the Claude store, store: in the git config of a marketplace's clone, its own or a
// submodule's (marketplace_secret), and in known_marketplaces.json's source URLs. A config git
// can't read is named too (marketplace_unchecked), as the VM reads the file, not git.
func warnMarketplaceSecrets(ctx context.Context, r msg.Reporter, store string, written []string) {
	for _, rel := range written {
		p := filepath.Join(store, filepath.FromSlash(rel))
		if rel == marketplacesFile {
			for _, s := range knownSecrets(p) {
				r.Warn(msg.MarketplaceSecret(p, s.name, s.why))
			}
			continue
		}
		name, ok := cloneConfig(rel)
		if !ok {
			continue
		}
		why, err := gitConfigSecret(ctx, filepath.Join(store, marketplacesDir, name), p, 0)
		switch {
		case err != nil:
			r.Warn(msg.MarketplaceUnchecked(p, name, gitReason(err)).Because(err))
		case why != "":
			r.Warn(msg.MarketplaceSecret(p, name, why))
		}
	}
}

// gitReason is why git failed, as it said on its stderr, its first line, or err itself when it
// said nothing.
func gitReason(err error) string {
	if e, ok := errors.AsType[*exec.ExitError](err); ok {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(e.Stderr)), "\n"); line != "" {
			return msg.OneLine(line)
		}
	}
	return err.Error()
}

// cloneConfig is the marketplace whose clone has the git config at rel, relative to the store:
// a file named config or config.worktree anywhere below its .git folder, a submodule's
// (.git/modules/NAME/config) included.
func cloneConfig(rel string) (string, bool) {
	rest, ok := strings.CutPrefix(rel, marketplacesDir+"/")
	name, inner, _ := strings.Cut(rest, "/")
	base := path.Base(inner)
	return name, ok && name != "" && strings.HasPrefix(inner, ".git/") && (base == "config" || base == "config.worktree")
}

// gitConfigSecret is the keys of the git config file p that hold a credential, as git itself
// reads it (listConfigFile), comma-separated: an http.extraHeader, for any URL, and a URL with a
// credential (urlSecret) in a remote's or a submodule's URL, a push URL, a proxy or the base
// of an insteadOf. It follows each include inside the clone, clone, below depth; an include
// outside it isn't copied. A key whose subsection is a URL with a credential is named with
// <url> in its place, so the warning never shows it.
func gitConfigSecret(ctx context.Context, clone, p string, depth int) (string, error) {
	recs, err := listConfigFile(ctx, p)
	if err != nil {
		return "", err
	}
	var why []string
	for _, rec := range recs {
		section, sub, name, ok := configKey(rec.key)
		if !ok {
			continue
		}
		shown := rec.key
		if sub != "" && urlSecret(sub) {
			shown = section + ".<url>." + name
		}
		switch {
		case section == "include" && name == "path", section == "includeif" && name == "path":
			more, err := includedSecret(ctx, clone, p, rec.value, depth)
			if err != nil {
				return "", err
			}
			why = append(why, more...)
		case name == "extraheader" && section == "http",
			urlKey(section, name) && urlSecret(rec.value),
			(section == "url" && (name == "insteadof" || name == "pushinsteadof")) && urlSecret(sub):
			why = append(why, shown)
		}
	}
	slices.Sort(why)
	return strings.Join(slices.Compact(why), ", "), nil
}

// includedSecret is the keys with a credential in the file that an include of the config at p
// names, value, when it's inside the clone, which the store copied too; none for one outside.
func includedSecret(ctx context.Context, clone, p, value string, depth int) ([]string, error) {
	inc := value
	if strings.HasPrefix(inc, "~") || strings.HasPrefix(inc, "%(prefix)/") {
		return nil, nil // in a home folder or git's install, never in the clone, so not copied
	}
	if !filepath.IsAbs(inc) {
		inc = filepath.Join(filepath.Dir(p), inc)
	}
	rel, err := filepath.Rel(clone, inc)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(rel) || depth >= includeMax {
		return nil, nil // outside the clone, so not copied, or past the depth git itself refuses
	}
	if _, err := os.Lstat(inc); errors.Is(err, fs.ErrNotExist) {
		return nil, nil // git skips an include that isn't there, and so nothing of it is read
	}
	why, err := gitConfigSecret(ctx, clone, inc, depth+1)
	if why == "" || err != nil {
		return nil, err
	}
	return strings.Split(why, ", "), nil
}

// urlKey reports whether a config key's value is a URL that git sends: a remote's URL, push URL
// or proxy, a submodule's URL, or an http proxy.
func urlKey(section, name string) bool {
	switch section {
	case "remote":
		return name == "url" || name == "pushurl" || name == "proxy"
	case "submodule":
		return name == "url"
	case "http":
		return name == "proxy"
	}
	return false
}

// urlSecret reports whether u holds a credential in its user info, as the clone's seed judges a
// remote (cleanURL): it has an @ and cleanURL changes it, as it takes out an http(s) URL's user
// and an ssh URL's password, or refuses it, as it does a transport helper's and a URL it can't
// parse. An scp-like git@host:path, whose user is a name, keeps it.
func urlSecret(u string) bool {
	if !strings.Contains(u, "@") {
		return false
	}
	clean, ok := cleanURL(u)
	return !ok || clean != u
}

// knownSecret is a marketplace in known_marketplaces.json whose source URL holds a credential.
type knownSecret struct{ name, why string }

// knownSecrets are the marketplaces of known_marketplaces.json at p whose source holds a URL
// with a credential (urlSecret), each naming the field; none when it isn't JSON, which the copy
// keeps as it is.
func knownSecrets(p string) []knownSecret {
	data, err := safefile.Read(p, saveMaxFile, safefile.Opts{})
	var known map[string]struct {
		Source map[string]any `json:"source"`
	}
	if err != nil || json.Unmarshal(data, &known) != nil {
		return nil // not one devvm reads: Claude Code would refuse it too
	}
	var out []knownSecret
	for name, m := range known {
		for field, v := range m.Source {
			if s, ok := v.(string); ok && urlSecret(s) {
				out = append(out, knownSecret{name, "source." + field})
			}
		}
	}
	slices.SortFunc(out, func(a, b knownSecret) int { return strings.Compare(a.name+a.why, b.name+b.why) })
	return out
}

// marketplaceFiles are the paths of h's files in the Claude store, relative to it.
func marketplaceFiles(folder string, files []string) []string {
	var out []string
	for _, f := range files {
		out = append(out, path.Join(folder, f))
	}
	return out
}
