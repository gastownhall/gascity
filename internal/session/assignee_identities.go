package session

import (
	"errors"
	"fmt"
	"slices"
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

// recordLookupKeys are the session-bead metadata keys HasRecordAnsweringTo
// looks an identity up under: the AssigneeIdentities sources that can hold a
// qualified "<rig>/<name>" identity. A bead ID never contains "/", and the
// session_name syntax (sessionNamePattern) admits none either.
var recordLookupKeys = []string{"alias", NamedSessionIdentityMetadata, aliasHistoryMetadataKey}

// anyShapeRecordLookupKeys extends recordLookupKeys with session_name: the
// AssigneeIdentities source that cannot hold "/" but holds every runtime name a
// session claims under.
var anyShapeRecordLookupKeys = append([]string{"session_name"}, recordLookupKeys...)

// HasRecordAnsweringTo reports whether a session bead in store, open or closed,
// answers to identity under its alias, its configured named identity, or its
// alias history. Each key is one exact-metadata listing, and the lookup writes
// nothing: an empty-type session bead counts without being repaired. What a
// listing costs is the backend's. On SQLite (an indexed metadata lookup) and in
// memory it follows the records that match, not the size of the session
// ledger, but bd/Dolt filters metadata without an index, so there each listing
// scans the whole store (about 0.6s measured on an 11.3k-bead store).
//
// alias_history is stored comma-joined, so the exact match finds a prior alias
// only when it is the record's sole history entry. False therefore means no
// record was found, not that none exists.
//
// A hard listing error ends the lookup and returns false with the error: the
// answer is unknown. The rows a partial result returns still count, so true is
// reliable even when the first partial-result error comes back with it. The
// listings are Live reads, so a CachingStore returns a backing failure as the
// hard error it is, not as a partial result over its cached open rows.
func HasRecordAnsweringTo(store beads.Store, identity string) (bool, error) {
	id, err := recordAnsweringTo(store, strings.TrimSpace(identity), recordLookupKeys)
	return id != "", err
}

// RecordAnsweringTo returns the ID of a session bead in store, open or closed,
// that answers to identity under any AssigneeIdentities source, or "" when none
// was found. It is HasRecordAnsweringTo for callers that meet claims of every
// shape (the core orphan-sweep order through `gc agent is-foreign`): for an
// identity containing "/" it issues exactly HasRecordAnsweringTo's lookups,
// since a bead ID and a session_name never contain "/". For any other identity
// it first reads the bead whose ID is the identity (an unaliased pool or
// ephemeral session claims under its bead ID, AssigneeIdentifier), then looks
// the identity up under session_name, alias, configured named identity and
// alias history. Error semantics match HasRecordAnsweringTo; a non-NotFound
// Get error is a hard error.
func RecordAnsweringTo(store beads.Store, identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if store == nil || identity == "" {
		return "", nil
	}
	if strings.Contains(identity, "/") {
		return recordAnsweringTo(store, identity, recordLookupKeys)
	}
	if couldBeBeadID(identity) {
		b, err := store.Get(identity)
		switch {
		case err == nil:
			if IsSessionBeadOrRepairable(b) && slices.Contains(AssigneeIdentities(infoFromPersistedBead(b)), identity) {
				return b.ID, nil
			}
		case errors.Is(err, beads.ErrNotFound):
		default:
			return "", fmt.Errorf("reading session bead %q: %w", identity, err)
		}
	}
	return recordAnsweringTo(store, identity, anyShapeRecordLookupKeys)
}

// couldBeBeadID reports whether identity is worth a Get by ID: bead IDs carry
// no whitespace, and a leading "-" would read as a flag to a CLI-backed store.
func couldBeBeadID(identity string) bool {
	return !strings.HasPrefix(identity, "-") && !strings.ContainsAny(identity, " \t\r\n")
}

// recordAnsweringTo is the listing loop behind HasRecordAnsweringTo and
// RecordAnsweringTo: one Live, closed-inclusive exact-metadata listing per key,
// returning the first session record whose own identities include identity.
func recordAnsweringTo(store beads.Store, identity string, keys []string) (string, error) {
	if store == nil || identity == "" {
		return "", nil
	}
	var partial error
	for _, key := range keys {
		rows, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{key: identity},
			IncludeClosed: true,
			Live:          true,
		})
		if err != nil {
			err = fmt.Errorf("listing session beads by %s: %w", key, err)
			if !beads.IsPartialResult(err) {
				return "", err
			}
			if partial == nil {
				partial = err
			}
		}
		for _, b := range rows {
			if IsSessionBeadOrRepairable(b) && slices.Contains(AssigneeIdentities(infoFromPersistedBead(b)), identity) {
				return b.ID, partial
			}
		}
	}
	return "", partial
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
