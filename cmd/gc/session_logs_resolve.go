package main

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// session_logs_resolve.go holds the session-front-door-injected half of `gc
// session logs`: the resolvers that take the typed *session.Store rather than a
// raw store. The command root (cmd_session_logs.go) opens the city store and
// constructs the front door; these leaves receive it, so this file is store-free
// (frontDoorStoreFreeFiles). Session-bead access reaches the raw session-class
// store the front door wraps via sessFront.Store().Store — the same underlying
// store, so behavior is unchanged.

func resolveStoredSessionLogSource(cityPath string, cfg *config.City, sessFront *sessionpkg.Store, identifier string, searchPaths []string) (string, string, bool, string) {
	logCtx, ok := resolveSessionLogContext(cityPath, cfg, sessFront, identifier)
	if !ok {
		return "", "", false, ""
	}
	if logCtx.sessionID != "" {
		handle, err := workerHandleForSessionWithConfig(cityPath, sessFront.Store().Store, newSessionProvider(), cfg, logCtx.sessionID)
		if err == nil {
			if path, pathErr := handle.TranscriptPath(context.Background()); pathErr == nil && strings.TrimSpace(path) != "" {
				return path, logCtx.provider, true, ""
			}
		}
	}
	path := ""
	fallbackAllowed := canFallbackStoredSessionLogByWorkDir(sessFront, logCtx)
	if strings.TrimSpace(logCtx.sessionKey) != "" {
		path = resolveSessionKeyedLogPath(searchPaths, logCtx)
		if path == "" && fallbackAllowed {
			path = resolveSessionLogPath(searchPaths, logCtx)
		}
	} else if fallbackAllowed {
		path = resolveSessionLogPath(searchPaths, logCtx)
	}
	if !sessionLogPathFreshEnough(path, logCtx.createdAt) {
		path = ""
	}
	if path == "" && fallbackAllowed {
		factory, err := worker.NewFactory(worker.FactoryConfig{SearchPaths: searchPaths})
		if err == nil {
			path = factory.DiscoverWorkDirTranscript(logCtx.provider, logCtx.workDir)
		}
	}
	if !sessionLogPathFreshEnough(path, logCtx.createdAt) {
		path = ""
	}
	// origin/main addition, ported into the front-door structure: when the plain
	// workdir fallback is disallowed (multiple live siblings share the workdir),
	// disambiguate a Codex session by session-start ordering before giving up with
	// the ambiguity diagnostic.
	if path == "" && !fallbackAllowed {
		path = resolveCodexSiblingLogPath(sessFront, searchPaths, logCtx)
	}
	if path == "" && !fallbackAllowed {
		return "", logCtx.provider, true, ambiguousSessionLogDiagnostic(logCtx)
	}
	return path, logCtx.provider, true, ""
}

func resolveSessionLogContext(cityPath string, cfg *config.City, sessFront *sessionpkg.Store, identifier string) (sessionLogContext, bool) {
	if !sessFront.Backed() {
		return sessionLogContext{}, false
	}
	store := sessFront.Store().Store
	sessionID, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, identifier)
	if err != nil {
		return sessionLogContext{}, false
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return sessionLogContext{}, false
	}
	info := sessionpkg.InfoFromPersistedBead(b)
	workDir := strings.TrimSpace(info.WorkDir)
	if workDir == "" {
		return sessionLogContext{}, false
	}
	provider := strings.TrimSpace(info.ProviderKind)
	if provider == "" {
		provider = strings.TrimSpace(info.Provider)
	}
	return sessionLogContext{
		sessionID:  sessionID,
		workDir:    workDir,
		sessionKey: strings.TrimSpace(info.SessionKey),
		provider:   provider,
		createdAt:  info.CreatedAt,
	}, true
}

