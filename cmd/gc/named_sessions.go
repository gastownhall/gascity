package main

import (
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

// isNamedSessionInfo is the session.Info mirror of isNamedSessionBead:
// session.IsNamedSessionBead reads the trimmed configured_named_session flag,
// which Info.ConfiguredNamedSession already projects identically.
func isNamedSessionInfo(i session.Info) bool {
	return i.ConfiguredNamedSession
}

func namedSessionIdentity(b beads.Bead) string {
	return session.NamedSessionIdentity(b)
}

// namedSessionIdentityInfo is the session.Info mirror of namedSessionIdentity:
// session.NamedSessionIdentityInfo reads the trimmed configured_named_identity,
// which Info.ConfiguredNamedIdentity carries verbatim.
func namedSessionIdentityInfo(i session.Info) string {
	return session.NamedSessionIdentityInfo(i)
}

func configuredNamedSessionBeadHasSpec(b beads.Bead, cfg *config.City, cityName string) bool {
	if cfg == nil || !isNamedSessionBead(b) {
		return false
	}
	identity := namedSessionIdentity(b)
	if identity == "" {
		return false
	}
	_, ok := findNamedSessionSpec(cfg, cityName, identity)
	return ok
}

// configuredNamedSessionBeadHasSpecInfo is the session.Info mirror of
// configuredNamedSessionBeadHasSpec: isNamedSessionInfo and namedSessionIdentityInfo
// are the equivalence-proven siblings, and findNamedSessionSpec keys off the
// projected identity string identically.
func configuredNamedSessionBeadHasSpecInfo(i session.Info, cfg *config.City, cityName string) bool {
	if cfg == nil || !isNamedSessionInfo(i) {
		return false
	}
	identity := namedSessionIdentityInfo(i)
	if identity == "" {
		return false
	}
	_, ok := findNamedSessionSpec(cfg, cityName, identity)
	return ok
}

func namedSessionMode(b beads.Bead) string {
	return session.NamedSessionMode(b)
}

func namedSessionContinuityEligible(b beads.Bead) bool {
	return session.NamedSessionContinuityEligible(b)
}

func findCanonicalNamedSessionInfo(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (session.Info, bool) {
	if sessionBeads == nil {
		return session.Info{}, false
	}
	return session.FindCanonicalNamedSessionInfo(sessionBeads.OpenInfos(), spec)
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

func findNamedSessionConflictInfo(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (session.Info, bool) {
	if sessionBeads == nil {
		return session.Info{}, false
	}
	return session.FindNamedSessionConflictInfo(sessionBeads.OpenInfos(), spec)
}

func findConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (namedSessionSpec, bool, error) {
	return session.FindConflictingNamedSessionSpecForBead(cfg, cityName, b)
}
