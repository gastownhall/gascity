// Package transcript contains worker transcript discovery helpers.
package transcript

import (
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// SupportsIDLookup reports whether the provider family exposes a stable
// transcript identifier that should be preferred over workdir-only discovery.
func SupportsIDLookup(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
	case "codex", "gemini", "opencode", "mimocode", "zcode":
		return false
	default:
		return true
	}
}

// keyedFinders are the family-specific by-id lookups DiscoverKeyedPath
// resolves a session key with. HasKeyedLookup reads the same table, so the
// two cannot disagree about a family.
var keyedFinders = map[string]func(searchPaths []string, workDir, sessionID string) string{
	"auggie":      sessionlog.FindAuggieSessionFileByID,
	"amp":         sessionlog.FindAmpSessionFileByID,
	"copilot":     sessionlog.FindCopilotSessionFileByID,
	"codex":       sessionlog.FindCodexSessionFileByIDNoWindow,
	"cursor":      sessionlog.FindCursorSessionFileByID,
	"grok":        sessionlog.FindGrokSessionFileByID,
	"kiro":        sessionlog.FindKiroSessionFileByID,
	"gemini":      sessionlog.FindGeminiSessionFileByID,
	"kimi":        sessionlog.FindKimiSessionFileByID,
	"pi":          sessionlog.FindPiSessionFileByID,
	"antigravity": sessionlog.FindAntigravitySessionFileByID,
	// The zcode mirror filename IS the provider session id, so an identity
	// lookup is exact. It is not keyed by gc's session id, which is why
	// SupportsIDLookup stays false: without an identity the family still
	// discovers by cwd. Preferring the keyed hit removes the newest-wins
	// ambiguity when several sessions share a work dir.
	"zcode": sessionlog.FindZCodeSessionFileByID,
	// The OpenCode mirror filename IS the provider session id (the bead's
	// session_key), so the identity lookup is exact. SupportsIDLookup stays
	// false for the same reason as zcode. Without the keyed hit, sessions
	// sharing a work dir are indistinguishable and usage sweeps refuse them
	// as ambiguous.
	"opencode": func(searchPaths []string, _, sessionID string) string {
		return sessionlog.FindOpenCodeSessionFileByID(searchPaths, sessionID)
	},
}

// HasKeyedLookup reports whether DiscoverKeyedPath can resolve a transcript
// of provider's family by a session key at all: a family-specific by-id
// lookup, or the generic one SupportsIDLookup allows.
func HasKeyedLookup(provider string) bool {
	_, keyed := keyedFinders[sessionlog.ProviderFamily(provider)]
	return keyed || SupportsIDLookup(provider)
}

// inexactKeyedLookupFamilies are the keyed-lookup families whose lookup can
// return a file of another conversation than the key names: Gemini's returns
// the newest of the project directory's chats that record the key, rather
// than refusing a second one.
var inexactKeyedLookupFamilies = []string{"gemini"}

// HasExactKeyedLookup reports whether a session key finds exactly the
// transcript of the conversation it names for provider's family: a keyed
// lookup (HasKeyedLookup) whose match is that conversation's own file, so a
// caller may attribute what it reads to the session.
func HasExactKeyedLookup(provider string) bool {
	return HasKeyedLookup(provider) && !slices.Contains(inexactKeyedLookupFamilies, sessionlog.ProviderFamily(provider))
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
	if find, ok := keyedFinders[sessionlog.ProviderFamily(provider)]; ok {
		return find(searchPaths, workDir, gcSessionID)
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
// found. The kimi, pi, copilot, kiro and antigravity readers reached through
// DiscoverKeyedPath merge their own default roots on top of searchPaths. The
// claude reader searches searchPaths exactly as given, so a claude caller
// must include its default root: an empty list finds nothing.
func HasKeyedTranscript(searchPaths []string, provider, workDir, sessionKey string) (exists, probeable bool) {
	if strings.TrimSpace(sessionKey) == "" || strings.TrimSpace(workDir) == "" || !ProviderHasKeyedTranscript(provider) {
		return false, false
	}
	return DiscoverKeyedPath(searchPaths, provider, workDir, sessionKey) != "", true
}

// ProviderHasKeyedTranscript reports whether the provider family can use keyed
// transcript absence as a stale-resume signal. This is stricter than
// SupportsIDLookup, which only answers whether transcript display lookup can
// use a provider session id. Here we only claim a provider when its keyed
// transcript absence is meaningful for resume-key invalidation, so the
// stale-resume guard never clears a resume key for a provider whose restart
// semantics we have not verified. It is pure: it reads only the name.
func ProviderHasKeyedTranscript(provider string) bool {
	switch sessionlog.ProviderFamily(provider) {
	case "copilot", "kiro", "kimi", "pi", "antigravity":
		return true
	}
	// claude and claude-eco fall through ProviderFamily unchanged; match them
	// by name since both store keyed JSONL under ~/.claude/projects.
	return strings.Contains(strings.ToLower(strings.TrimSpace(provider)), "claude")
}

// DiscoverScopedPath resolves a transcript for families whose on-disk layout is
// keyed by the session's own identity — name, bead id and conversation epoch —
// rather than by any provider session id gc holds.
//
// Only zcode qualifies today: gc never learns its provider session id, so
// DiscoverKeyedPath cannot hit, but the adapter names its mirror directory
// from GC_SESSION_NAME, GC_SESSION_ID and GC_CONTINUATION_EPOCH — all
// persisted on the session bead. The bead id is what tells two seats sharing
// a session name apart; a mirror written by an adapter that had no bead id is
// still found under the name-only scope. Returns "" for every other family,
// so callers can try it unconditionally.
func DiscoverScopedPath(searchPaths []string, provider, workDir, sessionName, sessionBeadID, continuationEpoch string) string {
	if sessionlog.ProviderFamily(provider) != "zcode" {
		return ""
	}
	return sessionlog.FindZCodeSessionFileByScope(searchPaths, workDir, sessionName, sessionBeadID, continuationEpoch)
}
