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

// configuredNamedSessionSpecForRuntimeName returns the configured named-session
// owner for an exact runtime session name. A unique exact name is durable
// ownership evidence even if a transient desired-state classification omits
// configured-named fields.
func configuredNamedSessionSpecForRuntimeName(cfg *config.City, cityName, sessionName string) (namedSessionSpec, bool) {
	sessionName = strings.TrimSpace(sessionName)
	if cfg == nil || sessionName == "" {
		return namedSessionSpec{}, false
	}

	var matched namedSessionSpec
	for i := range cfg.NamedSessions {
		spec, ok := findNamedSessionSpec(cfg, cityName, cfg.NamedSessions[i].QualifiedName())
		if !ok || strings.TrimSpace(spec.SessionName) != sessionName {
			continue
		}
		if matched.Identity != "" && matched.Identity != spec.Identity {
			return namedSessionSpec{}, false
		}
		matched = spec
	}
	return matched, matched.Identity != ""
}

func namedSessionBackingTemplate(spec namedSessionSpec) string {
	return session.NamedSessionBackingTemplate(spec)
}

// namedSessionAssigneeMatchesSpec reports whether assignee names spec's session.
// Work routed to a named session is claimed under the session's runtime name
// (config.NamedSessionRuntimeName: "/" -> "--", "." -> "__"), not under its
// qualified identity, so both forms have to count. spec.SessionName is already
// an accepted alias for the identity in the resolver
// (session.ResolveNamedSessionSpecForConfigTarget); matching only the qualified
// form here left on-demand named sessions asleep on their own assigned work
// (ga-e70d2).
func namedSessionAssigneeMatchesSpec(spec namedSessionSpec, identity, assignee string) bool {
	if assignee == "" {
		return false
	}
	return assignee == identity || assignee == strings.TrimSpace(spec.SessionName)
}

// findNamedSessionSpecForAssignee resolves the configured named session that a
// bead's assignee refers to, accepting every form a real claim can carry.
//
// findNamedSessionSpec alone is not enough: it matches the qualified identity
// (and the V2 bare leaf), but a named session claims work under its tmux-safe
// runtime name — "seth.seth" claims as "seth__seth". Callers that resolved
// assignees with the identity-only lookup were therefore inert for the one form
// that actually appears on claimed beads (ga-e70d2).
//
// The fallback deliberately reuses namedSessionAssigneeMatchesSpec rather than
// session.ResolveNamedSessionSpecForConfigTarget. That resolver also accepts
// bare template names because it resolves USER input (`gc session wake seth`);
// an assignee is not user input, and widening claim resolution that far would
// preserve routes for names no session ever claimed under. Config load rejects
// identity/session-name collisions city-wide (config.validateNamedSessions), so
// at most one spec can match.
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

// namedSessionModeInfo is the session.Info mirror of namedSessionMode:
// session.NamedSessionModeInfo trims the raw configured_named_mode
// (Info.ConfiguredNamedMode), identical to the bead form.
func namedSessionModeInfo(i session.Info) string {
	return session.NamedSessionModeInfo(i)
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
