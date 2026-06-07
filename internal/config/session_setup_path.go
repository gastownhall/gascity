package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveSessionSetupScriptPath resolves a session_setup_script path for
// runtime and validation. Paths prefixed with "//" resolve against cityPath.
// Relative paths resolve against sourceDir when present, with legacy
// city-root-relative strings still supported during the transition.
func ResolveSessionSetupScriptPath(cityPath, sourceDir, script string) string {
	if strings.HasPrefix(script, "//") {
		return filepath.Join(cityPath, strings.TrimPrefix(script, "//"))
	}
	if script == "" || filepath.IsAbs(script) {
		return script
	}
	if sourceDir != "" {
		relSource, err := filepath.Rel(cityPath, sourceDir)
		if err == nil {
			relSource = filepath.Clean(relSource)
			cleanScript := filepath.Clean(script)
			if relSource != "." && relSource != "" && !strings.HasPrefix(relSource, "..") &&
				(cleanScript == relSource || strings.HasPrefix(cleanScript, relSource+string(os.PathSeparator))) {
				return filepath.Join(cityPath, cleanScript)
			}
		}

		sourceCandidate := filepath.Join(sourceDir, script)
		cityCandidate := filepath.Join(cityPath, filepath.Clean(script))
		if sessionSetupScriptPathExists(cityCandidate) && !sessionSetupScriptPathExists(sourceCandidate) {
			return cityCandidate
		}
		return sourceCandidate
	}
	return filepath.Join(cityPath, script)
}

func sessionSetupScriptPathExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
