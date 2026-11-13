package settings

import (
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// KeyFollowLinks is config.toml's follow-links, the folders whose files
// devvm agent import copies through a link out of an agent's folder,
// without asking.
const KeyFollowLinks = "follow-links"

// FollowLinks is config.toml's follow-links, each folder as an absolute
// path, ~/ taken as d's home folder; nil when it sets none. No command
// saves it. Whether devvm may read a folder, and where its links lead,
// is devvm agent import's to check, as it resolves them.
func FollowLinks(d paths.Dirs) ([]string, error) {
	cf, err := decodeConfig(d.ConfigFile())
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, s := range cf.FollowLinks.v {
		if rest, ok := strings.CutPrefix(s, "~/"); ok {
			s = filepath.Join(d.Home, filepath.FromSlash(rest))
		}
		dirs = append(dirs, filepath.FromSlash(s))
	}
	return dirs, nil
}

type followLinksKind struct{}

func (followLinksKind) parse(v any) (string, error) { return checked(text(v))(checkFollowLinks) }

// checkFollowLinks refuses a folder that isn't an absolute path or one
// below ~/, as clean as path.Clean leaves it (no trailing slash, no ..),
// that is / or ~ itself, or that has a control character.
func checkFollowLinks(s string) error {
	rest, home := strings.CutPrefix(s, "~/")
	if !home {
		rest = strings.TrimPrefix(s, "/")
	}
	switch {
	case !home && !strings.HasPrefix(s, "/"),
		rest == "" || rest == "." || rest != path.Clean(rest) || strings.HasPrefix(rest, "../") || rest == "..",
		strings.ContainsFunc(s, unicode.IsControl): //nolint:forbidigo // a path in config.toml, not the terminal
		return msg.FollowLinksInvalid(s, KeyFollowLinks)
	}
	return nil
}
