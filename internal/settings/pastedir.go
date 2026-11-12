package settings

import (
	"path"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// KeyPasteDir is config.toml's paste-dir, the folder in every VM that
// devvm paste puts images and files in.
const KeyPasteDir = "paste-dir"

// PasteDirDefault is the folder devvm paste uses when config.toml sets
// none: a fixed path, so you can allow it once in an agent's settings.
const PasteDirDefault = "/tmp/devvm-paste"

// PasteDir is config.toml's paste-dir, PasteDirDefault when it sets none.
// Each launch makes the folder in the VM, and devvm paste writes there.
func PasteDir(d paths.Dirs) (string, error) {
	cf, err := decodeConfig(d.ConfigFile())
	if err != nil {
		return "", err
	}
	if !cf.PasteDir.set {
		return PasteDirDefault, nil
	}
	return cf.PasteDir.v, nil
}

type pasteDirKind struct{}

func (pasteDirKind) parse(v any) (string, error) { return checked(text(v))(checkPasteDir) }

// checkPasteDir refuses a folder that isn't an absolute path as clean as
// path.Clean leaves it, that is /, or that holds a character outside
// [A-Za-z0-9._/-]: the path goes into a shell in the VM, into the setup's
// vm file and onto the clipboard, where an agent reads it as a path.
func checkPasteDir(s string) error {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)
	}
	if !strings.HasPrefix(s, "/") || s == "/" || s != path.Clean(s) || strings.ContainsFunc(s, func(r rune) bool { return !safe(r) }) {
		return msg.PasteDirInvalid(KeyPasteDir)
	}
	return nil
}
