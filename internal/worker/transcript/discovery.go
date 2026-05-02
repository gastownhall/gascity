// Package transcript contains worker transcript discovery helpers.
package transcript

import (
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// SupportsIDLookup reports whether the provider family exposes a stable
// transcript identifier that should be preferred over workdir-only discovery.
func SupportsIDLookup(provider string) bool {
	switch providerFamily(provider) {
	case "codex", "gemini", "opencode":
		return false
	default:
		return true
	}
}

// DiscoverPath resolves the best available transcript for a worker session.
func DiscoverPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if path := DiscoverKeyedPath(searchPaths, provider, workDir, gcSessionID); path != "" {
		return path
	}
	if strings.TrimSpace(gcSessionID) != "" && SupportsIDLookup(provider) {
		return ""
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

// DiscoverKeyedPath resolves only the session-id-based transcript path.
func DiscoverKeyedPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) == "" || !SupportsIDLookup(provider) {
		return ""
	}
	return sessionlog.FindSessionFileByID(searchPaths, workDir, gcSessionID)
}

// DiscoverFallbackPath resolves the narrow provider-specific fallback path to
// use when a keyed transcript lookup misses.
func DiscoverFallbackPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) != "" && SupportsIDLookup(provider) {
		return sessionlog.FindProviderFallbackSessionFile(searchPaths, provider, workDir)
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

func providerFamily(provider string) string {
	lower := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case strings.Contains(lower, "codex"):
		return "codex"
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "opencode"):
		return "opencode"
	default:
		return "claude"
	}
}
