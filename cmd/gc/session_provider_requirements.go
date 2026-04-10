package main

import "strings"

// effectiveSessionProviderForCity returns the effective session backend name
// for a city after applying the GC_SESSION override. When config cannot be
// loaded, it falls back to the default provider contract.
func effectiveSessionProviderForCity(cityPath string) string {
	configured := ""
	if cfg, err := loadCityConfig(cityPath); err == nil {
		configured = cfg.Session.Provider
	}
	return effectiveProviderName(configured)
}

// sessionProviderRequiresTmux reports whether the effective session backend
// depends on a local tmux installation.
func sessionProviderRequiresTmux(name string) bool {
	if strings.HasPrefix(name, "exec:") {
		return false
	}
	switch name {
	case "subprocess", "acp", "k8s", "fake", "fail":
		return false
	case "", "tmux", "hybrid":
		return true
	default:
		// Unknown provider names currently fall back to the tmux adapter.
		return true
	}
}
