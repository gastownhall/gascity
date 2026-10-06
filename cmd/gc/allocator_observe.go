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

const (
	// livenessUnknown: nothing proves the runtime alive or absent. The row is
	// uncertain: Keep, and no grant (C2.9, GUAR-053).
	livenessUnknown rowLiveness = iota
	// livenessAlive: the row's runtime name is listed and fresh (Yes, or
	// listed by the latest fresh pass on a backend that has not primed), its
	// pane and process are not known dead, and the runtime is attributed to
	// this row or its name is unique.
	livenessAlive
	// livenessOccupied: the name is listed, alive or dead, but another row
	// owns it.
	livenessOccupied
	// livenessAbsent: listed No by a complete, attested pass.
	livenessAbsent
	// livenessAbsentUnconfirmed: not listed by the latest pass, which
	// finished within maxAge with every backend complete or unattested. A
	// start is allowed, as legacy's bool probe allowed it; the start path is
	// the fence (ErrSessionExists, START-018).
	livenessAbsentUnconfirmed
	// livenessDead: the row's runtime name is listed and fresh, but its pane
	// or process is dead (tmux remain-on-exit keeps an exited pane listed),
	// and the runtime is attributed to this row or its name is unique. A
	// zombie is not alive (BEHAVIORS #7). It is not uncertain: like absent,
	// the row is a start candidate, and the start path recycles the dead pane.
	livenessDead
)

func (l rowLiveness) String() string {
	switch l {
	case livenessAlive:
		return "alive"
	case livenessOccupied:
		return "occupied"
	case livenessAbsent:
		return "absent"
	case livenessAbsentUnconfirmed:
		return "absent-unconfirmed"
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
// a start may proceed: absent, absent-unconfirmed, or dead.
func (l rowLiveness) startCandidate() bool {
	return l == livenessAbsent || l == livenessAbsentUnconfirmed || l == livenessDead
}

// Reasons a row reads uncertain or unknown.
const (
	observeReasonNoPass       = "no-fresh-complete-pass"
	observeReasonOwnerUnknown = "shared-name-owner-unknown"
	observeReasonAttach       = "attach-"
)

// rowObservation is one census row's runtime facts for one pass.
type rowObservation struct {
	Liveness rowLiveness
	Attached bool // only for an alive row
	Pending  bool // a fresh Yes, only for an alive row
	// Uncertain is ObservationUncertain: liveness is unknown, or the row is
	// alive and its attach is stale or unprimed on a backend that reports it.
	Uncertain bool
	Reason    string // why the row is unknown or uncertain
}

// observeCensus reads snap for every canonical census row at now.
func observeCensus(snap *ObservationSnapshot, c *sessionCensus, now time.Time, maxAge time.Duration) map[rowKey]rowObservation {
	listed, provable := inventoryAbsence(snap, now, maxAge)
	out := make(map[rowKey]rowObservation, len(c.canonical))
	for _, k := range c.canonical {
		name := strings.TrimSpace(c.Rows[k].Info.SessionName)
		out[k] = observeRow(snap, k.ID, name, len(c.RowsNamed(name)), listed, provable, now, maxAge)
	}
	return out
}

// inventoryAbsence returns the names the latest pass listed on a backend that
// did not fail, when that pass finished within maxAge (nil otherwise), and
// whether the pass can stand for "not listed means absent, unconfirmed": its
// merged listing did not fail, and every backend listed completely or
// unattested.
func inventoryAbsence(snap *ObservationSnapshot, now time.Time, maxAge time.Duration) (map[string]bool, bool) {
	if snap == nil {
		return nil, false
	}
	pass := snap.Inventory
	if pass.FinishedAt.IsZero() || now.Sub(pass.FinishedAt) > maxAge {
		return nil, false
	}
	provable := !pass.mergedFailed() && len(pass.Backends) > 0
	listed := make(map[string]bool)
	for _, b := range pass.Backends {
		switch b.Outcome {
		case OutcomeComplete, OutcomeUnattested:
		case OutcomeFailed:
			provable = false
			continue
		default:
			provable = false
		}
		for _, name := range b.Names {
			listed[name] = true
		}
	}
	return listed, provable
}

// observeRow reads one row. sharers is how many canonical rows carry name.
func observeRow(snap *ObservationSnapshot, id, name string, sharers int, listed map[string]bool, provable bool, now time.Time, maxAge time.Duration) rowObservation {
	var o rowObservation
	var obs RuntimeObservation
	present := false
	f := snap.Fact(name, FactListed, now, maxAge)
	switch {
	case name == "":
		o.Reason = observeReasonNoPass
	case f.Value == ObsNo:
		o.Liveness = livenessAbsent
	case f.Value == ObsYes:
		obs, _ = snap.Observation(name, now, maxAge)
		present = true
	case listed[name]:
		// Listed by the latest fresh pass on a backend that has not primed
		// (exec, ssh and other unattested backends never do): present.
		obs, present = listedObservation(snap, name, now, maxAge), true
	case provable:
		o.Liveness = livenessAbsentUnconfirmed
	default:
		o.Reason = observeReasonNoPass
		if f.Reason != "" {
			o.Reason = f.Reason
		}
	}
	if present {
		switch {
		case obs.OwnerState == OwnerSession && obs.Owner.SessionID != id:
			o.Liveness = livenessOccupied
		case obs.OwnerState != OwnerSession && sharers > 1:
			o.Reason = observeReasonOwnerUnknown
		case obs.Running.Value == ObsNo || obs.ProcessAlive.Value == ObsNo:
			o.Liveness = livenessDead
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
		obs.Owner, obs.OwnerState = reconcilekey.Key{}, OwnerUnknown
	}
	return obs
}
