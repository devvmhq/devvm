//go:build !darwin && !linux

package platform

// AppSettingsDir is the folder where desktop apps keep their settings,
// each in a folder of its own: configHome, the XDG config folder, as on
// Linux.
func AppSettingsDir(_, configHome string) string {
	return configHome
}
