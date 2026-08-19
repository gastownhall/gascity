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

// namedSessionAssigneeMatchesSpec reports whether assignee names spec's session.
// Work claimed under a named session may carry either the qualified identity
// or the tmux-safe runtime session name (config.NamedSessionRuntimeName:
// "/" -> "--", "." -> "__"). Matching only the qualified form leaves
// on-demand sessions asleep on their own assigned work.
func namedSessionAssigneeMatchesSpec(spec namedSessionSpec, identity, assignee string) bool {
	if assignee == "" {
		return false
	}
	return assignee == identity || assignee == strings.TrimSpace(spec.SessionName)
}

// findNamedSessionSpecForAssignee resolves the configured named session that a
// bead's assignee refers to, accepting every form a real claim can carry —
// qualified identity and runtime session name alike.
func findNamedSessionSpecForAssignee(cfg *config.City, cityName, assignee string) (namedSessionSpec, bool) {
	if cfg == nil || strings.TrimSpace(assignee) == "" {
		return namedSessionSpec{}, false
	}
	if spec, ok := findNamedSessionSpec(cfg, cityName, assignee); ok {
		return spec, true
	}
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if namedSessionAssigneeMatchesSpec(spec, identity, assignee) {
			return spec, true
		}
	}
	return namedSessionSpec{}, false
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
	bead, ok, _ := session.FindClosedNamedSessionBead(store, identity)
	return bead, ok
}

func findClosedNamedSessionBeadForSessionName(store beads.Store, identity, sessionName string) (beads.Bead, bool) {
	bead, ok, _ := session.FindClosedNamedSessionBeadForSessionName(store, identity, sessionName)
	return bead, ok
}

func findNamedSessionConflict(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	return session.FindNamedSessionConflict(sessionBeads.Open(), spec)
}

func findConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (namedSessionSpec, bool, error) {
	return session.FindConflictingNamedSessionSpecForBead(cfg, cityName, b)
}
