// Package transcript contains worker transcript discovery helpers.
package transcript

import (
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// SupportsIDLookup reports whether the provider family exposes a stable
// transcript identifier that should be preferred over workdir-only discovery.
func SupportsIDLookup(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
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
	if sessionlog.ProviderFamily(provider) == "kimi" {
		return sessionlog.FindKimiSessionFileIfUnambiguous(searchPaths, workDir)
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

// DiscoverKeyedPath resolves only the session-id-based transcript path.
func DiscoverKeyedPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) == "" || !SupportsIDLookup(provider) {
		return ""
	}
	switch sessionlog.ProviderFamily(provider) {
	case "kimi":
		return sessionlog.FindKimiSessionFileByID(searchPaths, workDir, gcSessionID)
	case "pi":
		return sessionlog.FindPiSessionFileByID(searchPaths, workDir, gcSessionID)
	case "antigravity":
		return sessionlog.FindAntigravitySessionFileByID(searchPaths, workDir, gcSessionID)
	}
	return sessionlog.FindSessionFileByID(searchPaths, workDir, gcSessionID)
}

// DiscoverFallbackPath resolves the narrow provider-specific fallback path to
// use when a keyed transcript lookup misses.
func DiscoverFallbackPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) != "" && (sessionlog.ProviderFamily(provider) == "pi" || sessionlog.ProviderFamily(provider) == "antigravity") {
		return ""
	}
	if strings.TrimSpace(gcSessionID) != "" && SupportsIDLookup(provider) {
		if sessionlog.ProviderFamily(provider) == "kimi" {
			return ""
		}
		return sessionlog.FindProviderFallbackSessionFile(searchPaths, provider, workDir)
	}
	if sessionlog.ProviderFamily(provider) == "kimi" {
		return sessionlog.FindKimiSessionFileIfUnambiguous(searchPaths, workDir)
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

// HasKeyedTranscript reports whether the per-session ("keyed") transcript that
// a resume would reattach to is present on disk, and whether this provider
// exposes a keyed transcript that can be probed on disk at all.
//
// probeable is true only for provider families that store a transcript keyed by
// the gc session id, so that its absence on disk is a reliable stale-resume
// signal: claude (and claude-eco), kimi, and pi. It is false for providers that
// discover transcripts by cwd/date (codex/gemini/opencode), for unknown or
// custom providers whose layout we cannot assume, and when no session key or
// work dir is supplied — callers should leave such sessions' resume metadata
// untouched rather than guess. When probeable is true, exists reports whether
// the keyed transcript file was found. The per-provider readers reached through
// DiscoverKeyedPath merge their own default roots on top of searchPaths, so
// claude/kimi/pi each probe their real on-disk location even when given only
// the claude default search root.
func HasKeyedTranscript(searchPaths []string, provider, workDir, sessionKey string) (exists, probeable bool) {
	if strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(workDir) == "" || !providerHasKeyedTranscript(provider) {
		return false, false
	}
	return DiscoverKeyedPath(searchPaths, provider, workDir, sessionKey) != "", true
}

// providerHasKeyedTranscript reports whether the provider family persists a
// per-session transcript keyed by the gc session id. This is stricter than
// SupportsIDLookup (which treats any non-codex/gemini/opencode provider as
// id-capable for discovery-strategy purposes): here we only claim a provider
// when we actually know its on-disk keyed layout, so the stale-resume guard
// never clears a resume key for a provider whose transcript we cannot verify.
func providerHasKeyedTranscript(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
	case "kimi", "pi":
		return true
	}
	// claude and claude-eco fall through ProviderFamily unchanged; match them
	// by name since both store keyed JSONL under ~/.claude/projects.
	return strings.Contains(strings.ToLower(strings.TrimSpace(provider)), "claude")
}
