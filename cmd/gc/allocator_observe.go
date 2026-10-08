package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// The allocator's reading of the observation cache (I3) per census row, under
// amendment AM3 (P3 spec §4.4 step 10). The inventory lane publishes listing,
// liveness and incarnation for every backend, and attach only for backends
// with a batched inventory; it never publishes pending or activity (F1). So:
//
//   - an unsupported fact is No, never a reason to keep a session;
//   - pending is an input only as a fresh Yes a session key wrote through
//     Note, and only for a row whose runtime is alive; the session key's fresh
//     check before a drain is the fence;
//   - only unknown liveness, and a stale or unprimed attach on a backend that
//     reports attach, make a row uncertain.
//
// Facts are attributed by bead ID: a runtime name that several open rows
// share (enterprise slot-scoped names, F8) is alive for its owner row only,
// and occupied for the others.

// rowLiveness is a census row's runtime liveness as the allocator reads it.
type rowLiveness uint8

// The five classes of v5 O1. Gone and dead propose; every destructive effect
// re-proves (F3).
const (
	// livenessUnknown: nothing proves the runtime alive or gone, or the name
	// is listed and its identity is unread, failed or ownerless (v5 O1). The
	// row is uncertain: Keep, and no grant (C2.9, GUAR-053).
	livenessUnknown rowLiveness = iota
	// livenessAlive: the row's runtime name is listed and fresh (Yes, or
	// listed by the latest fresh pass on a backend that has not primed), its
	// pane and process are not known dead, and the runtime is attributed to
	// this row or its name is unique.
	livenessAlive
	// livenessOccupied: the name is listed, alive or dead, but another row
	// owns it.
	livenessOccupied
	// livenessGone: the latest pass finished within maxAge, every backend
	// listed completely and attested or confirmed its server dead, and the
	// name was not listed. Whether the name was ever seen does not matter.
	livenessGone
	// livenessDead: the row's runtime name is listed and fresh, but its pane
	// is dead (a corpse: tmux remain-on-exit keeps an exited pane listed) or
	// the process probe found its agent dead (a zombie), and the runtime is
	// attributed to this row or its name is unique. A zombie is not alive
	// (BEHAVIORS #7). It is not uncertain: like gone, the row is a start
	// candidate, and the start path recycles the dead pane.
	livenessDead
)

func (l rowLiveness) String() string {
	switch l {
	case livenessAlive:
		return "alive"
	case livenessOccupied:
		return "occupied"
	case livenessGone:
		return "gone"
	case livenessDead:
		return "dead"
	default:
		return "unknown"
	}
}

// alive reports whether the row's own runtime is running; dependencies count
// only these.
func (l rowLiveness) alive() bool { return l == livenessAlive }

// startCandidate reports whether nothing running holds the row's runtime, so
// a start may proceed: gone or dead.
func (l rowLiveness) startCandidate() bool {
	return l == livenessGone || l == livenessDead
}

// Reasons a row reads uncertain or unknown.
const (
	observeReasonNoPass       = "no-fresh-complete-pass"
	observeReasonOwnerUnknown = "shared-name-owner-unknown"
	observeReasonIdentity     = "identity-unread"
	observeReasonOwnerless    = "identity-ownerless"
	observeReasonAttach       = "attach-"
)

// rowObservation is one census row's runtime facts for one pass.
type rowObservation struct {
	Liveness rowLiveness
	Attached bool // only for an alive row
	Pending  bool // a fresh Yes, only for an alive row
	Corpse   bool // a dead row whose pane is dead, not only its agent
	// Uncertain is ObservationUncertain: liveness is unknown, or the row is
	// alive and its attach is stale or unprimed on a backend that reports it.
	Uncertain bool
	Reason    string // why the row is unknown or uncertain
}

// observeCensus reads snap for every canonical census row at now.
func observeCensus(snap *ObservationSnapshot, c *sessionCensus, now time.Time, maxAge time.Duration) map[rowKey]rowObservation {
	listed, complete := inventoryAbsence(snap, now, maxAge)
	out := make(map[rowKey]rowObservation, len(c.canonical))
	for _, k := range c.canonical {
		name := strings.TrimSpace(c.Rows[k].Info.SessionName)
		out[k] = observeRow(snap, k.ID, name, len(c.RowsNamed(name)), listed, complete, now, maxAge)
	}
	return out
}

