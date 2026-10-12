package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the confined session-class assignee-identity vocabulary: the
// forms under which a work bead may be assigned to a session. It is shared by
// the reconciler orphan-release loops (which enumerate every form a live
// session answers to) and the API assignee list filter and assign stamper
// (which enumerate the same set and pick the durable stamp form). Confining it
// here keeps the session-bead metadata keys (session_name / alias /
// configured_named_identity / alias_history) out of cmd/gc and internal/api, so
// those callers speak session identities via session.Info instead of cracking
// beads.Bead.Metadata directly.
//
// All reads use the RAW Info mirrors (SessionNameMetadata, not SessionName)
// because Info.SessionName falls back to sessionNameFor(ID); admitting that
// derived runtime name into the assignee set would match work the session was
// never assigned.

// AssigneeIdentities returns every identifier under which a work bead could be
// assigned to this session: the session bead ID, session_name,
// configured_named_identity, current alias, and any prior aliases preserved in
// alias_history — each trimmed, empty values skipped, in that order. Pool
// polecat aliases (e.g. "nux") are first-class assignment identities, so
// leaving them out of orphan-detection resets in-progress work under a live
// owner — see the SkipsLiveSessionAssignedByAlias regression tests.
func AssigneeIdentities(i Info) []string {
	identities := make([]string, 0, 5)
	if id := strings.TrimSpace(i.ID); id != "" {
		identities = append(identities, id)
	}
	if sn := strings.TrimSpace(i.SessionNameMetadata); sn != "" {
		identities = append(identities, sn)
	}
	if ni := strings.TrimSpace(i.ConfiguredNamedIdentity); ni != "" {
		identities = append(identities, ni)
	}
	if al := strings.TrimSpace(i.Alias); al != "" {
		identities = append(identities, al)
	}
	for _, prior := range i.AliasHistory {
		if prior = strings.TrimSpace(prior); prior != "" {
			identities = append(identities, prior)
		}
	}
	return identities
}

// isPoolManagedIdentity reports whether i carries one of the reconciler's
// pool_managed / pool_slot / session_origin=="ephemeral" markers. It is the
// session.Info-only subset of cmd/gc's isPoolManagedSessionInfo (which
// additionally resolves a cfg-driven template fallback); AssigneeIdentifier
// has no config to resolve that fallback against. Every pool-managed bead the
// controller creates stamps one of these markers directly, so all pool
// workers are covered. The check is intentionally broader than "pool": a
// non-pool session stamped session_origin=ephemeral also matches, which is
// correct because gc hook --claim records any unaliased session's claims
// under its session bead ID. Unaliased manual sessions still fall through to
// session_name.
func isPoolManagedIdentity(i Info) bool {
	if strings.TrimSpace(i.SessionOrigin) == "ephemeral" {
		return true
	}
	if i.PoolManaged {
		return true
	}
	return strings.TrimSpace(i.PoolSlot) != ""
}

// AssigneeIdentifier returns the durable agent-facing ownership identity of a
// session: its current public alias or configured named identity always win.
// Otherwise, an unaliased pool-managed or ephemeral session claims under its
// session bead ID. The bead ID is the stable per-session identity that the
// claim (gc hook --claim records it), the bd actor (BEADS_ACTOR) and the stored
// assignee all share, independent of how the runtime is named: a runtime
// session_name is a provider-facing name whose shape has changed across
// releases (slot-derived chair names on rc builds, <template>-<beadID> again
// since #6549) and may be identity-derived for tmux_alias pools. Other
// sessions keep the runtime session name, falling back to the bead ID when no
// name metadata is present.
// This is the same alias-first identity RuntimeEnvWithSessionContext exposes
// through GC_ALIAS and BEADS_ACTOR; GC_AGENT mirrors it only for compatibility.
// Keeping API assignment normalization on this rule prevents one session from
// owning work under a different exact string than it presents to bd.
func AssigneeIdentifier(i Info) string {
	for _, v := range []string{i.Alias, i.ConfiguredNamedIdentity} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if isPoolManagedIdentity(i) {
		if id := strings.TrimSpace(i.ID); id != "" {
			return id
		}
	}
	if sn := strings.TrimSpace(i.SessionNameMetadata); sn != "" {
		return sn
	}
	return strings.TrimSpace(i.ID)
}

// IsOpenSessionOfTemplate reports whether b is a live (not closed) session bead,
// or a repairable one, created from template. Callers that meet a work bead's
// assignee and need to know whether it is one of a pool's own sessions use it
// after resolving the assignee as a bead ID: since #6324 an unaliased pool or
// ephemeral session claims under its session bead ID (AssigneeIdentifier), so
// the "<template>-" session_name prefix no longer identifies the claimant.
func IsOpenSessionOfTemplate(b beads.Bead, template string) bool {
	template = strings.TrimSpace(template)
	if template == "" || !IsSessionBeadOrRepairable(b) {
		return false
	}
	info := infoFromPersistedBead(b)
	return !info.Closed && strings.TrimSpace(info.Template) == template
}
