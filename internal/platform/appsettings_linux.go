package platform

// AppSettingsDir is the folder where desktop apps keep their settings,
// each in a folder of its own: on Linux, configHome, the XDG config folder
// ($XDG_CONFIG_HOME, by default ~/.config), as Electron apps such as
// Cursor find it.
func AppSettingsDir(_, configHome string) string {
	return configHome
}
