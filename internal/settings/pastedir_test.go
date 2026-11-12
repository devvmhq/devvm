package settings

import (
	"errors"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// config.toml's paste-dir: an absolute, clean path of letters, digits,
// dots, dashes and underscores, else refused with paste_dir_invalid at
// its line; none is the default folder.
func TestPasteDir(t *testing.T) {
	d := testDirs(t)
	if p, err := PasteDir(d); err != nil || p != PasteDirDefault {
		t.Fatalf("no config.toml: %q, %v", p, err)
	}
	for _, good := range []string{"/tmp/devvm-paste", "/home/me/pastes", "/var/tmp/a.b-c_d", "/p"} {
		write(t, d.ConfigFile(), "paste-dir = \""+good+"\"\n")
		if p, err := PasteDir(d); err != nil || p != good {
			t.Errorf("%s: %q, %v", good, p, err)
		}
	}
	for _, bad := range []string{
		"", "/", "tmp/pastes", "~/pastes", "/tmp/pastes/", "/tmp//pastes", "/tmp/./pastes", "/tmp/../pastes",
		"/tmp/my pastes", "/tmp/paste$", "/tmp/p;rm", "/tmp/pâte", "/tmp/p\tq", "/tmp/'p'",
	} {
		write(t, d.ConfigFile(), "# the folder\npaste-dir = \""+bad+"\"\n")
		_, err := PasteDir(d)
		var c *state.CorruptError
		if !errors.As(err, &c) || c.Msg.Names() != msg.PasteDirInvalid(KeyPasteDir).Names() || c.Line != 2 {
			t.Errorf("%q: %v", bad, err)
		}
	}
	write(t, d.ConfigFile(), "paste-dir = 3\n")
	if _, err := PasteDir(d); err == nil {
		t.Error("a number: read")
	}
}
