package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	namedSessionMetadataKey      = session.NamedSessionMetadataKey
	namedSessionIdentityMetadata = session.NamedSessionIdentityMetadata
	namedSessionModeMetadata     = session.NamedSessionModeMetadata
)

type namedSessionSpec = session.NamedSessionSpec

func normalizeNamedSessionTarget(target string) string {
	return session.NormalizeNamedSessionTarget(target)
}

func targetBasename(target string) string {
	return session.TargetBasename(target)
}

func findNamedSessionSpec(cfg *config.City, cityName, identity string) (namedSessionSpec, bool) {
	return session.FindNamedSessionSpec(cfg, cityName, identity)
}

func namedSessionBackingTemplate(spec namedSessionSpec) string {
	return session.NamedSessionBackingTemplate(spec)
}

func resolveNamedSessionSpecForConfigTarget(cfg *config.City, cityName, target, rigContext string) (namedSessionSpec, bool, error) {
	return session.ResolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
}

func findNamedSessionSpecForTarget(cfg *config.City, cityName, target string) (namedSessionSpec, bool, error) {
	return session.FindNamedSessionSpecForTarget(cfg, cityName, target, currentRigContext(cfg))
}

func isNamedSessionBead(b beads.Bead) bool {
	return session.IsNamedSessionBead(b)
}

func namedSessionIdentity(b beads.Bead) string {
	return session.NamedSessionIdentity(b)
}

func namedSessionMode(b beads.Bead) string {
	return session.NamedSessionMode(b)
}

func namedSessionContinuityEligible(b beads.Bead) bool {
	return session.NamedSessionContinuityEligible(b)
}

func findCanonicalNamedSessionBead(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	return session.FindCanonicalNamedSessionBead(sessionBeads.Open(), spec)
}

// findClosedNamedSessionBead searches for a closed bead that was previously
// the canonical bead for the given named session identity. Uses a targeted
// metadata query (Store.ListByMetadata) so only matching beads are returned
// — no bulk scan of all closed beads.
func findClosedNamedSessionBead(store beads.Store, identity string) (beads.Bead, bool) {
	return findClosedNamedSessionBeadForSessionName(store, identity, "")
}

func findClosedNamedSessionBeadForSessionName(store beads.Store, identity, sessionName string) (beads.Bead, bool) {
	identity = normalizeNamedSessionTarget(identity)
	sessionName = strings.TrimSpace(sessionName)
	candidates, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			namedSessionIdentityMetadata: identity,
		},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false
	}
	var fallback beads.Bead
	hasFallback := false
	for _, b := range candidates {
		if b.Status != "closed" {
			continue
		}
		if sessionName != "" {
			if strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
				return b, true
			}
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) != "" {
			return b, true
		}
		if !hasFallback {
			fallback = b
			hasFallback = true
		}
	}
	if hasFallback {
		return fallback, true
	}
	return beads.Bead{}, false
}

func beadConflictsWithNamedSession(b beads.Bead, spec namedSessionSpec) bool {
	return session.BeadConflictsWithNamedSession(b, spec)
}

func findNamedSessionConflict(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	for _, b := range sessionBeads.Open() {
		if beadConflictsWithNamedSession(b, spec) {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func findConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (namedSessionSpec, bool, error) {
	return session.FindConflictingNamedSessionSpecForBead(cfg, cityName, b)
}

func sessionAliasHistoryContains(metadata map[string]string, target string) bool {
	for _, alias := range session.AliasHistory(metadata) {
		if alias == target {
			return true
		}
	}
	return false
}
