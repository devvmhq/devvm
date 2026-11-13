package platform

import "path/filepath"

// AppSettingsDir is the folder in home where desktop apps keep their
// settings, each in a folder of its own: macOS's Application Support,
// whatever the XDG config folder (configHome) is.
func AppSettingsDir(home, _ string) string {
	return filepath.Join(home, "Library", "Application Support")
}
