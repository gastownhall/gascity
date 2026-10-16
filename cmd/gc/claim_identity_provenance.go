package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// claimProvenanceVerdict is whether this city can vouch for a claim's
// assignee: local, foreign, or unknown when it cannot answer.
type claimProvenanceVerdict string

// claimProvenanceReason names the test that decided a claimProvenanceVerdict.
type claimProvenanceReason string

const (
	claimVerdictLocal   claimProvenanceVerdict = "local"
	claimVerdictForeign claimProvenanceVerdict = "foreign"
	claimVerdictUnknown claimProvenanceVerdict = "unknown"

	// claimReasonRoster: the identity resolves against this city's config.
	claimReasonRoster claimProvenanceReason = "roster"
	// claimReasonSessionRecord: one of this city's own session records, open
	// or closed, answers to the identity. Detail is the record's ID.
	claimReasonSessionRecord claimProvenanceReason = "session_record"
	// claimReasonForeignBinding: the identity carries a "<binding>." this
	// city does not mint on its rig (ga-8yi7ne). Detail is the binding.
	claimReasonForeignBinding claimProvenanceReason = "foreign_binding"
	// claimReasonAbsentFromRoster: neither the roster nor a local record
	// answers; possibly one of ours whose config and records are both gone.
	claimReasonAbsentFromRoster claimProvenanceReason = "absent_from_roster"
	// claimReasonNoConfig: the resolved config carries no roster to ask.
	claimReasonNoConfig claimProvenanceReason = "no_config"
	// claimReasonLookupFailed: the session-record lookup failed before it
	// could answer. Detail is the error.
	claimReasonLookupFailed claimProvenanceReason = "lookup_failed"
)

// claimProvenance is one verdict of claimIdentityProvenance.
type claimProvenance struct {
	Verdict claimProvenanceVerdict
	Reason  claimProvenanceReason
	// Detail is the matched record ID, the foreign binding, or the error text.
	Detail string
}

// claimIdentityProvenance is the foreign-identity gate for callers that
// release claims without a route filter: it is foreignClaimGate's roster and
// session-record tests applied to every identity shape, with no shape
// exemption. openSessionStore is called only on a roster miss.
//
// WHY NO SHAPE EXEMPTION. poolAssigneeIsLocallyObservable passes every
// identity that is not a well-formed <rig>/<name>, which is tolerable only
// because releaseOrphanedPoolAssignments considers pool-routed work alone.
// The core orphan-sweep order (through `gc agent is-foreign`) walks every
// in-progress bead in every store it can see, and another city on a shared
// rig store mints bare shapes too: runtime session names and session bead IDs
// (ga-x1f77i measured six of them). The exemption existed so local bare claims
// stayed releasable; the record half now covers them instead, since
// session.RecordAnsweringTo also answers by bead ID and session_name. A dead
// local ephemeral claim stays releasable through its own record, with no
// prefix heuristic.
//
// For a well-formed <rig>/<name> identity the verdict is local exactly when
// foreignClaimGate.observable is true: both use poolIdentityInLocalRoster and
// the same three-key record lookup, and unqualifiedAgentNameInLocalRoster never
// fires on "/". The cost and record-lifetime paragraphs on foreignClaimGate
// apply unchanged, plus one Get and one session_name listing for an identity
// without "/". So do its residuals: two cities importing the same pack share an
// identity space (ga-dk24ao), and a record surviving after an identity moves to
// another city keeps vouching for it until the record is purged.
func claimIdentityProvenance(cfg *config.City, cityName, identity string, openSessionStore func() (beads.Store, error)) claimProvenance {
	identity = strings.TrimSpace(identity)
	if cfg == nil || len(cfg.Agents) == 0 {
		return claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonNoConfig}
	}
	if identity != "" && (poolIdentityInLocalRoster(cfg, cityName, identity) || unqualifiedAgentNameInLocalRoster(cfg, identity)) {
		return claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonRoster}
	}
	store, err := openSessionStore()
	if err != nil {
		return claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonLookupFailed, Detail: err.Error()}
	}
	recordID, err := session.RecordAnsweringTo(store, identity)
	if recordID != "" {
		return claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonSessionRecord, Detail: recordID}
	}
	if err != nil {
		return claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonLookupFailed, Detail: err.Error()}
	}
	if !poolCandidateBindingIsLocal(cfg, identity) {
		_, local := config.ParseQualifiedName(identity)
		binding, _, _ := strings.Cut(local, ".")
		return claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonForeignBinding, Detail: binding}
	}
	return claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonAbsentFromRoster}
}

// unqualifiedAgentNameInLocalRoster reports whether a bare identity (no "/"
// and no ".") is a configured agent's Name, or an instance of one
// (poolInstanceBaseNames). findAgentByTemplate resolves bare names only for
// city-scoped agents, so without this a bare short-form claim of a dead
// rig-scoped agent ("backend_dev" of "repo/pack.backend_dev") would read as
// foreign and never be released.
func unqualifiedAgentNameInLocalRoster(cfg *config.City, identity string) bool {
	identity = strings.TrimSpace(identity)
	if cfg == nil || identity == "" || strings.ContainsAny(identity, "/.") {
		return false
	}
	names := append([]string{identity}, poolInstanceBaseNames(identity)...)
	for i := range cfg.Agents {
		for _, name := range names {
			if cfg.Agents[i].Name == name {
				return true
			}
		}
	}
	return false
}
