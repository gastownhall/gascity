package api

import (
	"path/filepath"
	"strings"
)

func resolveScopeRoot(cityPath, scopePath string) string {
	scopePath = strings.TrimSpace(scopePath)
	cityPath = strings.TrimSpace(cityPath)
	if scopePath == "" {
		scopePath = cityPath
	}
	if !filepath.IsAbs(scopePath) && cityPath != "" {
		scopePath = filepath.Join(cityPath, scopePath)
	}
	return filepath.Clean(scopePath)
}
