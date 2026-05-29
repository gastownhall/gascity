package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func providerContinuationIntegrityForSession(session beads.Bead, resolved *config.ResolvedProvider) string {
	if resolved != nil {
		return string(config.NormalizeContinuationIntegrity(resolved.ContinuationIntegrity))
	}
	return sessionpkg.NormalizeStopContinuationIntegrity(session.Metadata[sessionpkg.ProviderContinuationIntegrityMetadataKey])
}

func providerNameForContinuation(session beads.Bead, resolved *config.ResolvedProvider) string {
	if resolved != nil && strings.TrimSpace(resolved.Name) != "" {
		return strings.TrimSpace(resolved.Name)
	}
	return strings.TrimSpace(session.Metadata["provider"])
}

func stopContinuationInputForSession(session beads.Bead, resolved *config.ResolvedProvider, reason string, boundary sessionpkg.StopBoundary) sessionpkg.StopContinuationInput {
	return sessionpkg.StopContinuationInput{
		Provider:  providerNameForContinuation(session, resolved),
		Integrity: providerContinuationIntegrityForSession(session, resolved),
		Reason:    reason,
		Boundary:  boundary,
	}
}

func setStopContinuationMetadata(store beads.Store, session *beads.Bead, now time.Time, input sessionpkg.StopContinuationInput) error {
	if session == nil || store == nil || strings.TrimSpace(session.ID) == "" {
		return nil
	}
	patch := sessionpkg.StopContinuationPatch(now, input)
	if err := store.SetMetadataBatch(session.ID, patch); err != nil {
		return err
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(patch))
	}
	for key, value := range patch {
		session.Metadata[key] = value
	}
	return nil
}

func waitForStopBoundary(ctx context.Context, sp runtime.Provider, sessionName string, timeout time.Duration) sessionpkg.StopBoundary {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" || timeout <= 0 {
		return sessionpkg.StopBoundaryUnknown
	}
	wp, ok := sp.(runtime.IdleWaitProvider)
	if !ok {
		return sessionpkg.StopBoundaryUnknown
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := wp.WaitForIdle(waitCtx, sessionName, timeout); err == nil {
		return sessionpkg.StopBoundaryIdleVerified
	}
	return sessionpkg.StopBoundaryTimeout
}

func providerHistoryInvalidDetected(resolved *config.ResolvedProvider, meta map[string]string, output string) bool {
	if strings.TrimSpace(output) == "" || !providerFatalResumeConfigured(resolved, meta, config.ProviderFatalResumeClaudeThinkingBlockMutation) {
		return false
	}
	lower := strings.ToLower(output)
	return strings.Contains(lower, "cannot be modified") &&
		(strings.Contains(lower, "redacted_thinking") || strings.Contains(lower, "thinking"))
}

func drainAckStopBoundary(session beads.Bead, reconcilerOwned bool) sessionpkg.StopBoundary {
	if !reconcilerOwned {
		return sessionpkg.StopBoundaryAgentAck
	}
	boundary := sessionpkg.StopBoundary(strings.TrimSpace(session.Metadata["last_stop_boundary"]))
	if boundary == sessionpkg.StopBoundaryIdleVerified {
		return boundary
	}
	return sessionpkg.StopBoundaryUnknown
}

func providerFatalResumeConfigured(resolved *config.ResolvedProvider, meta map[string]string, classifier config.ProviderFatalResumeError) bool {
	if resolved != nil {
		for _, value := range resolved.FatalResumeErrors {
			if value == classifier {
				return true
			}
		}
	}
	raw := meta[sessionpkg.ProviderFatalResumeErrorsMetadataKey]
	for _, value := range strings.Split(raw, ",") {
		if strings.TrimSpace(value) == string(classifier) {
			return true
		}
	}
	return false
}
