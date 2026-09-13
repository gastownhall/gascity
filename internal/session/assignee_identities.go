package session

import "strings"

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

// isPoolManagedIdentity reports whether i is a pool-managed worker session:
// the reconciler's own pool_managed / pool_slot / session_origin=="ephemeral"
// markers. It is the session.Info-only subset of cmd/gc's
// isPoolManagedSessionInfo (which additionally resolves a cfg-driven template
// fallback) — AssigneeIdentifier has no config to resolve that fallback
// against, and every pool-managed bead the controller creates stamps one of
// these three markers directly, so the subset is exact for this decision.
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
// Otherwise, an unaliased pool-managed worker claims under its unique session
// bead ID — pool session_name is a chair reused by every occupant of a slot,
// so stamping it as the ownership identity lets a dead occupant's claim look
// held by whoever the controller seats there next. Non-pool sessions keep the
// runtime session name, falling back to the bead ID when no name metadata is
// present.
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
