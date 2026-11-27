package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The legacy re-home repairs (canonicalizeLegacyBoundAssignedWork,
// canonicalizeLegacyBoundUnassignedRoutedWork) rewrite persisted identities
// that the permissive legacy resolver reads as old spellings of a local agent.
// That resolver consults only this city's config, and a rig bead store may be
// attached by several cities: city A's unbound "qcore/omp" reads city B's
// canonical "qcore/pool.omp" as a legacy bound form, and B's bound
// "qcore/pool.omp" reads A's canonical "qcore/omp" as a legacy unbound form.
// Unguarded, the two cities rewrite each other's rows every tick (#5846).
//
// The guard below is scoped by leg. On a leg only this city reads (the city
// work store, a relocated class binding) every legacy rewrite is still made.
// On a shared rig leg the only rewrite allowed is the one that strips a binding
// this city mints on that rig: cityMintsBindingOnRig, the predicate the orphan
// sweeper already trusts for the same question. Adding a binding to a bare
// identity is refused there, because a bare "dir/name" carries no mark of the
// city that wrote it and the city adding the binding is the one minting it, so
// no local fact proves the row is ours. Since the only rewrite allowed on a
// shared leg removes a binding segment, a row changes at most once there and
// no two cities' guards authorize opposite rewrites.

// legacyRehomeFieldAssignee names the Assignee column in a legacyRehomeChange;
// the route is named by its metadata key, beadmeta.RoutedToMetadataKey.
const legacyRehomeFieldAssignee = "assignee"

// legacyRehomeChange is one identity rewrite on one field of a work row.
type legacyRehomeChange struct {
	Field string
	From  string
	To    string
}

// legacyRehomeRow is one work row a re-home pass rewrote or refused, on the
// leg (store ref) it was collected from.
type legacyRehomeRow struct {
	ID      string
	Leg     string
	Changes []legacyRehomeChange
}

// legacyRehomeReport is what one canonicalizeLegacyBound* pass did: the rows
// it rewrote and the rows the shared-leg guard refused.
type legacyRehomeReport struct {
	Rehomed []legacyRehomeRow
	Refused []legacyRehomeRow
}

// legacyRehomeChangeFor resolves a persisted identity through the permissive
// legacy resolver (findAgentByTemplate) and reports the rewrite to the
// resolved agent's canonical identity together with that agent, or a nil agent
// when there is nothing to rewrite: the identity is empty, resolves to no
// agent, or is already canonical.
func legacyRehomeChangeFor(cfg *config.City, field, from string) (legacyRehomeChange, *config.Agent) {
	from = strings.TrimSpace(from)
	if from == "" {
		return legacyRehomeChange{}, nil
	}
	agent := findAgentByTemplate(cfg, from)
	if agent == nil {
		return legacyRehomeChange{}, nil
	}
	to := agent.QualifiedName()
	if to == "" || to == from {
		return legacyRehomeChange{}, nil
	}
	return legacyRehomeChange{Field: field, From: from, To: to}, agent
}

// legacyRehomeLegIsOwn reports whether a work leg is read by this city alone:
// the city work store under either census vocabulary ("" or "city:<name>") or
// a relocated class binding ("class:<token>"), which serves only this city's
// classes and is never attached as another city's rig. Every other ref, a rig
// leg ("<rig>", "rig:<rig>") or a spelling this does not recognize, is treated
// as shared, so an unknown ref fails closed.
func legacyRehomeLegIsOwn(ref string) bool {
	return ref == "" || strings.HasPrefix(ref, "city:") || storeref.IsClassRef(ref)
}

// legacyRehomeAllowed reports whether a legacy rewrite of from onto agent (the
// agent the permissive resolver returned, whose canonical identity differs
// from from) may be written on a leg. An own leg allows every rewrite. On a
// shared leg the agent's binding gives the direction:
//
//   - strip (agent unbound, from "dir/<b>.name"): allowed only when this city
//     mints binding b on rig dir, b cut at the first "." exactly as
//     legacyBoundTemplateMatchesUnboundAgent cuts it; an empty b is refused.
//   - add (agent bound, from a bare identity): refused, since a bare identity
//     cannot be proven to be this city's.
func legacyRehomeAllowed(cfg *config.City, own bool, from string, agent *config.Agent) bool {
	if own {
		return true
	}
	if agent == nil || strings.TrimSpace(agent.BindingName) != "" {
		return false
	}
	rig, local := config.ParseQualifiedName(strings.TrimSpace(from))
	binding, _, ok := strings.Cut(local, ".")
	if !ok {
		return false
	}
	return cityMintsBindingOnRig(cfg, rig, binding)
}
