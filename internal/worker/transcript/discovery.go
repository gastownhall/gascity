// Package transcript contains worker transcript discovery helpers.
package transcript

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// SupportsIDLookup reports whether the provider family exposes a stable
// transcript identifier that should be preferred over workdir-only discovery.
func SupportsIDLookup(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
	case "codex", "gemini", "opencode", "mimocode":
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
	family := sessionlog.ProviderFamily(provider)
	if strings.TrimSpace(gcSessionID) != "" && SupportsIDLookup(provider) && (family != "antigravity" || !isProvisionalGCSessionID(gcSessionID)) {
		return ""
	}
	if family == "kimi" {
		return sessionlog.FindKimiSessionFileIfUnambiguous(searchPaths, workDir)
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

// DiscoverKeyedPath resolves only the session-id-based transcript path.
func DiscoverKeyedPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	if strings.TrimSpace(gcSessionID) == "" {
		return ""
	}
	switch sessionlog.ProviderFamily(provider) {
	case "auggie":
		return sessionlog.FindAuggieSessionFileByID(searchPaths, workDir, gcSessionID)
	case "amp":
		return sessionlog.FindAmpSessionFileByID(searchPaths, workDir, gcSessionID)
	case "copilot":
		return sessionlog.FindCopilotSessionFileByID(searchPaths, workDir, gcSessionID)
	case "codex":
		return sessionlog.FindCodexSessionFileByIDNoWindow(searchPaths, workDir, gcSessionID)
	case "cursor":
		return sessionlog.FindCursorSessionFileByID(searchPaths, workDir, gcSessionID)
	case "grok":
		return sessionlog.FindGrokSessionFileByID(searchPaths, workDir, gcSessionID)
	case "kiro":
		return sessionlog.FindKiroSessionFileByID(searchPaths, workDir, gcSessionID)
	case "gemini":
		return sessionlog.FindGeminiSessionFileByID(searchPaths, workDir, gcSessionID)
	case "kimi":
		return sessionlog.FindKimiSessionFileByID(searchPaths, workDir, gcSessionID)
	case "pi":
		return sessionlog.FindPiSessionFileByID(searchPaths, workDir, gcSessionID)
	case "antigravity":
		return sessionlog.FindAntigravitySessionFileByID(searchPaths, workDir, gcSessionID)
	}
	if !SupportsIDLookup(provider) {
		return ""
	}
	return sessionlog.FindSessionFileByID(searchPaths, workDir, gcSessionID)
}

// DiscoverCodexPathInTimeWindow resolves a Codex transcript whose metadata
// timestamp uniquely matches the supplied session-start window.
func DiscoverCodexPathInTimeWindow(searchPaths []string, workDir string, start, end time.Time) string {
	return sessionlog.FindCodexSessionFileInTimeWindow(searchPaths, workDir, start, end)
}

// DiscoverFallbackPath resolves the narrow provider-specific fallback path to
// use when a keyed transcript lookup misses.
func DiscoverFallbackPath(searchPaths []string, provider, workDir, gcSessionID string) string {
	sessionID := strings.TrimSpace(gcSessionID)
	family := sessionlog.ProviderFamily(provider)
	if sessionID != "" && family == "pi" {
		return ""
	}
	if sessionID != "" && family == "codex" {
		return ""
	}
	if sessionID != "" && family == "antigravity" && !isProvisionalGCSessionID(sessionID) {
		return ""
	}
	if sessionID != "" && family == "amp" {
		return ""
	}
	if sessionID != "" && family == "auggie" {
		return ""
	}
	if sessionID != "" && family == "grok" {
		return ""
	}
	if sessionID != "" && family == "cursor" {
		return ""
	}
	if sessionID != "" && SupportsIDLookup(provider) {
		if family == "kimi" {
			return ""
		}
		return sessionlog.FindProviderFallbackSessionFile(searchPaths, provider, workDir)
	}
	if family == "kimi" {
		return sessionlog.FindKimiSessionFileIfUnambiguous(searchPaths, workDir)
	}
	return sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
}

func isProvisionalGCSessionID(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if !strings.HasPrefix(sessionID, "gc-") || len(sessionID) <= len("gc-") {
		return false
	}
	for _, r := range sessionID[len("gc-"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// HasKeyedTranscript reports whether the per-session ("keyed") transcript that
// a resume would reattach to is present on disk, and whether this provider
// exposes a keyed transcript that can be probed on disk at all.
//
// probeable is true only for provider families where a missing keyed transcript
// is a reliable stale-resume signal. Some providers, including Codex, support
// keyed transcript discovery for display but are intentionally excluded here
// because absence on disk is not yet used as a resume-key invalidation signal
// for that provider. Unknown or custom providers whose layout we cannot assume,
// and calls missing a session key or work dir, are also not probeable; callers
// should leave such sessions' resume metadata untouched rather than guess. When
// probeable is true, exists reports whether the keyed transcript file was
// found. The per-provider readers reached through DiscoverKeyedPath merge their
// own default roots on top of searchPaths, so known probeable providers each
// probe their real on-disk location even when given only a partial configured
// search root.
func HasKeyedTranscript(searchPaths []string, provider, workDir, sessionKey string) (exists, probeable bool) {
	if strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(workDir) == "" || !providerHasKeyedTranscript(provider) {
		return false, false
	}
	return DiscoverKeyedPath(searchPaths, provider, workDir, sessionKey) != "", true
}

// providerHasKeyedTranscript reports whether the provider family can use keyed
// transcript absence as a stale-resume signal. This is stricter than
// SupportsIDLookup, which only answers whether transcript display lookup can
// use a provider session id. Here we only claim a provider when its keyed
// transcript absence is meaningful for resume-key invalidation, so the
// stale-resume guard never clears a resume key for a provider whose restart
// semantics we have not verified.
func providerHasKeyedTranscript(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
	case "copilot", "kiro", "kimi", "pi", "antigravity":
		return true
	}
	// claude and claude-eco fall through ProviderFamily unchanged; match them
	// by name since both store keyed JSONL under ~/.claude/projects.
	return strings.Contains(strings.ToLower(strings.TrimSpace(provider)), "claude")
}
