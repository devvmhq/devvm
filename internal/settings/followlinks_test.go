package settings

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// config.toml's follow-links: folders as absolute paths or below ~/,
// each as clean as path.Clean leaves it, ~/ read as the home folder;
// anything else, / and ~ themselves, and a repeat are refused at their
// line; none is none.
func TestFollowLinks(t *testing.T) {
	d := testDirs(t)
	d.Home = "/home/me"
	if dirs, err := FollowLinks(d); err != nil || dirs != nil {
		t.Fatalf("no config.toml: %q, %v", dirs, err)
	}
	write(t, d.ConfigFile(), `follow-links = ["~/dotfiles", "/opt/dots", "~/a b/c.d"]`+"\n")
	want := []string{filepath.Join(d.Home, "dotfiles"), "/opt/dots", filepath.Join(d.Home, "a b/c.d")}
	if dirs, err := FollowLinks(d); err != nil || !slices.Equal(dirs, want) {
		t.Errorf("FollowLinks = %q, %v, want %q", dirs, err, want)
	}
	for _, bad := range []string{
		"", "/", "~", "~/", "dotfiles", "./dotfiles", "~user/dots", "/opt/dots/", "/opt//dots", "/opt/./dots",
		"/opt/../dots", "~/..", "~/../x", "~/a/../b", "/opt/do\nts",
	} {
		write(t, d.ConfigFile(), "# the folders\nfollow-links = ["+strconv.Quote(bad)+"]\n")
		_, err := FollowLinks(d)
		c, ok := errors.AsType[*state.CorruptError](err)
		if !ok || c.Msg.Names() != msg.FollowLinksInvalid(bad, KeyFollowLinks).Names() || c.Line != 2 {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, bad := range []string{`follow-links = "~/dotfiles"`, `follow-links = ["~/a", "~/a"]`, `follow-links = [3]`} {
		write(t, d.ConfigFile(), bad+"\n")
		if _, err := FollowLinks(d); err == nil {
			t.Errorf("%s: read", bad)
		}
	}
}