// inventoryAbsence returns the names the latest pass listed on a backend that
// did not fail, when that pass finished within maxAge (nil otherwise), and
// whether the pass proves an unlisted name gone: its merged listing did not
// fail, and every backend listed completely and attested, or confirmed its
// server dead (v5 O1). An unattested backend proves nothing.
func inventoryAbsence(snap *ObservationSnapshot, now time.Time, maxAge time.Duration) (map[string]bool, bool) {
	if snap == nil {
		return nil, false
	}
	pass := snap.Inventory
	if pass.FinishedAt.IsZero() || now.Sub(pass.FinishedAt) > maxAge {
		return nil, false
	}
	complete := !pass.mergedFailed() && len(pass.Backends) > 0
	listed := make(map[string]bool)
	for _, b := range pass.Backends {
		if b.Outcome == OutcomeFailed {
			complete = false
			continue
		}
		complete = complete && (b.Outcome == OutcomeComplete || b.ConfirmedDead)
		for _, name := range b.Names {
			listed[name] = true
		}
	}
	return listed, complete
}

// observeRow reads one row. sharers is how many canonical rows carry name.
// A listed name is classified on its identity first: no arm acts on a runtime
// whose identity is unread, failed or ownerless (v5 O1). The owner test is
// otherwise today's, over the identity read, until C4c2 rewires it through
// compareIdentity.
func observeRow(snap *ObservationSnapshot, id, name string, sharers int, listed map[string]bool, complete bool, now time.Time, maxAge time.Duration) rowObservation {
	var o rowObservation
	var obs RuntimeObservation
	present := false
	f := snap.Fact(name, FactListed, now, maxAge)
	switch {
	case name == "":
		o.Reason = observeReasonNoPass
	case listed[name] && f.Value == ObsYes:
		obs, present = snap.Observation(name, now, maxAge)
	case listed[name]:
		// Listed by the latest fresh pass on a backend that has not primed
		// (exec, ssh and other unattested backends never do): present.
		obs, present = listedObservation(snap, name, now, maxAge), true
	case listedSincePass(snap, name, now, maxAge):
		obs, present = snap.Observation(name, now, maxAge)
	case complete:
		o.Liveness = livenessGone
	case f.Value == ObsYes:
		// Listed by an earlier fresh pass; the latest was partial there.
		obs, present = snap.Observation(name, now, maxAge)
	default:
		o.Reason = observeReasonNoPass
		if f.Reason != "" {
			o.Reason = f.Reason
		}
	}
	if present {
		ident := obs.Identity
		switch {
		case !ident.Known:
			o.Reason = observeReasonIdentity
		case ident.SessionID == "" && ident.Token == "":
			o.Reason = observeReasonOwnerless
		case ident.SessionID != "" && ident.SessionID != id:
			o.Liveness = livenessOccupied
		case ident.SessionID == "" && sharers > 1:
			o.Reason = observeReasonOwnerUnknown
		case obs.Running.Value == ObsNo || obs.ProcessAlive.Value == ObsNo:
			o.Liveness, o.Corpse = livenessDead, obs.Running.Value == ObsNo
		default:
			o.Liveness = livenessAlive
		}
	}
	if o.Liveness == livenessAlive {
		switch a := snap.Fact(name, FactAttached, now, maxAge); {
		case a.Value == ObsYes:
			o.Attached = true
		case a.Value == ObsNo || a.Reason == obsReasonUnsupported:
		default:
			reason := a.Reason
			if reason == "" {
				reason = "unknown"
			}
			o.Uncertain, o.Reason = true, observeReasonAttach+reason
		}
		// Legacy probes pending only on live targets (compute_awake_bridge.go).
		o.Pending = snap.Fact(name, FactPending, now, maxAge).Value == ObsYes
	}
	o.Uncertain = o.Uncertain || o.Liveness == livenessUnknown
	return o
}

// listedSincePass reports whether a fresh probe wrote name's Listed=Yes
// after the latest pass started, so that pass's absence predates it. It reads
// the raw fact: a probe can note a name no backend has listed yet.
func listedSincePass(snap *ObservationSnapshot, name string, now time.Time, maxAge time.Duration) bool {
	if snap == nil {
		return false
	}
	f := snap.ByName[name].Listed
	return f.Value == ObsYes && f.ObservedAt.After(snap.Inventory.StartedAt) && now.Sub(f.ObservedAt) <= maxAge
}

// listedObservation is Observation without the priming rule, for a name the
// latest fresh pass listed on a backend that has not primed: that pass's facts
// are current even though the backend cannot attest absence. Stale facts read
// unknown, and the owner only counts when the listing pass enriched the name.
func listedObservation(snap *ObservationSnapshot, name string, now time.Time, maxAge time.Duration) RuntimeObservation {
	obs := snap.ByName[name]
	for _, kind := range allFactKinds {
		if f := obs.fact(kind); now.Sub(f.ObservedAt) > maxAge {
			*f = RuntimeFact{ObservedAt: f.ObservedAt, Source: f.Source, Reason: obsReasonStale}
		}
	}
	if obs.Listed.Value != ObsYes || obs.EnrichedAt.IsZero() || !obs.EnrichedAt.Equal(obs.Listed.ObservedAt) {
		obs.Owner, obs.OwnerState, obs.Identity = reconcilekey.Key{}, OwnerUnknown, runtimeIdentity{}
	}
	return obs
}
