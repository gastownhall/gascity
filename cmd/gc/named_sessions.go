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

func applyNamedSessionTemplateParams(tp *TemplateParams, spec namedSessionSpec) {
	if tp == nil {
		return
	}
	identity := strings.TrimSpace(spec.Identity)
	backingTemplate := strings.TrimSpace(namedSessionBackingTemplate(spec))
	if identity == "" || backingTemplate == "" {
		return
	}
	tp.Alias = identity
	tp.TemplateName = backingTemplate
	tp.InstanceName = identity
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_TEMPLATE"] = backingTemplate
	tp.Env["GC_ALIAS"] = identity
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_SESSION_ORIGIN"] = "named"
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
