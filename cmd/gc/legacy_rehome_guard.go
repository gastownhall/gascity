package main

import (
	"fmt"
	"io"
	"sort"
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

// log writes the report to w under the repair's name: one line per rewritten
// row (each row is rewritten at most once, so this stays bounded) and at most
// one summary of the refused rows, in the format of the orphan sweeper's
// protected-identity summary. An empty report writes nothing.
func (r legacyRehomeReport) log(w io.Writer, repair string) {
	if w == nil {
		return
	}
	for _, row := range r.Rehomed {
		changes := make([]string, 0, len(row.Changes))
		for _, c := range row.Changes {
			changes = append(changes, fmt.Sprintf("%s %q -> %q", c.Field, c.From, c.To))
		}
		fmt.Fprintf(w, "%s: rehomed %s on %s: %s\n", repair, row.ID, demandMigrationLeg(row.Leg), strings.Join(changes, ", ")) //nolint:errcheck
	}
	if summary := r.refusalSummary(); summary != "" {
		fmt.Fprintf(w, "%s: %s\n", repair, summary) //nolint:errcheck
	}
}

// refusalSummary renders the refused rows grouped by rewrite (from -> to),
// sorted so consecutive passes produce comparable lines, with an exact row
// count and a bounded sample of IDs per rewrite; "" when nothing was refused.
// Identities are %q-quoted: they are untrusted text read off rows another city
// may have written, and an unquoted comma or newline would forge structure.
// A refused row stays as it is until an operator re-homes it by hand
// (gc bd update <id> --assignee <to> --set-metadata gc.routed_to=<to>).
func (r legacyRehomeReport) refusalSummary() string {
	type rewrite struct{ from, to string }
	byRewrite := map[rewrite][]string{}
	for _, row := range r.Refused {
		for _, c := range row.Changes {
			key := rewrite{c.From, c.To}
			if ids := byRewrite[key]; len(ids) == 0 || ids[len(ids)-1] != row.ID {
				byRewrite[key] = append(ids, row.ID)
			}
		}
	}
	if len(byRewrite) == 0 {
		return ""
	}
	keys := make([]rewrite, 0, len(byRewrite))
	for key := range byRewrite {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		return keys[i].to < keys[j].to
	})
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		ids := byRewrite[key]
		sample, suffix := ids, ""
		if len(sample) > protectedForeignAssigneeIDSampleLimit {
			suffix = fmt.Sprintf(" +%d more", len(sample)-protectedForeignAssigneeIDSampleLimit)
			sample = sample[:protectedForeignAssigneeIDSampleLimit]
		}
		parts = append(parts, fmt.Sprintf("%q -> %q (%d: %s%s)", key.from, key.to, len(ids), strings.Join(sample, ", "), suffix))
	}
	return fmt.Sprintf("refused %d legacy re-home(s) on shared rig legs this pass (%d rows): %s",
		len(keys), len(r.Refused), strings.Join(parts, ", "))
}
