package gchome

import (
	"os"
	"path/filepath"
	"strings"
)

// Default returns the Gas City machine-local state directory.
//
// Resolution order: GC_HOME, user home/.gc, temp fallback.
func Default() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

func RegistriesPath(home string) string {
	return filepath.Join(home, "registries.toml")
}

func RegistryCacheRoot(home string) string {
	return filepath.Join(home, "registry-cache")
}
