//go:build !windows

package overlay

// AdaptSettingsForPlatform returns data unchanged: the bundled provider
// settings and hook files are written for this platform as they ship.
func AdaptSettingsForPlatform(_ string, data []byte) []byte { return data }

func platformAdaptsPath(_ string) bool {
	return false
}

func platformAdaptsDirectoryCopies() bool {
	return false
}

func platformPreserveExistingPath(relPath string) bool {
	return relPath == "AGENTS.md"
}
