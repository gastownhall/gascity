package cityinit

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	// BootstrapProfileK8sCell applies hosted/container-friendly API defaults.
	BootstrapProfileK8sCell = "k8s-cell"

	// BootstrapProfileSingleHostCompat preserves local single-host defaults.
	BootstrapProfileSingleHostCompat = "single-host-compat"
)

// NormalizeBootstrapProfile returns the canonical bootstrap profile name.
func NormalizeBootstrapProfile(profile string) (string, error) {
	switch strings.TrimSpace(profile) {
	case "":
		return "", nil
	case BootstrapProfileK8sCell, "kubernetes", "kubernetes-cell":
		return BootstrapProfileK8sCell, nil
	case BootstrapProfileSingleHostCompat:
		return BootstrapProfileSingleHostCompat, nil
	default:
		return "", fmt.Errorf("unknown bootstrap profile %q", profile)
	}
}

// IsBuiltinProvider reports whether provider names one of Gas City's built-in
// provider presets.
func IsBuiltinProvider(provider string) bool {
	_, ok := config.BuiltinProviders()[provider]
	return ok
}

// ResolveCityName returns the workspace name to use during init.
func ResolveCityName(nameOverride, sourceName, cityPath string) string {
	if n := strings.TrimSpace(nameOverride); n != "" {
		return n
	}
	if n := strings.TrimSpace(sourceName); n != "" {
		return n
	}
	return strings.TrimSpace(filepath.Base(cityPath))
}