func canFallbackStoredSessionLogByWorkDir(sessFront *sessionpkg.Store, logCtx sessionLogContext) bool {
	if !sessFront.Backed() || strings.TrimSpace(logCtx.sessionID) == "" || strings.TrimSpace(logCtx.workDir) == "" {
		return false
	}
	siblings, err := sessionLogFallbackSiblings(sessFront, logCtx)
	return err == nil && len(siblings) == 1
}

// sessionLogFallbackSiblings returns the live same-workdir session beads that a
// workdir-based transcript fallback would be ambiguous across. canFallback...
// gates on exactly one; resolveCodexSiblingLogPath uses the full set to order
// Codex transcripts. The filters mirror the pre-split raw-metadata version but
// read through the session.Info codec (class-store leak closure).
func sessionLogFallbackSiblings(sessFront *sessionpkg.Store, logCtx sessionLogContext) ([]beads.Bead, error) {
	all, err := sessionLogFallbackCandidates(sessFront, logCtx.workDir, logCtx.provider)
	if err != nil {
		return nil, err
	}
	targetLive := false
	for _, b := range all {
		if b.ID == logCtx.sessionID {
			targetLive = sessionLogFallbackCandidateLive(sessionpkg.InfoFromPersistedBead(b))
			break
		}
	}
	var matches []beads.Bead
	for _, b := range all {
		info := sessionpkg.InfoFromPersistedBead(b)
		if !sessionpkg.IsSessionBeadOrRepairableInfo(info) {
			continue
		}
		if strings.TrimSpace(info.WorkDir) != logCtx.workDir {
			continue
		}
		provider := strings.TrimSpace(info.ProviderKind)
		if provider == "" {
			provider = strings.TrimSpace(info.Provider)
		}
		if logCtx.provider != "" && provider != "" && provider != logCtx.provider {
			continue
		}
		if targetLive && info.ID != logCtx.sessionID && !sessionLogFallbackCandidateLive(info) {
			continue
		}
		matches = append(matches, b)
	}
	return matches, nil
}

// resolveCodexSiblingLogPath resolves an ambiguous same-workdir Codex session to
// its transcript by session-start ordering. It is used only when the plain
// workdir fallback is disallowed because multiple live siblings share the
// workdir. It returns "" when the sibling set cannot be gathered or the resolved
// transcript is not fresh enough for the session.
func resolveCodexSiblingLogPath(sessFront *sessionpkg.Store, searchPaths []string, logCtx sessionLogContext) string {
	siblings, err := sessionLogFallbackSiblings(sessFront, logCtx)
	if err != nil {
		return ""
	}
	path := sessionpkg.ResolveCodexTranscriptBySessionOrder(searchPaths, logCtx.provider, logCtx.workDir, logCtx.sessionID, siblings)
	if !sessionLogPathFreshEnough(path, logCtx.createdAt) {
		return ""
	}
	return path
}

func sessionLogFallbackCandidates(sessFront *sessionpkg.Store, workDir, provider string) ([]beads.Bead, error) {
	store := sessFront.Store().Store
	candidates := make(map[string]beads.Bead)
	add := func(filters map[string]string) error {
		found, err := store.ListByMetadata(filters, 0)
		if err != nil {
			return err
		}
		for _, b := range found {
			candidates[b.ID] = b
		}
		return nil
	}
	if strings.TrimSpace(provider) == "" {
		if err := add(map[string]string{beadmeta.LegacyWorkDirMetadataKey: workDir}); err != nil {
			return nil, err
		}
	} else {
		if err := add(map[string]string{beadmeta.LegacyWorkDirMetadataKey: workDir, "provider": provider}); err != nil {
			return nil, err
		}
		if err := add(map[string]string{beadmeta.LegacyWorkDirMetadataKey: workDir, "provider_kind": provider}); err != nil {
			return nil, err
		}
	}
	out := make([]beads.Bead, 0, len(candidates))
	for _, b := range candidates {
		out = append(out, b)
	}
	return out, nil
}
