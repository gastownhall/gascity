package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The legacy re-home guard sits between the legacy-identity canonicalizers
// (canonicalizeLegacyBoundAssignedWork, canonicalizeLegacyBoundUnassignedRoutedWork)
// and their store writes. Those repairs resolve a persisted template identity
// through permissive migration fallbacks that ask only about the LOCAL config,
// then rewrite the row. On a rig bead store shared by several cities — every
// attached city's census reads every row of it — a foreign city's CANONICAL
// identity resolves the same way: city A's unbound "qcore/omp" and city B's
// binding-qualified "qcore/pool.omp" each read as the other's legacy spelling,
// so A and B rewrite each other's gc.routed_to and assignee back and forth on
// every tick pair (gastownhall/gascity#5846), with no audit trail.
//
// The guard scopes each row by the store leg it was collected under. On the
// city's own legs it changes nothing. On a shared rig leg it refuses every
// rewrite except the one direction a city can prove is its own: stripping a
// binding segment THIS city mints on that rig (cityMintsBindingOnRig, the
// ga-8yi7ne predicate the pool orphan sweeper already runs). The
// reverse "add" rewrite is always refused there, because a bare identity
// carries no mark of which city minted it and the rewrite would also mint the
// binding it adds — no question asked of the local config can prove it.
//
// Convergence by construction: on a shared leg the only allowed rewrite removes
// a binding segment, so a row changes at most once and no two cities' guards
// authorize opposite rewrites.

// legacyRehomeEntry records one guarded re-home decision: the work bead, the
// store leg it was collected under, which field moved, and the identity pair.
type legacyRehomeEntry struct {
	ID    string // work bead ID
	Leg   string // store ref the row was collected under ("" = the city work store)
	Field string // "assignee" or the gc.routed_to metadata key
	From  string // persisted identity the repair would have rewritten
	To    string // canonical identity the repair would have written
}

// legacyRehomeReport is the audit trail of one canonicalizer pass: every
// rewrite it issued (Rehomed) and every rewrite it refused on a foreign leg
// (Refused). Refused rows issue no Update, so they never count as repairs;
// the report is what makes the refusals visible instead of silent.
type legacyRehomeReport struct {
	Rehomed []legacyRehomeEntry
	Refused []legacyRehomeEntry
}

func (r *legacyRehomeReport) rehomed(e legacyRehomeEntry) {
	r.Rehomed = append(r.Rehomed, e)
}

func (r *legacyRehomeReport) refuse(e legacyRehomeEntry) {
	r.Refused = append(r.Refused, e)
}

// legacyRehomeLegIsOwn reports whether ref names a store only this city's
// configuration can have put on the census: the city work store itself (the
// assigned-work arm's empty ref) and the scoped city and relocated-class refs.
// A class binding serves this city's relocated classes, belongs to no rig, and
// no other city attaches it as a rig leg (storeref.ScopeRigContext already
// answers city scope for it). Everything else — a bare rig name, a scoped
// "rig:<name>" leg, and any unrecognized spelling — is treated as a leg other
// cities also read, so unknown refs fail closed.
func legacyRehomeLegIsOwn(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	rig, ok := storeref.ScopeRigContext(ref)
	return ok && rig == ""
}

// legacyRehomeAllowed reports whether cfg may rewrite from to agent's canonical
// identity on the leg the row was collected under. agent is what the template
// resolver returned for from; an exact match never reaches the guard, so the
// agent's binding gives the rewrite direction.
//
// Own legs keep today's behavior. On a shared rig leg the only provable rewrite
// is the STRIP of a binding segment: agent unbound, from shaped
// "<dir>/<binding>.<name>", and this city mints <binding> on <dir>
// (cityMintsBindingOnRig) — the same test the pool orphan sweeper applies, so
// sweeper and re-home agree on every identity. An empty binding, a from that is
// not a bound form, or an unresolvable agent refuses. The ADD direction (a bare
// identity resolving onto a bound agent) is refused on shared legs too: a bare
// identity carries no mark of which city minted the binding it would gain, and
// the rewrite would mint that binding on the row, so no question asked of the
// local config can prove it is this city's legacy spelling.
func legacyRehomeAllowed(cfg *config.City, own bool, from string, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if own {
		return true
	}
	if strings.TrimSpace(agent.BindingName) != "" {
		// Add direction: the resolver matched a BARE persisted identity onto a
		// bound agent. On a shared leg that match is exactly how two cities'
		// vocabularies cross-read (#5846), so refuse.
		return false
	}
	dir, local := config.ParseQualifiedName(strings.TrimSpace(from))
	binding, _, ok := strings.Cut(local, ".")
	if !ok || strings.TrimSpace(binding) == "" {
		return false
	}
	return cityMintsBindingOnRig(cfg, dir, binding)
}

// log writes the report's audit trail to w: one line per rewrite naming the
// bead, the leg, the field, and the quoted identity pair, then at most one
// refusal summary (refusalSummary). An empty report writes nothing, so a quiet
// pass stays quiet. repair names the canonicalizer whose pass produced the
// report.
func (r legacyRehomeReport) log(w io.Writer, repair string) {
	for _, e := range r.Rehomed {
		fmt.Fprintf(w, "%s: rehomed %s on %s: %s %q -> %q\n", repair, e.ID, e.Leg, e.Field, e.From, e.To) //nolint:errcheck // best-effort operator audit line
	}
	if s := r.refusalSummary(repair); s != "" {
		fmt.Fprintln(w, s) //nolint:errcheck // best-effort operator audit line
	}
}

// refusalSummary renders the report's refused re-homes as one line, grouped by
// from->to identity pair and sorted by pair, each with its exact count and up
// to protectedForeignAssigneeIDSampleLimit bead IDs before " +N more". The
// identities are %q-quoted: they are untrusted text read off rows another city
// wrote, and a bare comma or newline inside one would forge summary pairs.
// An empty Refused list renders nothing.
func (r legacyRehomeReport) refusalSummary(repair string) string {
	if len(r.Refused) == 0 {
		return ""
	}
	type pairKey struct{ from, to string }
	ids := map[pairKey][]string{}
	for _, e := range r.Refused {
		k := pairKey{e.From, e.To}
		ids[k] = append(ids[k], e.ID)
	}
	order := make([]pairKey, 0, len(ids))
	for k := range ids {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].from != order[j].from {
			return order[i].from < order[j].from
		}
		return order[i].to < order[j].to
	})
	pairs := make([]string, len(order))
	for i, k := range order {
		list := ids[k]
		sample := list
		suffix := ""
		if len(list) > protectedForeignAssigneeIDSampleLimit {
			sample = list[:protectedForeignAssigneeIDSampleLimit]
			suffix = fmt.Sprintf(" +%d more", len(list)-protectedForeignAssigneeIDSampleLimit)
		}
		pairs[i] = fmt.Sprintf("%q -> %q (%d: %s%s)", k.from, k.to, len(list), strings.Join(sample, ", "), suffix)
	}
	return fmt.Sprintf("%s: refused %d foreign legacy re-home(s) this pass: %s",
		repair, len(r.Refused), strings.Join(pairs, ", "))
}
